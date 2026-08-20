// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 4.2: the durable per-conversation "story so far"
// record — the durable replacement for Neo's ephemeral a.summary
// (agent.go:562).
//
// # What this is (req.7.1)
//
// StorySoFar is a DERIVED, journaled-but-NOT-anchored record — a single
// story/<conv> record per conversation (keys.StoryKey), in the SAME lane as
// cortex.Compact (compact.go:429-431), the session store (session.go), and
// the temporal ladder (rollup.go). BuildStorySoFar NEVER calls
// snap.StageMemoryUpdate / StageEdgeUpdate, so it perturbs neither the
// anchored "memories"/"edges" SMT roots nor the D11 replay byte-identity of
// the canonical world-state.
//
// # "Maintained by the ladder" — deterministic extractive floor
//
// Exactly like BuildRollup's deterministic floor (rollup.go), StorySoFar is a
// PURE function of the conversation's own sess/<conv>/* transcript
// (session.go) up to the highest seq present when it is built: the same
// transcript content always yields byte-identical ShortForm + record bytes,
// regardless of wall-clock time. Role tallies render with alphabetically
// sorted keys (reusing rollup.go's renderTally) and the last-user /
// last-assistant excerpts are a deterministic fixed-length slice of the
// transcript content — no LLM in this path (mirrors the two-lane Q3
// decision: the extractive floor is the replayable summarizer; an optional
// LLM enrichment, if ever added, would ride a separate rebuildable record,
// exactly like rollup_enrich.go does for rollups).
//
// # Lazy read-repair (mirrors repair.go's RepairRollup exactly)
//
// BuildStorySoFar is idempotent: rebuilding at the same UpToSeq (no new
// messages appended since) yields a byte-identical record. Activate
// (activate.go) reads the story via LoadStorySoFar and — when the stored
// record is missing or STALE (its UpToSeq is behind the conversation's
// current transcript head) — performs an idempotent rebuild inline, the
// exact "missing or stale coarser tier" repair discipline task 3.1
// established for the rollup ladder (repair.go). There is no separate
// eager-sweep scheduler for the story record (no task/requirement asks for
// one); lazy repair alone keeps it materialized and current on read.
package cortex

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"

	"centra/core/cortex/journal"
	"centra/core/cortex/keys"
	"centra/core/cortex/memory"
)

// StorySoFarSchemaVersion is stamped on every emitted StorySoFarPayload and
// StorySoFarRecord. Bumping requires a journal-kind migration.
const StorySoFarSchemaVersion uint8 = 1

// StorySoFarExcerptChars caps the length of the last-user / last-assistant
// excerpts folded into the deterministic ShortForm, keeping the record a
// bounded page-in target rather than growing unbounded with a long message.
// Truncation is a fixed prefix slice (deterministic, not summarized).
const StorySoFarExcerptChars = 500

// StorySoFarRecord is the canonical CBOR-encoded blob persisted at
// story/<conv>. It is a pure extractive summary of the conversation's own
// transcript up to UpToSeq: role tallies, message count, and a deterministic
// short-form (req.7.1's "durable rolling summary of THIS conversation").
type StorySoFarRecord struct {
	SchemaVersion  uint8             `cbor:"0,keyasint"`
	ConversationID string            `cbor:"1,keyasint"`
	UpToSeq        uint64            `cbor:"2,keyasint"`
	MessageCount   uint32            `cbor:"3,keyasint"`
	RoleTally      map[string]uint32 `cbor:"4,keyasint"`
	ShortForm      string            `cbor:"5,keyasint"`
}

// Canonical CBOR encoder for StorySoFarRecord. Mirrors rollup.go's
// rollEnc/rollDec init (rollup.go:156-167): CoreDetEncOptions produces
// RFC 8949 §4.2.1 deterministic encoding — required because the encoded
// bytes are integrity-hashed into StorySoFarPayload.RecordHash and because
// map[string]uint32 tallies must encode byte-stably (CoreDetEnc sorts map
// keys).
var (
	storyEnc cbor.EncMode
	storyDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("cortex/story: build EncMode: %w", err))
	}
	storyEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("cortex/story: build DecMode: %w", err))
	}
	storyDec = dm
}

// EncodeStorySoFarRecord returns canonical deterministic CBOR for r.
func EncodeStorySoFarRecord(r *StorySoFarRecord) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("cortex/story: nil StorySoFarRecord")
	}
	return storyEnc.Marshal(r)
}

// DecodeStorySoFarRecord parses canonical CBOR into out.
func DecodeStorySoFarRecord(b []byte, out *StorySoFarRecord) error {
	return storyDec.Unmarshal(b, out)
}

// BuildStorySoFarURI returns the agent-facing canonical URI for a
// story-so-far record:
//
//	matrix://cortex/story/<conv>
func BuildStorySoFarURI(conv string) memory.URI {
	return memory.URI(fmt.Sprintf("matrix://cortex/story/%s", conv))
}

