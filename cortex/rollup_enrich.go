// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 2.3: the OPTIONAL LLM enrichment lane for the
// temporal-ladder rollup (rollup.go / cascade.go).
//
// # What this is (req.3.4 + req.4.3)
//
// A rollup's deterministic extractive ShortForm is the replayable FLOOR. This
// file adds a SEPARATE derived record — an EnrichRecord — produced by an LLM
// that rewrites that short-form into richer prose. The enrichment is:
//
//   - OPTIONAL: it MAY be absent; without it the deterministic short-form
//     stands alone (req.3.4).
//   - SEPARATE: it is stored at a PARALLEL window key enr/<tier><start>
//     (keys.EnrichKey) — the stored RollupRecord is NEVER mutated to set
//     EnrichRef, so the deterministic floor's bytes + RecordHash stay
//     byte-stable and the task 2.1/2.2 determinism + Cascade idempotence
//     (which compare record bytes) are untouched.
//   - REBUILDABLE from the deterministic short-form: EnrichRollup re-derives
//     it by calling the Enricher over rec.ShortForm.
//   - NEVER load-bearing for determinism or replay (req.4.3): it carries no
//     SMT write, and it is resolved by the same (tier, start) window key with
//     a SourceRecordHash staleness guard — if the rollup is later rebuilt with
//     different content, the hashes diverge and the enrichment is treated as
//     stale/absent, so the deterministic short-form always stands.
//
// # Read-time view (no persisted pointer)
//
// LoadRollupEnriched loads the rollup and, IFF a non-stale enrichment exists,
// sets rec.EnrichRef = BuildEnrichURI(tier, start) IN MEMORY (never persisted).
// The stored RollupRecord always has EnrichRef == "".
//
// # Config knob lives at the CALLER
//
// cortex holds no LLM client and no enrich_enabled/enrich_model config. The
// caller (Neo / Chronos) owns those knobs and only invokes EnrichRollup when
// enrichment is enabled, passing a real Enricher wired to a real model. There
// is deliberately NO cortex-side gate: the seam is a pure extension point,
// mirroring how cortex takes a pluggable Embedder.
//
// # Derived-lane posture
//
// EnrichRollup writes enr/<tier><start> + a KindRollupEnrich journal entry in
// one atomic batch with NO snap.StageMemoryUpdate / StageEdgeUpdate — the same
// posture as BuildRollup (rollup.go:415-429) and cortex.Compact
// (compact.go:429-431).

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

// Enricher is the pluggable LLM seam the caller wires a real model into. Given
// the rollup's deterministic ShortForm (and the full record for context), it
// returns a richer prose summary plus the model identifier that produced it.
//
// This is a genuine extension seam (mirrors cortex's pluggable Embedder), NOT
// a fake: the caller supplies a real implementation. cortex never inspects the
// prose semantically — it only pins it cryptographically and gates it by the
// SourceRecordHash staleness check.
type Enricher interface {
	Enrich(shortForm string, rec *RollupRecord) (prose string, model string, err error)
}

// EnrichRecord is the canonical CBOR-encoded blob persisted at
// enr/<tier><start>, PARALLEL to the roll/<tier><start> RollupRecord. It is a
// derived, rebuildable LLM rewrite of the deterministic ShortForm; it is never
// part of any anchored SMT root and never load-bearing for replay (req.4.3).
type EnrichRecord struct {
	SchemaVersion    uint8    `cbor:"0,keyasint"`
	Window           Window   `cbor:"1,keyasint"`
	SourceRecordHash [32]byte `cbor:"2,keyasint"` // RecordHash of the rollup this was derived from
	Prose            string   `cbor:"3,keyasint"`
	Model            string   `cbor:"4,keyasint"`
}

// EnrichSchemaVersion is stamped on every emitted EnrichRecord and
// EnrichPayload. Bumping requires a journal-kind migration.
const EnrichSchemaVersion uint8 = 1

// Canonical CBOR encoder for EnrichRecord. Mirrors rollup.go's rollEnc/rollDec
// init (rollup.go:156-167): CoreDetEncOptions produces RFC 8949 §4.2.1
// deterministic encoding — required because the encoded bytes are
// integrity-hashed into EnrichPayload.RecordHash and because re-enriching with
// the same deterministic Enricher must yield byte-identical records.
var (
	enrEnc cbor.EncMode
	enrDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("cortex/rollup_enrich: build EncMode: %w", err))
	}
	enrEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("cortex/rollup_enrich: build DecMode: %w", err))
	}
	enrDec = dm
}

// EncodeEnrichRecord returns canonical deterministic CBOR for r.
func EncodeEnrichRecord(r *EnrichRecord) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("cortex/rollup_enrich: nil EnrichRecord")
	}
	return enrEnc.Marshal(r)
}

// DecodeEnrichRecord parses canonical CBOR into out.
func DecodeEnrichRecord(b []byte, out *EnrichRecord) error {
	return enrDec.Unmarshal(b, out)
}

// BuildEnrichURI returns the agent-facing canonical URI for an enrichment
// record:
//
//	matrix://cortex/rollup_enrich/<tier>/<start-unix-nano>
func BuildEnrichURI(tier RollupTier, start int64) memory.URI {
	return memory.URI(fmt.Sprintf("matrix://cortex/rollup_enrich/%s/%d", tier.String(), start))
}

