// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// provenance.go implements DEJA-VU task 1.1: derived_from provenance edges
// from a consolidated memory back to the verbatim session transcript it was
// extracted from, plus the resolver that expands a memory URI into that exact
// past exchange.
//
// EDGE MODEL. cortex edges are memory.ID -> memory.ID and anchored into the
// edges SMT (edges.go). A session message record (sess/<conv>/<seq>) is NOT a
// memory and has no memory.ID, so a provenance edge is a canonical
// EdgeDerivedFrom AddEdge whose dst is a DETERMINISTIC synthetic ID minted
// from the (conv, seqLo, seqHi) slice identity, with the real slice coordinates
// carried in the edge's opaque Data payload (sessionProvenance). The synthetic
// dst never resolves to a memory, so it does not surface in memory retrieval
// cascades; the Data is the load-bearing pointer the resolver reads. Because
// the dst is slice-scoped, a memory reinforced across multiple turns of one
// conversation accrues one provenance edge per distinct slice rather than
// collapsing to a single first-write edge under AddEdge idempotency.
//
// DERIVED LANE. The resolver is a pure read (Transcript + edge iteration, no
// journal append, no SMT write). AddSessionProvenance is a canonical, journaled,
// anchored edge mutation exactly like any other AddEdge — it is NOT a
// derived/side-channel record; provenance IS world-state.

package cortex

import (
	"crypto/sha256"
	"fmt"
	"time"

	"matrix/cortex/keys"
	"matrix/cortex/memory"
)

// SessionProvenanceSchemaVersion is stamped on every provenance edge payload.
const SessionProvenanceSchemaVersion uint8 = 1

// sessionProvenance is the opaque CBOR payload carried in a provenance edge's
// EdgeRecord.Data: the conversation id and the (inclusive) session seq span of
// the transcript slice the source memory was extracted from. Canonical
// deterministic CBOR (sessEnc/sessDec, session.go) because it is journaled into
// the edge record.
type sessionProvenance struct {
	SchemaVersion  uint8  `cbor:"0,keyasint"`
	ConversationID string `cbor:"1,keyasint"`
	SeqLo          uint64 `cbor:"2,keyasint"`
	SeqHi          uint64 `cbor:"3,keyasint"`
	Heuristic      bool   `cbor:"4,keyasint,omitempty"`
}

// sessionProvenanceDstID mints the deterministic synthetic dst memory.ID for a
// provenance edge over the (conv, seqLo, seqHi) slice. Distinct slices map to
// distinct ids (so a re-consolidated memory accrues an edge per slice); the
// same slice always maps to the same id (so AddEdge stays idempotent). The id
// is derived from a domain-separated hash and is guaranteed non-zero.
func sessionProvenanceDstID(conv string, seqLo, seqHi uint64) memory.ID {
	h := sha256.Sum256([]byte(fmt.Sprintf("matrix://cortex/session-prov/%s/%d/%d", conv, seqLo, seqHi)))
	var id memory.ID
	copy(id[:], h[:16])
	if id.IsZero() {
		id[0] = 1
	}
	return id
}

// AddSessionProvenance adds a canonical EdgeDerivedFrom edge from memID to the
// synthetic node for the (conv, seqLo, seqHi) transcript slice, carrying the
// slice coordinates in the edge Data. Best-effort semantics are the CALLER's
// responsibility (a failed edge must never fail the memory write); this method
// returns the underlying AddEdge error so the caller can swallow it.
func (c *Cortex) AddSessionProvenance(memID memory.ID, conv string, seqLo, seqHi uint64, createdBy string) error {
	return c.addSessionProvenance(memID, conv, seqLo, seqHi, createdBy, false)
}

func (c *Cortex) AddSessionProvenanceHeuristic(memID memory.ID, conv string, seqLo, seqHi uint64, createdBy string) error {
	return c.addSessionProvenance(memID, conv, seqLo, seqHi, createdBy, true)
}

func (c *Cortex) addSessionProvenance(memID memory.ID, conv string, seqLo, seqHi uint64, createdBy string, heuristic bool) error {
	if conv == "" {
		return ErrEmptyConversationID
	}
	sp := sessionProvenance{
		SchemaVersion:  SessionProvenanceSchemaVersion,
		ConversationID: conv,
		SeqLo:          seqLo,
		SeqHi:          seqHi,
		Heuristic:      heuristic,
	}
	data, err := sessEnc.Marshal(&sp)
	if err != nil {
		return fmt.Errorf("cortex.AddSessionProvenance: encode: %w", err)
	}
	dst := sessionProvenanceDstID(conv, seqLo, seqHi)
	return c.AddEdge(memID, memory.EdgeDerivedFrom, dst, AddEdgeMeta{CreatedBy: createdBy, Data: data})
}

// TranscriptSlice is the resolver's result: the verbatim session messages a
// memory was (exactly or approximately) extracted from, with provenance
// metadata. Exact is true when the slice came from a write-time provenance
// edge; false when it came from the CreatedAt-proximity fallback ladder.
type TranscriptSlice struct {
	ConversationID string
	Date           time.Time // timestamp of the first message in the slice (UTC)
	SeqLo          uint64
	SeqHi          uint64
	Exact          bool
	Messages       []Message
}

