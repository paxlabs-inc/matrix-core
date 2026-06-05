// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Drop primitives — delete every key prefix that maps to spec's `indexes/`
// namespace. Canonical state under spec's `store/` is never touched.
//
// Mapping from research/04-cortex.md §1 architecture to our actual Pebble key
// prefixes is documented in the package doc (replay.go).
//
// Implementation note: we use Pebble's DeleteRange for prefix-scoped wipes
// (O(1) on the LSM level — Pebble writes a single range tombstone) instead of
// iterate-then-delete (which would be O(N) point deletes). The two single-key
// derived-state markers (meta/embed_cursor, meta/embed_vertex_next) use
// Delete since DeleteRange has no benefit on single keys.

package replay

import (
	"fmt"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/keys"
	"matrix/cortex/store"
)

// metaEmbedCursor and metaEmbedVertexNext mirror the package-private
// constants in cortex/embedder.go. Duplicated here so the replay package
// has no import on cortex (which would create a cycle: cortex imports
// replay via cortex.Rebuild).
var (
	metaEmbedCursor     = append(append([]byte{}, keys.PrefixMeta...), []byte("embed_cursor")...)
	metaEmbedVertexNext = append(append([]byte{}, keys.PrefixMeta...), []byte("embed_vertex_next")...)
	// metaEmbedModel — sess#19 Q3 lazy-migrate: records the Model() string
	// of the last completed embedder pass. Derived state; cleared on drop so
	// the next StartEmbedder re-walks the journal from seq=0 under whichever
	// model the caller passes in.
	metaEmbedModel = append(append([]byte{}, keys.PrefixMeta...), []byte("embed_model")...)
)

// derivedSingleMetaKeys are meta/ keys that map to derived state (cleared
// on drop, rewritten by rebuild from the kept canonical state + journal).
// Kept separate from derivedPrefixes because meta/ as a whole is partly
// canonical (meta/journal_head, meta/snapshot_seq) and partly derived.
var derivedSingleMetaKeys = [][]byte{
	metaEmbedCursor,
	metaEmbedVertexNext,
	metaEmbedModel,
	// Phase 12: per-actor learned salience weights. Rebuilt by walking
	// KindLearnWeights entries in the journal.
	keys.MetaSalienceWeights,
}

// derivedPrefixes lists every key prefix that maps to spec's indexes/ side
// of the §1 architecture. Order is alphabetical for deterministic
// iteration in tests; DropDerived doesn't depend on the order.
//
// Citation per prefix:
//
//	vec/         research/04-cortex.md:23 (indexes/vector/)
//	idx/type/    research/04-cortex.md:24 (indexes/predicate/)
//	idx/tag/     research/04-cortex.md:24 (indexes/predicate/)
//	idx/frame/   research/04-cortex.md:24 (indexes/predicate/)
//	idx/actor_obj/ research/04-cortex.md:24 (indexes/predicate/)
//	idx/smt/     sess#7 phase7_locked_design Q3 (anchored-namespace SMT nodes)
//	salience/    research/04-cortex.md:25 (indexes/salience/)
//	accum/       sess#7 phase7_locked_design Q2 (MMR persistence)
var derivedPrefixes = [][]byte{
	keys.PrefixAccum,
	keys.PrefixIdxActorObj,
	keys.PrefixIdxFrame,
	keys.PrefixIdxSMT,
	keys.PrefixIdxTag,
	keys.PrefixIdxType,
	keys.PrefixSalience,
	[]byte("vec/"), // PrefixVecMeta is vec/meta/, but vec/ is the broader umbrella; safe to drop the whole tree
}

// DropDerived deletes every key under spec's `indexes/` namespace from s.
// Idempotent — running twice is the same as running once.
//
// After this call returns:
//
//	accum/* idx/* salience/* vec/* — empty
//	meta/embed_cursor              — absent
//	meta/embed_vertex_next         — absent
//
// Canonical state is untouched:
//
//	m/* mv/* e/* j/* tomb/* snap/* chk/*  — preserved byte-identical
//	meta/journal_head meta/snapshot_seq    — preserved byte-identical
//
// Callers that hold an open snapshot.State on s must reconstruct its
// in-process MMR + SMT views by calling CurrentRoots after the rebuild
// (the underlying handles read from the persisted accum/+idx/smt/* keys
// every call, so they auto-pick-up the post-rebuild state).
func DropDerived(s *store.Store) error {
	if s == nil {
		return ErrNilStore
	}
	db := s.DB()

	for _, prefix := range derivedPrefixes {
		lo, hi := keys.PrefixRange(prefix)
		if hi == nil {
			// PrefixRange returns nil hi when prefix is all 0xff;
			// none of derivedPrefixes are. Treat as defensive guard.
			return fmt.Errorf("replay.DropDerived: prefix %q has no upper bound", prefix)
		}
		if err := db.DeleteRange(lo, hi, pebble.Sync); err != nil {
			return fmt.Errorf("replay.DropDerived: DeleteRange %q: %w", prefix, err)
		}
	}

	// Single-key derived markers. Pebble.Delete is silent on missing
	// keys; no need to handle ErrNotFound.
	for _, k := range derivedSingleMetaKeys {
		if err := db.Delete(k, pebble.Sync); err != nil {
			return fmt.Errorf("replay.DropDerived: delete %q: %w", k, err)
		}
	}

	return nil
}

// CountDerived returns the total number of derived-namespace keys present
// in s. Used by tests to assert that DropDerived left the store empty
// under indexes/ and by ops dashboards measuring index size.
func CountDerived(s *store.Store) (uint64, error) {
	if s == nil {
		return 0, ErrNilStore
	}
	var n uint64
	for _, prefix := range derivedPrefixes {
		err := s.PrefixIter(prefix, func(_, _ []byte) error {
			n++
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("replay.CountDerived: scan %q: %w", prefix, err)
		}
	}
	// Single-key markers
	for _, k := range derivedSingleMetaKeys {
		_, ok, err := s.Get(k)
		if err != nil {
			return 0, fmt.Errorf("replay.CountDerived: get %q: %w", k, err)
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
