// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 1.1: durable, provider-agnostic conversation
// transcript store.
//
// cortex owns the live turn-by-turn transcript. AppendMessage records one
// message durably, scoped to actor (the per-actor Pebble DB) + conversation,
// with a per-conversation monotonic gap-free seq and a timestamp. Transcript
// reads an ordered slice back.
//
// LANE DISCIPLINE. This rides the SAME derived, journaled-but-NOT-anchored
// lane as cortex.Compact (compact.go:429-431): each AppendMessage writes a
// journal entry (journal.KindSession) + a derived sess/<conv>/<seq> store
// record, and performs NO snap.StageMemoryUpdate / StageEdgeUpdate. The
// anchored namespace SMT roots ("memories", "edges") are therefore
// byte-identical before and after any AppendMessage — cortex manages its own
// working transcript without perturbing OverallRoot's anchored world-state or
// the D11 replay byte-identity of the canonical memory graph. (OverallRoot
// itself does move because the journal MMR grows a leaf — that is expected and
// is the derived-lane posture, identical to KindCompact.)
//
// PROVIDER-AGNOSTIC. The Message / SessionRecord schema is neutral: {Role,
// Content, ToolName, ToolArgs (canonical JSON bytes), ToolResultRef,
// MediaRefs[], TS, Seq, ConversationID}. It has no coupling to any provider
// wire shape (Fireworks / OpenAI / Baseten). Tool calls and tool results are
// representable losslessly (ToolName + ToolArgs for a call; Content or a
// spilled ToolResultRef for a result), so any provider transcript can be
// projected onto it and read back without loss.
//
// OVERFLOW DISCIPLINE. Large tool_result payloads spill to a separate
// resolvable blob (sessblob/<conv>/<seq>): the stored record keeps Content
// empty and carries a ToolResultRef URI, so the sess/ record stays small — a
// page-in target, never resident bloat. ResolveToolResult reads the spilled
// bytes back exactly.
//
// CALLER CONTRACT — NO SECRETS. This store SHALL NOT be used to persist
// secrets, tokens, API keys, or .env values. Message content is durable and
// integrity-hashed into the journal chain; treat everything written here as
// permanently retained transcript. Callers MUST redact credentials before
// calling AppendMessage. cortex deliberately never logs message content
// (Content / ToolArgs / spilled tool results are never written to any log
// line by this code).

package cortex

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
)

// SessionSchemaVersion is stamped on every emitted SessionRecord and
// SessionPayload. Bumping requires a journal-kind migration.
const SessionSchemaVersion uint8 = 1

// SessionToolResultSpillBytes is the threshold above which a RoleToolResult
// message's Content is spilled to a resolvable blob (sessblob/<conv>/<seq>)
// instead of being stored inline in the sess/ record. Keeps the transcript
// record a compact page-in target.
const SessionToolResultSpillBytes = 8192

// Role is the provider-agnostic message role. Kept as a small enum so it can
// be denormalized into the journal SessionPayload (a uint8) for audit scans
// without decoding the heavy record.
type Role uint8

const (
	// RoleUser is a message authored by the human user.
	RoleUser Role = 0
	// RoleAssistant is a message authored by the assistant/model.
	RoleAssistant Role = 1
	// RoleToolCall is the assistant's request to invoke a tool (ToolName +
	// ToolArgs carry the call; Content may hold any accompanying rationale).
	RoleToolCall Role = 2
	// RoleToolResult is the result returned from a tool invocation (Content
	// holds the result, or ToolResultRef when it was spilled).
	RoleToolResult Role = 3
	// RoleSystem is a system/instruction message.
	RoleSystem Role = 4
)

// String returns the canonical lowercase role name.
func (r Role) String() string {
	switch r {
	case RoleUser:
		return "user"
	case RoleAssistant:
		return "assistant"
	case RoleToolCall:
		return "tool_call"
	case RoleToolResult:
		return "tool_result"
	case RoleSystem:
		return "system"
	default:
		return fmt.Sprintf("role(%d)", uint8(r))
	}
}

