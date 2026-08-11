// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
)

const (
	ToolMatchMatched    = "matched"
	ToolMatchMismatched = "mismatched"
	ToolMatchUnknown    = "unknown"
)

type ToolEvent struct {
	CallID         string
	ToolName       string
	Arguments      json.RawMessage
	Result         json.RawMessage
	Error          string
	Expect         string
	MatchVerdict   string
	SubgoalID      string
	IdempotencyKey string
	CreatedBy      string
}

type ToolEventCitation struct {
	Seq         uint64   `json:"seq"`
	LeafCount   uint64   `json:"leaf_count"`
	LeafHash    [32]byte `json:"leaf_hash"`
	JournalRoot [32]byte `json:"journal_root"`
}

var (
	ErrToolCitationInvalid  = errors.New("cortex: invalid tool-event citation")
	ErrToolCitationMismatch = errors.New("cortex: tool-event citation mismatch")
	ErrToolEventConflict    = errors.New("cortex: tool-event idempotency conflict")
)

// RecordToolEvent commits one execution-evidence leaf into the journal MMR
// and returns the citation that pins it: the leaf's seq, the leaf count at
// commit, the canonical plaintext leaf hash, and the journal root over
// exactly that prefix.
//
// The root is read with MMR.RootAt(leafCount) rather than MMR.Root(). The
// store's seqMu is released by Commit, so a concurrent journal writer (the
// async embedder, a session append, a sibling tool execution) can land a
// leaf before this call reads the accumulator; Root() would then return a
// root over more leaves than the citation claims, minting a citation that
// can never verify. RootAt pins the prefix this call actually committed.
//
// When IdempotencyKey is set, a replay of the same execution resolves
// through the tev/idem/ index to the existing leaf and appends nothing.
// A different execution under the same key is ErrToolEventConflict.
func (c *Cortex) RecordToolEvent(
	event ToolEvent,
) (ToolEventCitation, error) {
	if c == nil || c.s == nil || c.snap == nil {
		return ToolEventCitation{}, fmt.Errorf(
			"cortex.RecordToolEvent: cortex unavailable",
		)
	}
	event.CallID = strings.TrimSpace(event.CallID)
	event.ToolName = strings.TrimSpace(event.ToolName)
	event.Expect = strings.TrimSpace(event.Expect)
	event.SubgoalID = strings.TrimSpace(event.SubgoalID)
	event.MatchVerdict = strings.TrimSpace(event.MatchVerdict)
	event.IdempotencyKey = strings.TrimSpace(event.IdempotencyKey)
	if event.CallID == "" || event.ToolName == "" ||
		len(event.Arguments) == 0 || !json.Valid(event.Arguments) ||
		event.Expect == "" || event.SubgoalID == "" ||
		!validToolMatch(event.MatchVerdict) {
		return ToolEventCitation{}, fmt.Errorf(
			"cortex.RecordToolEvent: invalid execution evidence",
		)
	}
	payload, err := journal.EncodeToolEventPayload(
		&journal.ToolEventPayload{
			SchemaVersion:  1,
			CallID:         event.CallID,
			ToolName:       event.ToolName,
			Arguments:      append([]byte(nil), event.Arguments...),
			ResultDigest:   sha256.Sum256(event.Result),
			Error:          strings.TrimSpace(event.Error),
			Expect:         event.Expect,
			MatchVerdict:   event.MatchVerdict,
			SubgoalID:      event.SubgoalID,
			IdempotencyKey: event.IdempotencyKey,
		},
	)
	if err != nil {
		return ToolEventCitation{}, err
	}
	if event.IdempotencyKey != "" {
		existing, existingPayload, found, err :=
			c.findToolEventByIdempotency(event.IdempotencyKey)
		if err != nil {
			return ToolEventCitation{}, err
		}
		if found {
			if !bytes.Equal(existingPayload, payload) {
				return ToolEventCitation{}, ErrToolEventConflict
			}
			return existing, nil
		}
	}
	entry := &journal.Entry{
		Kind:      journal.KindToolEvent,
		CreatedAt: c.now().UTC().UnixNano(),
		CreatedBy: []byte(strings.TrimSpace(event.CreatedBy)),
		Payload:   payload,
	}
	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.AppendJournal(entry); err != nil {
		return ToolEventCitation{}, fmt.Errorf(
			"cortex.RecordToolEvent: append journal: %w", err,
		)
	}
	citation := ToolEventCitation{
		Seq: wb.Seq(), LeafCount: wb.Seq() + 1,
		LeafHash: wb.LeafHash(),
	}
	if event.IdempotencyKey != "" {
		var seqBuf [8]byte
		binary.BigEndian.PutUint64(seqBuf[:], citation.Seq)
		if err := wb.Set(
			keys.ToolEventIdemKey(event.IdempotencyKey), seqBuf[:],
		); err != nil {
			return ToolEventCitation{}, fmt.Errorf(
				"cortex.RecordToolEvent: index idempotency key: %w", err,
			)
		}
	}
	if err := wb.Commit(); err != nil {
		return ToolEventCitation{}, fmt.Errorf(
			"cortex.RecordToolEvent: commit: %w", err,
		)
	}
	citation.JournalRoot, err = c.snap.MMR().RootAt(citation.LeafCount)
	if err != nil {
		return ToolEventCitation{}, fmt.Errorf(
			"cortex.RecordToolEvent: journal root: %w", err,
		)
	}
	return citation, nil
}