// rollupRecordHash re-encodes rec exactly as BuildRollup does (EncodeRollupRecord
// + sha256) and returns the RecordHash. This is the SAME derivation the
// deterministic lane pins into journal.RollupPayload.RecordHash, so the source
// pin an enrichment carries is directly comparable to a freshly-rebuilt rollup.
func rollupRecordHash(rec *RollupRecord) ([32]byte, error) {
	encoded, err := EncodeRollupRecord(rec)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

// EnrichRollup produces (or reproduces) the OPTIONAL LLM enrichment for the
// rollup at (tier, start): it loads the rollup, computes the rollup's current
// RecordHash, calls e.Enrich over the deterministic ShortForm, and persists the
// resulting EnrichRecord in the DERIVED lane (enr/<tier><start> + a
// KindRollupEnrich journal entry, NO SMT write).
//
// It never mutates the stored RollupRecord, so the deterministic floor stays
// byte-stable. Re-running with the same deterministic Enricher over the same
// rollup yields a byte-identical EnrichRecord.
//
// Returns memory.ErrNotFound (wrapped) when there is no rollup to enrich.
func (c *Cortex) EnrichRollup(tier RollupTier, start int64, e Enricher) (memory.URI, error) {
	if e == nil {
		return "", fmt.Errorf("cortex.EnrichRollup: nil Enricher")
	}
	rec, err := c.LoadRollup(tier, start)
	if err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: load rollup: %w", err)
	}

	srcHash, err := rollupRecordHash(rec)
	if err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: hash rollup: %w", err)
	}

	prose, model, err := e.Enrich(rec.ShortForm, rec)
	if err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: enrich: %w", err)
	}

	er := &EnrichRecord{
		SchemaVersion:    EnrichSchemaVersion,
		Window:           rec.Window,
		SourceRecordHash: srcHash,
		Prose:            prose,
		Model:            model,
	}
	encodedRec, err := EncodeEnrichRecord(er)
	if err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: encode record: %w", err)
	}
	recordHash := sha256.Sum256(encodedRec)

	// Journal payload carries only identity + the source pin + the self pin.
	ep := &journal.EnrichPayload{
		SchemaVersion:    EnrichSchemaVersion,
		Tier:             uint8(rec.Window.Tier),
		Start:            rec.Window.Start,
		SourceRecordHash: srcHash,
		RecordHash:       recordHash,
		Model:            model,
	}
	epBytes, err := journal.EncodeEnrichPayload(ep)
	if err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindRollupEnrich,
		CreatedAt: c.now().UnixNano(),
		Payload:   epBytes,
	}

	// Atomic derived-lane batch: enr/<tier><start> record + journal entry.
	// NO SMT update — the enrichment is derived audit / working prose, never
	// canonical world-state (rollup.go:415-429 posture).
	enrKey := keys.EnrichKey(uint8(rec.Window.Tier), uint64(rec.Window.Start))
	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(enrKey, encodedRec); err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: set enr: %w", err)
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: append journal: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.EnrichRollup: commit: %w", err)
	}

	return BuildEnrichURI(rec.Window.Tier, rec.Window.Start), nil
}

// LoadEnrichment returns the persisted EnrichRecord stored at enr/<tier><start>.
// Returns memory.ErrNotFound when no enrichment exists.
func (c *Cortex) LoadEnrichment(tier RollupTier, start int64) (*EnrichRecord, error) {
	k := keys.EnrichKey(uint8(tier), uint64(start))
	raw, ok, err := c.s.Get(k)
	if err != nil {
		return nil, fmt.Errorf("cortex.LoadEnrichment: get: %w", err)
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var rec EnrichRecord
	if err := DecodeEnrichRecord(raw, &rec); err != nil {
		return nil, fmt.Errorf("cortex.LoadEnrichment: decode: %w", err)
	}
	return &rec, nil
}

// LoadRollupEnriched returns the rollup at (tier, start) with EnrichRef
// populated IN MEMORY (never persisted) IFF a NON-STALE enrichment exists.
//
// An enrichment is non-stale when its SourceRecordHash equals the rollup's
// CURRENT RecordHash — i.e. it was derived from exactly this rollup content. If
// the enrichment is absent, or stale (the rollup was rebuilt with different
// content so the hashes diverge), the rollup is returned with EnrichRef == ""
// and the deterministic short-form stands alone (req.3.4 + req.4.3: the
// enrichment is never load-bearing).
func (c *Cortex) LoadRollupEnriched(tier RollupTier, start int64) (*RollupRecord, error) {
	rec, err := c.LoadRollup(tier, start)
	if err != nil {
		return nil, err
	}

	en, err := c.LoadEnrichment(tier, start)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			return rec, nil // no enrichment → deterministic short-form stands
		}
		return nil, fmt.Errorf("cortex.LoadRollupEnriched: load enrichment: %w", err)
	}

	curHash, err := rollupRecordHash(rec)
	if err != nil {
		return nil, fmt.Errorf("cortex.LoadRollupEnriched: hash rollup: %w", err)
	}
	if en.SourceRecordHash == curHash {
		rec.EnrichRef = string(BuildEnrichURI(tier, start))
	}
	return rec, nil
}