// validRole reports whether r is one of the five defined roles.
func validRole(r Role) bool { return r <= RoleSystem }

// Message is the provider-agnostic transcript message. It is both the input
// to AppendMessage and the element returned by Transcript.
//
// On input: Seq is ignored (AppendMessage allocates it); TS, if zero, is
// stamped with c.now(). On return: Seq is the allocated per-conversation seq
// and ConversationID / TS are populated.
type Message struct {
	// ConversationID scopes the message. Required. Must not contain '/'
	// and must be <=255 bytes (key-component bounds).
	ConversationID string
	// Seq is the per-conversation monotonic gap-free sequence number.
	// Ignored on input; set by AppendMessage and returned via Transcript.
	Seq uint64
	// Role is the message role. Required (must be one of the five roles).
	Role Role
	// Content is the message body. For a spilled tool_result this is empty
	// and ToolResultRef is set instead.
	Content string
	// ToolName is the invoked tool's name (RoleToolCall / RoleToolResult).
	ToolName string
	// ToolArgs is the tool-call arguments as canonical JSON bytes. Stored
	// verbatim; cortex does not parse or reshape it (provider-agnostic).
	ToolArgs []byte
	// ToolResultRef is set when a large RoleToolResult Content was spilled;
	// resolvable via ResolveToolResult. Empty otherwise.
	ToolResultRef string
	// MediaRefs are opaque resolvable references to attached media (images,
	// audio, etc.). Stored verbatim.
	MediaRefs []string
	// TS is the message timestamp in Unix nanoseconds. Zero on input →
	// stamped with c.now().
	TS int64
}

// SessionRecord is the canonical CBOR-encoded blob persisted at
// sess/<conv>/<seq>. Carries every Message field. RecordHash in the journal
// SessionPayload is sha256 over this record's canonical CBOR encoding.
//
// When a tool_result Content is spilled, the record stores Content="" and
// ToolResultRef=<uri> — so RecordHash pins the record-with-ref, not the
// spilled bytes (the bytes live at sessblob/<conv>/<seq>).
type SessionRecord struct {
	SchemaVersion  uint8    `cbor:"0,keyasint"`
	ConversationID string   `cbor:"1,keyasint"`
	Seq            uint64   `cbor:"2,keyasint"`
	Role           uint8    `cbor:"3,keyasint"`
	Content        string   `cbor:"4,keyasint"`
	ToolName       string   `cbor:"5,keyasint"`
	ToolArgs       []byte   `cbor:"6,keyasint"`
	ToolResultRef  string   `cbor:"7,keyasint"`
	MediaRefs      []string `cbor:"8,keyasint"`
	TS             int64    `cbor:"9,keyasint"`
}

// Errors raised by the session surface.
var (
	// ErrEmptyConversationID: a conversation id is required.
	ErrEmptyConversationID = errors.New("cortex.session: conversation_id is empty")
	// ErrInvalidRole: role is not one of the five defined roles.
	ErrInvalidRole = errors.New("cortex.session: invalid role")
)

// Canonical CBOR encoder for SessionRecord. Mirrors compact.go init()
// (compact.go:188-199): CoreDetEncOptions produces RFC 8949 §4.2.1
// deterministic encoding, required because the encoded bytes are
// integrity-hashed into SessionPayload.RecordHash.
var (
	sessEnc cbor.EncMode
	sessDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("cortex/session: build EncMode: %w", err))
	}
	sessEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("cortex/session: build DecMode: %w", err))
	}
	sessDec = dm
}

// EncodeSessionRecord returns canonical deterministic CBOR for r.
func EncodeSessionRecord(r *SessionRecord) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("cortex/session: nil SessionRecord")
	}
	return sessEnc.Marshal(r)
}

// DecodeSessionRecord parses canonical CBOR into out.
func DecodeSessionRecord(b []byte, out *SessionRecord) error {
	return sessDec.Unmarshal(b, out)
}