// BuildStorySoFar reads the conversation's own transcript (session.go's
// Transcript) and (re)persists the deterministic extractive StorySoFarRecord
// in the derived lane (story/<conv> record + a KindStorySoFar journal entry,
// NO SMT write) — the SAME posture as BuildRollup.
//
// It is IDEMPOTENT: rebuilding when the transcript's current head seq has
// not advanced since the last build yields a byte-identical record (no new
// journal entry — the compute-compare-skip discipline cascade.go's
// buildCoarseRollup established for coarse rollups). Returns ("", nil) when
// the conversation has no transcript yet (nothing to summarize).
func (c *Cortex) BuildStorySoFar(conv string) (memory.URI, error) {
	if conv == "" {
		return "", ErrEmptyConversationID
	}

	msgs, err := c.Transcript(conv, 0, 0)
	if err != nil {
		return "", fmt.Errorf("cortex.BuildStorySoFar: transcript: %w", err)
	}
	if len(msgs) == 0 {
		return "", nil
	}
	upToSeq := msgs[len(msgs)-1].Seq

	roleTally := map[string]uint32{}
	var lastUser, lastAssistant string
	for _, m := range msgs {
		roleTally[m.Role.String()]++
		switch m.Role {
		case RoleUser:
			lastUser = m.Content
		case RoleAssistant:
			lastAssistant = m.Content
		}
	}

	shortForm := buildStorySoFarShortForm(conv, upToSeq, uint32(len(msgs)), roleTally, lastUser, lastAssistant)

	record := &StorySoFarRecord{
		SchemaVersion:  StorySoFarSchemaVersion,
		ConversationID: conv,
		UpToSeq:        upToSeq,
		MessageCount:   uint32(len(msgs)),
		RoleTally:      roleTally,
		ShortForm:      shortForm,
	}
	encodedRec, err := EncodeStorySoFarRecord(record)
	if err != nil {
		return "", fmt.Errorf("cortex.BuildStorySoFar: encode record: %w", err)
	}

	// Idempotence: skip the write (and journal append) when an identical
	// record already exists — mirrors cascade.go's buildCoarseRollup
	// compute-compare-skip discipline.
	if existing, lerr := c.LoadStorySoFar(conv); lerr == nil {
		if prevBytes, eerr := EncodeStorySoFarRecord(existing); eerr == nil && bytes.Equal(prevBytes, encodedRec) {
			return BuildStorySoFarURI(conv), nil
		}
	} else if !errors.Is(lerr, memory.ErrNotFound) {
		return "", fmt.Errorf("cortex.BuildStorySoFar: load existing: %w", lerr)
	}

	recordHash := sha256.Sum256(encodedRec)
	sp := &journal.StorySoFarPayload{
		SchemaVersion:  StorySoFarSchemaVersion,
		ConversationID: conv,
		UpToSeq:        upToSeq,
		RecordHash:     recordHash,
	}
	spBytes, err := journal.EncodeStorySoFarPayload(sp)
	if err != nil {
		return "", fmt.Errorf("cortex.BuildStorySoFar: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindStorySoFar,
		CreatedAt: c.now().UnixNano(),
		Payload:   spBytes,
	}

	storyKey, err := keys.StoryKey(conv)
	if err != nil {
		return "", fmt.Errorf("cortex.BuildStorySoFar: %w", err)
	}

	// Atomic derived-lane batch (compact.go:426-442 posture): story/<conv>
	// record + journal entry. NO SMT update — the story-so-far is derived
	// working state, not canonical world-state.
	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(storyKey, encodedRec); err != nil {
		return "", fmt.Errorf("cortex.BuildStorySoFar: set story: %w", err)
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", fmt.Errorf("cortex.BuildStorySoFar: append journal: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.BuildStorySoFar: commit: %w", err)
	}

	return BuildStorySoFarURI(conv), nil
}

// buildStorySoFarShortForm renders the deterministic extractive summary.
// Role tallies render with alphabetically sorted keys (renderTally,
// rollup.go) and the last-user/last-assistant excerpts are a fixed-prefix
// slice, so the output is byte-stable for the same transcript content
// (mirrors rollup.go's buildRollupShortForm determinism discipline).
func buildStorySoFarShortForm(conv string, upToSeq uint64, count uint32, roleTally map[string]uint32, lastUser, lastAssistant string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[story %s upToSeq=%d] %d messages; roles: %s",
		conv, upToSeq, count, renderTally(roleTally))
	if lastUser != "" {
		b.WriteString("; last_user: ")
		b.WriteString(truncateDeterministic(lastUser, StorySoFarExcerptChars))
	}
	if lastAssistant != "" {
		b.WriteString("; last_assistant: ")
		b.WriteString(truncateDeterministic(lastAssistant, StorySoFarExcerptChars))
	}
	return b.String()
}

// truncateDeterministic returns s capped at n runes, appending "..." when
// truncated. Rune-safe (never splits a multi-byte UTF-8 sequence).
func truncateDeterministic(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// LoadStorySoFar returns the persisted StorySoFarRecord stored at
// story/<conv>. Returns memory.ErrNotFound when no record exists.
func (c *Cortex) LoadStorySoFar(conv string) (*StorySoFarRecord, error) {
	if conv == "" {
		return nil, ErrEmptyConversationID
	}
	k, err := keys.StoryKey(conv)
	if err != nil {
		return nil, fmt.Errorf("cortex.LoadStorySoFar: %w", err)
	}
	raw, ok, err := c.s.Get(k)
	if err != nil {
		return nil, fmt.Errorf("cortex.LoadStorySoFar: get: %w", err)
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var rec StorySoFarRecord
	if err := DecodeStorySoFarRecord(raw, &rec); err != nil {
		return nil, fmt.Errorf("cortex.LoadStorySoFar: decode: %w", err)
	}
	return &rec, nil
}