// ExpandToTranscript resolves a memory URI to the verbatim past exchange it was
// extracted from (DEJA-VU req 1.3). It first follows the memory's derived_from
// provenance edges to a session slice and reads Transcript(conv, seqLo-radius,
// seqHi+radius) verbatim (Exact=true, choosing the most recent slice when a
// memory carries several). When no provenance edge is present it falls back to
// CreatedAt proximity — the nearest session message by timestamp — bounded by
// radius and marked Exact=false. Returns (nil, nil) when nothing resolves
// (fail-open: the caller degrades to no episodic excerpt). radius is clamped to
// >= 0.
func (c *Cortex) ExpandToTranscript(memURI memory.URI, radius int) (*TranscriptSlice, error) {
	if radius < 0 {
		radius = 0
	}
	_, id, _, err := ParseURI(memURI)
	if err != nil {
		return nil, err
	}

	var best *sessionProvenance
	visit := func(rec *memory.EdgeRecord) error {
		if len(rec.Data) == 0 {
			return nil
		}
		var sp sessionProvenance
		if derr := sessDec.Unmarshal(rec.Data, &sp); derr != nil {
			return nil // a memory->memory derived_from edge (no session Data)
		}
		if sp.SchemaVersion != SessionProvenanceSchemaVersion || sp.ConversationID == "" {
			return nil
		}
		if best == nil || sp.SeqHi > best.SeqHi {
			cp := sp
			best = &cp
		}
		return nil
	}
	if err := c.IterEdgesOut(id, IterEdgesOptions{Types: []memory.EdgeType{memory.EdgeDerivedFrom}}, visit); err != nil {
		return nil, err
	}
	if best != nil {
		return c.transcriptSliceExact(best.ConversationID, best.SeqLo, best.SeqHi, radius, !best.Heuristic)
	}
	return c.transcriptSliceApprox(id, radius)
}

// transcriptSliceExact reads the verbatim slice for a write-time provenance
// edge, widened by radius on both ends and clamped at the low end.
func (c *Cortex) transcriptSliceExact(conv string, seqLo, seqHi uint64, radius int, exact bool) (*TranscriptSlice, error) {
	r := uint64(radius)
	from := seqLo
	if r > from {
		from = 0
	} else {
		from -= r
	}
	hi := seqHi + r
	msgs, err := c.Transcript(conv, from, 0)
	if err != nil {
		return nil, err
	}
	kept := msgs[:0]
	for _, m := range msgs {
		if m.Seq > hi {
			break
		}
		kept = append(kept, m)
	}
	slice := &TranscriptSlice{
		ConversationID: conv,
		SeqLo:          seqLo,
		SeqHi:          seqHi,
		Exact:          exact,
		Messages:       kept,
	}
	if len(kept) > 0 {
		slice.Date = time.Unix(0, kept[0].TS).UTC()
	}
	return slice, nil
}

// transcriptSliceApprox is the fallback ladder for a memory with no write-time
// provenance edge: find the session message whose timestamp is nearest the
// memory's CreatedAt across all conversations, and return a radius-bounded
// window around it, marked Exact=false. Returns (nil, nil) when the memory
// cannot be resolved or no session records exist (fail-open).
func (c *Cortex) transcriptSliceApprox(id memory.ID, radius int) (*TranscriptSlice, error) {
	mem, err := c.ResolveLatest(id)
	if err != nil {
		return nil, nil // fail-open: an unresolvable memory yields no slice
	}
	target := mem.Version.CreatedAt.UnixNano()

	var (
		bestConv string
		bestSeq  uint64
		bestTS   int64
		bestDiff int64 = -1
	)
	_ = c.s.PrefixIter(keys.PrefixSession, func(k, v []byte) error {
		conv, seq, perr := keys.ParseSessionKey(k)
		if perr != nil {
			return nil
		}
		var rec SessionRecord
		if derr := DecodeSessionRecord(v, &rec); derr != nil {
			return nil
		}
		diff := rec.TS - target
		if diff < 0 {
			diff = -diff
		}
		if bestDiff < 0 || diff < bestDiff {
			bestDiff, bestConv, bestSeq, bestTS = diff, conv, seq, rec.TS
		}
		return nil
	})
	if bestConv == "" {
		return nil, nil
	}

	r := uint64(radius)
	from := bestSeq
	if r > from {
		from = 0
	} else {
		from -= r
	}
	hi := bestSeq + r
	msgs, err := c.Transcript(bestConv, from, 0)
	if err != nil {
		return nil, nil
	}
	kept := msgs[:0]
	for _, m := range msgs {
		if m.Seq > hi {
			break
		}
		kept = append(kept, m)
	}
	return &TranscriptSlice{
		ConversationID: bestConv,
		Date:           time.Unix(0, bestTS).UTC(),
		SeqLo:          from,
		SeqHi:          hi,
		Exact:          false,
		Messages:       kept,
	}, nil
}