// findToolEventByIdempotency resolves key through the tev/idem/ index and
// rebuilds the citation for the leaf it points at. Returns found=false when
// the key has never been recorded.
func (c *Cortex) findToolEventByIdempotency(
	key string,
) (ToolEventCitation, []byte, bool, error) {
	raw, ok, err := c.s.Get(keys.ToolEventIdemKey(key))
	if err != nil {
		return ToolEventCitation{}, nil, false, fmt.Errorf(
			"cortex.RecordToolEvent: read idempotency index: %w", err,
		)
	}
	if !ok {
		return ToolEventCitation{}, nil, false, nil
	}
	if len(raw) != 8 {
		return ToolEventCitation{}, nil, false, fmt.Errorf(
			"cortex.RecordToolEvent: malformed idempotency index (len=%d)",
			len(raw),
		)
	}
	seq := binary.BigEndian.Uint64(raw)
	entry, leaf, err := c.journalEntryAt(seq)
	if err != nil {
		return ToolEventCitation{}, nil, false, err
	}
	if entry.Kind != journal.KindToolEvent {
		return ToolEventCitation{}, nil, false, fmt.Errorf(
			"cortex.RecordToolEvent: idempotency index points at %q at seq %d",
			entry.Kind, seq,
		)
	}
	root, err := c.snap.MMR().RootAt(seq + 1)
	if err != nil {
		return ToolEventCitation{}, nil, false, err
	}
	return ToolEventCitation{
		Seq: seq, LeafCount: seq + 1, LeafHash: leaf, JournalRoot: root,
	}, entry.Payload, true, nil
}

// VerifyToolEventCitation proves a citation against the journal MMR and
// returns the evidence it pins.
//
// Three independent checks, all O(log n) or better: the entry at Seq must
// hash to the cited leaf (the citation names a real, unaltered entry), the
// persisted accumulator's root over LeafCount leaves must equal the cited
// root (the root was not forged, and still holds after later appends), and
// the pinned entry must decode as a well-formed tool event.
func (c *Cortex) VerifyToolEventCitation(
	citation ToolEventCitation,
) (journal.ToolEventPayload, error) {
	if c == nil || c.s == nil || c.snap == nil ||
		citation.LeafCount == 0 || citation.LeafCount != citation.Seq+1 {
		return journal.ToolEventPayload{}, ErrToolCitationInvalid
	}
	entry, leaf, err := c.journalEntryAt(citation.Seq)
	if err != nil {
		return journal.ToolEventPayload{}, err
	}
	if leaf != citation.LeafHash || entry.Kind != journal.KindToolEvent {
		return journal.ToolEventPayload{}, ErrToolCitationMismatch
	}
	root, err := c.snap.MMR().RootAt(citation.LeafCount)
	if err != nil {
		return journal.ToolEventPayload{}, fmt.Errorf(
			"%w: journal root: %v", ErrToolCitationMismatch, err,
		)
	}
	if root != citation.JournalRoot {
		return journal.ToolEventPayload{}, ErrToolCitationMismatch
	}
	var payload journal.ToolEventPayload
	if err := journal.DecodeToolEventPayload(
		entry.Payload, &payload,
	); err != nil {
		return journal.ToolEventPayload{}, fmt.Errorf(
			"%w: decode payload: %v", ErrToolCitationInvalid, err,
		)
	}
	if payload.SchemaVersion != 1 ||
		payload.CallID == "" || payload.ToolName == "" ||
		!json.Valid(payload.Arguments) || payload.Expect == "" ||
		payload.SubgoalID == "" || !validToolMatch(payload.MatchVerdict) {
		return journal.ToolEventPayload{}, ErrToolCitationInvalid
	}
	return payload, nil
}

// journalEntryAt reads j/<seq> through the vault seam and returns the
// decoded entry alongside the leaf hash over its canonical plaintext
// encoding — the same bytes the MMR hook committed.
func (c *Cortex) journalEntryAt(
	seq uint64,
) (journal.Entry, [32]byte, error) {
	raw, ok, err := c.s.Get(keys.JournalKey(seq))
	if err != nil {
		return journal.Entry{}, [32]byte{}, fmt.Errorf(
			"cortex: read journal entry %d: %w", seq, err,
		)
	}
	if !ok {
		return journal.Entry{}, [32]byte{}, fmt.Errorf(
			"%w: no journal entry at seq %d", ErrToolCitationMismatch, seq,
		)
	}
	var entry journal.Entry
	if err := journal.Decode(raw, &entry); err != nil {
		return journal.Entry{}, [32]byte{}, fmt.Errorf(
			"cortex: decode journal entry %d: %w", seq, err,
		)
	}
	return entry, journal.LeafHash(raw), nil
}

func validToolMatch(verdict string) bool {
	switch verdict {
	case ToolMatchMatched, ToolMatchMismatched, ToolMatchUnknown:
		return true
	default:
		return false
	}
}