// BuildSessionURI returns the agent-facing canonical URI for a session
// message record:
//
//	matrix://cortex/session/<conv>/<seq>
func BuildSessionURI(conv string, seq uint64) memory.URI {
	return memory.URI(fmt.Sprintf("matrix://cortex/session/%s/%d", conv, seq))
}

// BuildToolResultURI returns the resolvable URI for a spilled tool_result
// payload:
//
//	matrix://cortex/session/<conv>/<seq>/toolresult
func BuildToolResultURI(conv string, seq uint64) string {
	return fmt.Sprintf("matrix://cortex/session/%s/%d/toolresult", conv, seq)
}

// AppendMessage records one message durably and returns its canonical URI.
//
// Derived lane (mirrors compact.go:429-431 exactly): a journal.KindSession
// entry + a derived sess/<conv>/<seq> record (+ a sessblob/<conv>/<seq> spill
// when a large tool_result is stored), all in one atomic batch. NO SMT write
// — the anchored "memories"/"edges" roots are untouched.
//
// Seq allocation is per-conversation, monotonic, and gap-free: BeginWrite
// holds the per-store seqMu until Commit, so reading meta/session_head/<conv>
// (default 0 when absent), using that value as this message's seq, and writing
// seq+1 back are serialized against concurrent appends to any conversation.
//
// See the package doc comment for the NO-SECRETS caller contract.
func (c *Cortex) AppendMessage(m Message) (memory.URI, error) {
	if m.ConversationID == "" {
		return "", ErrEmptyConversationID
	}
	if !validRole(m.Role) {
		return "", ErrInvalidRole
	}
	// Validate conv against key-component bounds up front (also surfaces
	// the '/' / >255 constraints before we hold the write lock).
	headKey, err := keys.MetaSessionHead(m.ConversationID)
	if err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: %w", err)
	}

	if m.TS == 0 {
		m.TS = c.now().UnixNano()
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()

	// --- allocate the gap-free per-conversation seq (seqMu held) -----
	seq := uint64(0)
	raw, ok, err := c.s.Get(headKey)
	if err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: read session head: %w", err)
	}
	if ok {
		seq, _, err = keys.ReadUint64BE(raw)
		if err != nil {
			return "", fmt.Errorf("cortex.AppendMessage: decode session head: %w", err)
		}
	}

	sessKey, err := keys.SessionKey(m.ConversationID, seq)
	if err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: %w", err)
	}

	// --- build the record, spilling a large tool_result payload ------
	rec := &SessionRecord{
		SchemaVersion:  SessionSchemaVersion,
		ConversationID: m.ConversationID,
		Seq:            seq,
		Role:           uint8(m.Role),
		Content:        m.Content,
		ToolName:       m.ToolName,
		ToolArgs:       m.ToolArgs,
		ToolResultRef:  m.ToolResultRef,
		MediaRefs:      m.MediaRefs,
		TS:             m.TS,
	}

	var spilled []byte
	if m.Role == RoleToolResult && len(m.Content) > SessionToolResultSpillBytes {
		spilled = []byte(m.Content)
		rec.Content = ""
		rec.ToolResultRef = BuildToolResultURI(m.ConversationID, seq)
	}

	encodedRec, err := EncodeSessionRecord(rec)
	if err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: encode record: %w", err)
	}
	recordHash := sha256.Sum256(encodedRec)

	// --- journal payload carries only identity + integrity pin -------
	sp := &journal.SessionPayload{
		SchemaVersion:  SessionSchemaVersion,
		ConversationID: m.ConversationID,
		Seq:            seq,
		Role:           uint8(m.Role),
		RecordHash:     recordHash,
	}
	spBytes, err := journal.EncodeSessionPayload(sp)
	if err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindSession,
		CreatedAt: m.TS,
		Payload:   spBytes,
	}

	// --- atomic derived-lane batch (compact.go:426-442 posture) ------
	// sess/<conv>/<seq> record + (optional) sessblob spill + advance the
	// canonical per-conversation head counter + journal entry. NO SMT
	// update: session transcript is derived working state, not anchored
	// world-state (consistent with compact.go:429-431).
	if err := wb.Set(sessKey, encodedRec); err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: set sess: %w", err)
	}
	if spilled != nil {
		blobKey, berr := keys.SessionBlobKey(m.ConversationID, seq)
		if berr != nil {
			return "", fmt.Errorf("cortex.AppendMessage: %w", berr)
		}
		if err := wb.Set(blobKey, spilled); err != nil {
			return "", fmt.Errorf("cortex.AppendMessage: set sessblob: %w", err)
		}
	}
	var nextHead [8]byte
	nb := keys.PutUint64BE(nextHead[:0], seq+1)
	if err := wb.Set(headKey, nb); err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: set session head: %w", err)
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: append journal: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.AppendMessage: commit: %w", err)
	}

	return BuildSessionURI(m.ConversationID, seq), nil
}

