// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Rebuild primitives — walk the kept canonical state (m/, mv/, e/, j/) and
// re-emit derived projections deterministically.
//
// Three independent rebuild phases:
//
//  1. Memories — walk m/<id>, recompute idx/type+idx/tag+idx/frame+
//     idx/actor_obj, write fresh salience cold score, stage memories SMT
//     update.
//
//  2. Edges — walk e/from/<src>/<t>/<dst>, stage edges SMT update.
//
//  3. Journal MMR — walk j/<seq> in seq order, compute LeafHash of the
//     persisted CBOR bytes, stage the MMR append.
//
// Each rebuild step commits its own Pebble batch. This is required for
// SMT/MMR correctness: SMT.StageUpdate / MMR.StageAppend read sibling
// nodes from the persisted store mid-call, so two staged updates on the
// same uncommitted batch would race on sibling reads. Per-step commits
// add ~50µs sync per memory; for 100k memories that's ~5s, acceptable
// for a maintenance operation.

package replay

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
	"matrix/cortex/snapshot"
	"matrix/cortex/store"
)

// rebuildMemoriesIndexes walks m/<id> heads and re-emits every derived key
// the original cortex.Write path emits, plus the memories SMT update.
//
// idx/type and idx/tag carry a `created` time component. The original
// Write code uses the wallclock time of the write. We can't recover that
// exactly without the journal payload carrying it (it doesn't), so we
// load mv/<id>/v/1 (immutable, always present, carries CreatedAt of the
// original Write) and use that. For memories never updated, this exactly
// matches the original key bytes; for updated memories it matches the
// FIRST-WRITE time, which is the documented semantics of "created".
//
// idx/* keys are NOT inputs to OverallRoot, so any deterministic created
// value would satisfy the §13.4 invariant. Using v/1.CreatedAt is the
// most faithful reconstruction.
func rebuildMemoriesIndexes(s *store.Store, snap *snapshot.State, nowFn func() time.Time) (uint64, error) {
	now := nowFn()
	var count uint64

	// Pre-scan the heads. We can't iterate m/ AND open a write batch
	// per head simultaneously (the iter holds a Pebble snapshot of the
	// database state at iter open time, but our in-flight batches
	// commit forward; the iter doesn't see them, which is fine — we're
	// not reading from m/ inside the batch). Still safer to collect
	// IDs first and then iterate the slice with a fresh Get per ID:
	// guarantees the rebuild reads the canonical Head bytes.
	type headRef struct {
		id   memory.ID
		head []byte
	}
	var heads []headRef
	if err := s.PrefixIter(keys.PrefixMemoryHead, func(k, v []byte) error {
		if len(k) != len(keys.PrefixMemoryHead)+keys.ULIDSize {
			return fmt.Errorf("replay: malformed m/ key length %d", len(k))
		}
		var id memory.ID
		copy(id[:], k[len(keys.PrefixMemoryHead):])
		// Copy the value: PrefixIter doesn't promise stability past the
		// callback return.
		hv := make([]byte, len(v))
		copy(hv, v)
		heads = append(heads, headRef{id: id, head: hv})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("replay: scan m/: %w", err)
	}

	for _, hr := range heads {
		if err := rebuildOneMemory(s, snap, hr.id, hr.head, now); err != nil {
			return 0, fmt.Errorf("replay: memory %x: %w", hr.id, err)
		}
		count++
	}
	return count, nil
}

// rebuildOneMemory commits one Pebble batch for one m/<id> head: the
// idx/* projections, salience cold score, and the memories SMT update.
//
// All writes go through a raw *pebble.Batch (bypassing store.BeginWrite)
// because rebuild does NOT journal — it re-derives state from existing
// j/ entries which were already journaled at original-write time.
func rebuildOneMemory(s *store.Store, snap *snapshot.State, id memory.ID, headBytes []byte, now time.Time) error {
	var h memory.Head
	if err := memory.DecodeHead(headBytes, &h); err != nil {
		return fmt.Errorf("decode head: %w", err)
	}

	// Recover original write time from mv/<id>/v/1.CreatedAt. This is
	// the immutable first-write timestamp; matches what cortex.Write
	// stamped into idx/type and idx/tag created components.
	v1, err := readVersion(s, id, 1)
	if err != nil {
		return fmt.Errorf("load mv/<id>/v/1: %w", err)
	}
	createdNanos := uint64(v1.CreatedAt.UnixNano())

	ulid := toKeysULID(id)
	b := s.DB().NewBatch()
	defer b.Close()

	// idx/type/<type>/<created>/<id>
	if err := b.Set(
		keys.IdxTypeKey(byte(h.Type), createdNanos, ulid),
		nil,
		nil,
	); err != nil {
		return fmt.Errorf("set idx/type: %w", err)
	}

	// idx/tag/<tag_hash>/<created>/<id> per tag.
	for _, tag := range h.Tags {
		if err := b.Set(
			keys.IdxTagKey(hashTag(string(tag)), createdNanos, ulid),
			nil,
			nil,
		); err != nil {
			return fmt.Errorf("set idx/tag: %w", err)
		}
	}

	// idx/frame/<verb>/<kind>/<obj_hash>/<id> per frame (all types).
	// idx/actor_obj/<verb>/<obj_hash>/<created>/<id> per frame ONLY
	// when h.Type == TypeEvent (Outcomes is Event-only per
	// research/03-retrieval-patterns.md §2.1).
	for _, fr := range h.Frames {
		objHash := fr.Hash()
		if err := b.Set(
			keys.IdxFrameKey(byte(fr.Verb), byte(fr.ObjKind), objHash, ulid),
			nil,
			nil,
		); err != nil {
			return fmt.Errorf("set idx/frame: %w", err)
		}
		if h.Type == memory.TypeEvent {
			if err := b.Set(
				keys.IdxActorObjKey(byte(fr.Verb), objHash, createdNanos, ulid),
				nil,
				nil,
			); err != nil {
				return fmt.Errorf("set idx/actor_obj: %w", err)
			}
		}
	}

	// salience/<id> — fresh cold score using the supplied clock. The
	// cached value depends on `now` but is NOT in OverallRoot; clock
	// drift between rebuild and original-write is intentional and
	// safe (research/04-cortex.md §8 + invariants_locked sess#3).
	score := salience.NewForWrite(h.DeclaredImportance, now)
	if h.Tombstoned != nil {
		// Tombstoned memories collapse to zero per cortex.Tombstone
		// semantics; salience.ZeroForTombstone updates ComputedAt and
		// zeros Cached but preserves factor inputs.
		salience.ZeroForTombstone(&score, now)
	}
	scoreBytes, err := salience.Encode(&score)
	if err != nil {
		return fmt.Errorf("encode salience: %w", err)
	}
	if err := b.Set(keys.SalienceKey(ulid), scoreBytes, nil); err != nil {
		return fmt.Errorf("set salience: %w", err)
	}

	// Stage the memories SMT update with the canonical Head bytes
	// (verbatim — we did NOT decode-and-re-encode, so any future Head
	// schema additions stay round-trippable).
	setter := snapshot.NewPebbleBatchSetter(b)
	if err := snap.StageMemoryUpdate(setter, id, headBytes); err != nil {
		return fmt.Errorf("stage SMT memories: %w", err)
	}

	if err := b.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// rebuildSalienceFromJournal walks j/<seq> in seq order and replays
// the Phase 11.5 salience-mutation events (KindFind LateBinding,
// KindAttest). For each matching entry, decode the payload and apply
// the same BumpForAccess / BumpForCitation / DecrementCitation that
// the original write path applied — so post-rebuild salience.AccessCount
// and salience.Citations match pre-drop values exactly.
//
// LastUsed and Cached drift with the replay clock (research/04 §8.4
// recency factor) — that's acceptable because salience.* is not in
// OverallRoot. The byte-exact reconstruction target is AccessCount +
// Citations, which are integer counters fed by the journal.
//
// We commit one batch per journal entry. Salience writes do NOT stage
// any SMT update so the per-entry batch is just N salience writes; no
// sibling-read race like the SMT/MMR rebuild loops above.
//
// Returns the total number of (j/<seq>, memoryID) bump-pairs applied.
// Tombstoned-at-replay-time memories are silently skipped (matches
// Find/Attest live-path filter); they don't count toward the returned
// total.
func rebuildSalienceFromJournal(s *store.Store, now time.Time) (uint64, error) {
	type entryRef struct {
		seq uint64
		raw []byte
	}
	var entries []entryRef

	// Collect first; can't safely mutate inside PrefixIter. j/ is in
	// the kept namespace, so we read straight from canonical state.
	if err := s.PrefixIter(keys.PrefixJournal, func(k, v []byte) error {
		seq, err := keys.ParseJournalKey(k)
		if err != nil {
			return fmt.Errorf("parse j/ key: %w", err)
		}
		// Copy v — PrefixIter doesn't promise stability past the callback.
		body := make([]byte, len(v))
		copy(body, v)
		entries = append(entries, entryRef{seq: seq, raw: body})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("scan j/: %w", err)
	}

	var totalBumps uint64
	for _, er := range entries {
		var e journal.Entry
		if err := journal.Decode(er.raw, &e); err != nil {
			return totalBumps, fmt.Errorf("decode j/%d: %w", er.seq, err)
		}
		switch e.Kind {
		case journal.KindFind:
			var pl journal.LateBindingPayload
			if err := journal.DecodeLateBindingPayload(e.Payload, &pl); err != nil {
				return totalBumps, fmt.Errorf("decode KindFind payload at j/%d: %w", er.seq, err)
			}
			applied, err := applySalienceBumps(s, pl.AccessedIDs, salienceBumpAccess, now)
			if err != nil {
				return totalBumps, fmt.Errorf("apply KindFind bumps at j/%d: %w", er.seq, err)
			}
			totalBumps += applied
		case journal.KindAttest:
			var pl journal.AttestPayload
			if err := journal.DecodeAttestPayload(e.Payload, &pl); err != nil {
				return totalBumps, fmt.Errorf("decode KindAttest payload at j/%d: %w", er.seq, err)
			}
			var bumpKind salienceBumpKind
			switch {
			case pl.Outcome == journal.AttestOutcomeSuccess:
				bumpKind = salienceBumpCitation
			case pl.Outcome == journal.AttestOutcomeFailure &&
				(pl.Reason == journal.AttestReasonFactualError ||
					pl.Reason == journal.AttestReasonWrongAssumption):
				bumpKind = salienceBumpDecrement
			default:
				// Failure with other reason: spec leaves Citations
				// unchanged. Live path still writes salience to refresh
				// LastUsed (see attest.go default branch); replay path
				// skips the write because the LastUsed drift is clock-
				// dependent and the integer counters are unchanged.
				continue
			}
			applied, err := applySalienceBumps(s, pl.CitedIDs, bumpKind, now)
			if err != nil {
				return totalBumps, fmt.Errorf("apply KindAttest bumps at j/%d: %w", er.seq, err)
			}
			totalBumps += applied
		case journal.KindLearnWeights:
			var pl journal.LearnWeightsPayload
			if err := journal.DecodeLearnWeightsPayload(e.Payload, &pl); err != nil {
				return totalBumps, fmt.Errorf("decode KindLearnWeights payload at j/%d: %w", er.seq, err)
			}
			if pl.Skipped {
				// Live path does NOT write meta/salience_weights when the
				// EMA step degenerated; replay must match so the byte-
				// identity property holds (no key materialized in either).
				continue
			}
			if err := applyLearnWeights(s, &pl, now); err != nil {
				return totalBumps, fmt.Errorf("apply KindLearnWeights at j/%d: %w", er.seq, err)
			}
		}
	}
	return totalBumps, nil
}

// salienceBumpKind enumerates the three Phase 11.5 mutation flavours.
type salienceBumpKind int

const (
	salienceBumpAccess salienceBumpKind = iota
	salienceBumpCitation
	salienceBumpDecrement
)

// applySalienceBumps reads, mutates, and writes salience/<id> for each
// id in ids. Tombstoned-at-replay-time memories (m/<id> present and
// Head.Tombstoned != nil) are skipped. Missing m/<id> (e.g. a Find that
// occurred before the memory was tombstone-deleted) is also skipped.
//
// We use a fresh Pebble batch per call (one journal entry = one batch
// of N salience writes) and commit with Sync to match the durability
// posture of the live write path.
func applySalienceBumps(s *store.Store, ids [][16]byte, kind salienceBumpKind, now time.Time) (uint64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	b := s.DB().NewBatch()
	defer b.Close()
	var applied uint64
	for _, idArr := range ids {
		var ulid keys.ULID
		copy(ulid[:], idArr[:])
		// Skip tombstoned-at-replay-time memories. Cheap fast-path:
		// check if m/<id> exists; if not, skip.
		headBytes, ok, err := s.Get(keys.MemoryHeadKey(ulid))
		if err != nil {
			return applied, fmt.Errorf("read m/<id>: %w", err)
		}
		if !ok {
			continue
		}
		var h memory.Head
		if err := memory.DecodeHead(headBytes, &h); err != nil {
			return applied, fmt.Errorf("decode m/<id>: %w", err)
		}
		if h.Tombstoned != nil {
			continue
		}

		var sc salience.Score
		raw, ok, err := s.Get(keys.SalienceKey(ulid))
		if err != nil {
			return applied, fmt.Errorf("read salience/<id>: %w", err)
		}
		if ok {
			if err := salience.Decode(raw, &sc); err != nil {
				return applied, fmt.Errorf("decode salience/<id>: %w", err)
			}
		} else {
			// Salience was just seeded by rebuildOneMemory; cache-miss
			// here is unexpected but not fatal (matches live-path
			// fallback).
			sc = salience.NewForWrite(h.DeclaredImportance, now)
		}
		switch kind {
		case salienceBumpAccess:
			salience.BumpForAccess(&sc, now)
		case salienceBumpCitation:
			salience.BumpForCitation(&sc, now)
		case salienceBumpDecrement:
			salience.DecrementCitation(&sc, now)
		}
		encoded, err := salience.Encode(&sc)
		if err != nil {
			return applied, fmt.Errorf("encode salience/<id>: %w", err)
		}
		if err := b.Set(keys.SalienceKey(ulid), encoded, nil); err != nil {
			return applied, fmt.Errorf("set salience/<id>: %w", err)
		}
		applied++
	}
	if applied == 0 {
		return 0, nil
	}
	if err := b.Commit(pebble.Sync); err != nil {
		return applied, fmt.Errorf("commit salience bumps: %w", err)
	}
	return applied, nil
}

// applyLearnWeights persists pl.NewW* to meta/salience_weights, matching
// the live cortex.Attest write. Trusts the journaled NewW* values: any
// tampering with the payload would change the journal-leaf hash and
// diverge journal_root (and therefore OverallRoot), so the byte stream
// IS authoritative. Updates count is preserved as journaled; UpdatedAt
// is stamped to the replay clock so it stays consistent with the rest
// of the replay clock (LastUsed/Cached/ComputedAt convention).
//
// Drift in UpdatedAt across replay clocks is acceptable for the same
// reason salience.Cached drift is acceptable: meta/salience_weights is
// NOT in OverallRoot, only journal_root is.
func applyLearnWeights(s *store.Store, pl *journal.LearnWeightsPayload, now time.Time) error {
	// Read current weights so the Updates counter advances correctly
	// when multiple KindLearnWeights entries replay in sequence. Cold
	// start (no key) is fine — DefaultWeights returns the cold floor.
	cur, _, err := salience.ReadWeights(s)
	if err != nil {
		return fmt.Errorf("read weights: %w", err)
	}
	next := salience.Weights{
		SchemaVersion: salience.WeightsSchemaVersion,
		WR:            pl.NewWR,
		WA:            pl.NewWA,
		WC:            pl.NewWC,
		WD:            pl.NewWD,
		WV:            pl.NewWV,
		UpdatedAt:     now.UnixNano(),
		Updates:       cur.Updates + 1,
	}
	encoded, err := salience.EncodeWeights(&next)
	if err != nil {
		return fmt.Errorf("encode weights: %w", err)
	}
	b := s.DB().NewBatch()
	defer b.Close()
	if err := b.Set(keys.MetaSalienceWeights, encoded, nil); err != nil {
		return fmt.Errorf("set meta/salience_weights: %w", err)
	}
	if err := b.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("commit weights: %w", err)
	}
	return nil
}

// rebuildEdgesSMT walks e/from/<src>/<t>/<dst> and stages an edges SMT
// update per record. Forward direction only — the reverse e/to/ records
// are byte-identical and would double-count (Phase 7 invariant).
func rebuildEdgesSMT(s *store.Store, snap *snapshot.State) (uint64, error) {
	type edgeRef struct {
		src  memory.ID
		t    byte
		dst  memory.ID
		body []byte
	}
	var edges []edgeRef
	if err := s.PrefixIter(keys.PrefixEdgeFrom, func(k, v []byte) error {
		src, edge, dst, err := keys.ParseEdgeFromKey(k)
		if err != nil {
			return fmt.Errorf("parse e/from key: %w", err)
		}
		body := make([]byte, len(v))
		copy(body, v)
		var srcID, dstID memory.ID
		copy(srcID[:], src[:])
		copy(dstID[:], dst[:])
		edges = append(edges, edgeRef{src: srcID, t: edge, dst: dstID, body: body})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("scan e/from/: %w", err)
	}

	for _, er := range edges {
		b := s.DB().NewBatch()
		setter := snapshot.NewPebbleBatchSetter(b)
		if err := snap.StageEdgeUpdate(setter, er.src, er.t, er.dst, er.body); err != nil {
			b.Close()
			return 0, fmt.Errorf("stage SMT edge %x->%x t=%d: %w", er.src, er.dst, er.t, err)
		}
		if err := b.Commit(pebble.Sync); err != nil {
			b.Close()
			return 0, fmt.Errorf("commit edge SMT: %w", err)
		}
		b.Close()
	}
	return uint64(len(edges)), nil
}

// rebuildJournalMMR walks j/<seq> in seq order and stages the MMR leaf
// for each. Leaf hash is computed via journal.LeafHash over the persisted
// CBOR bytes — same function the live AppendJournal path uses, so the
// rebuilt MMR is byte-identical to the live one (assuming j/ is intact).
//
// Each MMR.StageAppend reads previously-committed MMR sibling positions,
// which means we MUST commit each leaf's batch before staging the next
// (see file header).
func rebuildJournalMMR(s *store.Store, snap *snapshot.State) (uint64, error) {
	mmr := snap.MMR()

	type leafRef struct {
		seq  uint64
		hash [32]byte
	}
	var leaves []leafRef

	// PrefixIter on j/ yields ascending seq order (numeric == byte order
	// under the BE-uint64 encoding).
	if err := s.PrefixIter(keys.PrefixJournal, func(k, v []byte) error {
		seq, err := keys.ParseJournalKey(k)
		if err != nil {
			return fmt.Errorf("parse j/ key: %w", err)
		}
		leaves = append(leaves, leafRef{seq: seq, hash: journalLeafHash(v)})
		return nil
	}); err != nil {
		return 0, fmt.Errorf("scan j/: %w", err)
	}

	// Sanity: gap-free invariant. seq[i] should equal i.
	for i, lf := range leaves {
		if lf.seq != uint64(i) {
			return 0, fmt.Errorf("replay: journal gap detected: leaf at idx %d has seq %d", i, lf.seq)
		}
	}

	for _, lf := range leaves {
		b := s.DB().NewBatch()
		setter := snapshot.NewPebbleBatchSetter(b)
		if err := mmr.StageAppend(setter, lf.hash); err != nil {
			b.Close()
			return 0, fmt.Errorf("stage MMR leaf seq=%d: %w", lf.seq, err)
		}
		if err := b.Commit(pebble.Sync); err != nil {
			b.Close()
			return 0, fmt.Errorf("commit MMR leaf seq=%d: %w", lf.seq, err)
		}
		b.Close()
	}
	return uint64(len(leaves)), nil
}

// readVersion fetches mv/<id>/v/<version> as a fully-decoded memory.Version.
func readVersion(s *store.Store, id memory.ID, version uint64) (*memory.Version, error) {
	ulid := toKeysULID(id)
	raw, ok, err := s.Get(keys.MemoryVersionKey(ulid, version))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("mv/<id>/v/%d not found", version)
	}
	var v memory.Version
	if err := memory.DecodeVersion(raw, &v); err != nil {
		return nil, fmt.Errorf("decode version: %w", err)
	}
	return &v, nil
}

// toKeysULID mirrors the cortex package-private helper. memory.ID and
// keys.ULID are both [16]byte but the Go type system requires an
// explicit conversion. Duplicated here so this package has no import on
// cortex.
func toKeysULID(id memory.ID) keys.ULID {
	var u keys.ULID
	copy(u[:], id[:])
	return u
}

// hashTag mirrors cortex.hashTag — computes the 8-byte sha256 prefix of
// a tag string used as the idx/tag bucket key.
func hashTag(tag string) [keys.TagHashSize]byte {
	sum := sha256.Sum256([]byte(tag))
	var out [keys.TagHashSize]byte
	copy(out[:], sum[:keys.TagHashSize])
	return out
}

// journalLeafHash returns the canonical leaf hash for the persisted CBOR
// bytes of a j/<seq> value. Thin alias around journal.LeafHash so this
// file reads symmetrically with the other domain-keyed hashes.
func journalLeafHash(encoded []byte) [32]byte {
	return journal.LeafHash(encoded)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