// Transcript returns the messages of conv with Seq >= sinceSeq, in ascending
// seq order, bounded by limit. A limit <= 0 means unbounded.
//
// Records are returned as-is: a spilled tool_result has empty Content and a
// non-empty ToolResultRef (resolve it via ResolveToolResult). The scan is a
// single ascending prefix iteration over sess/<conv> — the key layout
// guarantees seq order.
func (c *Cortex) Transcript(conv string, sinceSeq uint64, limit int) ([]Message, error) {
	if conv == "" {
		return nil, ErrEmptyConversationID
	}
	prefix, err := keys.SessionConvPrefix(conv)
	if err != nil {
		return nil, fmt.Errorf("cortex.Transcript: %w", err)
	}

	out := make([]Message, 0, 16)
	stop := errors.New("transcript-limit-reached")
	iterErr := c.s.PrefixIter(prefix, func(k, v []byte) error {
		_, seq, perr := keys.ParseSessionKey(k)
		if perr != nil {
			return fmt.Errorf("parse session key: %w", perr)
		}
		if seq < sinceSeq {
			return nil
		}
		var rec SessionRecord
		if derr := DecodeSessionRecord(v, &rec); derr != nil {
			return fmt.Errorf("decode session record seq=%d: %w", seq, derr)
		}
		out = append(out, Message{
			ConversationID: rec.ConversationID,
			Seq:            rec.Seq,
			Role:           Role(rec.Role),
			Content:        rec.Content,
			ToolName:       rec.ToolName,
			ToolArgs:       rec.ToolArgs,
			ToolResultRef:  rec.ToolResultRef,
			MediaRefs:      rec.MediaRefs,
			TS:             rec.TS,
		})
		if limit > 0 && len(out) >= limit {
			return stop
		}
		return nil
	})
	if iterErr != nil && !errors.Is(iterErr, stop) {
		return nil, fmt.Errorf("cortex.Transcript: %w", iterErr)
	}
	return out, nil
}

// ResolveToolResult returns the spilled tool_result payload bytes stored at
// sessblob/<conv>/<seq>. Returns memory.ErrNotFound when no spill exists for
// that (conv, seq) — i.e. the tool_result was small enough to stay inline (or
// the message is not a spilled tool_result). The returned bytes are exactly
// the original Content bytes that were spilled.
func (c *Cortex) ResolveToolResult(conv string, seq uint64) ([]byte, error) {
	if conv == "" {
		return nil, ErrEmptyConversationID
	}
	blobKey, err := keys.SessionBlobKey(conv, seq)
	if err != nil {
		return nil, fmt.Errorf("cortex.ResolveToolResult: %w", err)
	}
	raw, ok, err := c.s.Get(blobKey)
	if err != nil {
		return nil, fmt.Errorf("cortex.ResolveToolResult: get: %w", err)
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	return raw, nil
}
