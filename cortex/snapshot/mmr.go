// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package snapshot implements the journal Merkle accumulator and per-namespace
// sparse Merkle trees that compose the cortex snapshot root.
//
// Spec: research/04-cortex.md §7 (snapshots and Merkle layout). Concrete locks
// from sess#7 design review:
//
//   - Journal accumulator = MMR (Merkle Mountain Range), Grin / CT style,
//     1-indexed depth-first node numbering. Append cost ~2 hash ops amortized
//     (matches §7.1: "~2 hash ops per journal write"); persisted entirely
//     under the accum/ namespace so replay can drop+rebuild from j/.
//
//   - State trees = SMT-256 (sparse Merkle, 256-bit address space) per
//     anchored namespace ("memories", "edges"). Empty-subtree compression via
//     precomputed empty-subtree hashes per level. Persisted under idx/smt/.
//
//   - Tombstones folded into parent canonical bytes — Phase 2 stores the
//     tombstone state inside Head.Tombstoned and Phase 6 stores it inside
//     EdgeRecord.Tombstoned. Anchoring `tombstones_root` separately would
//     either double-count or commit to an empty tree (see spec_divergence
//     in matrix.ctx). Folded shape strictly improves single-source-of-truth.
//
//   - sha256 everywhere with per-purpose domain separation.
//
// The MMR shape used here is the textbook "Open Timestamps / Grin" MMR:
//
//	leaves       1   2       4       5   6       8 ...
//	internals          3           7
//	             └─peak─┘   └─peak─┘
//
// In ASCII for 4 leaves (positions 1..7):
//
//	     7
//	   /   \
//	  3     6
//	 / \   / \
//	1   2 4   5
//
// Position numbering is depth-first, left-to-right: leaves and merge
// nodes get the next available position as they are produced. After
// leaf 1 → pos 1; leaf 2 → pos 2, then merge → pos 3; leaf 3 → pos 4;
// leaf 4 → pos 5, then merge → pos 6, then merge → pos 7.
//
// This file implements MMR. SMT is in smt.go; snapshot orchestration in
// snapshot.go. All three are pure functions of the store; State is the
// thin orchestration object exposed to the cortex layer.
package snapshot

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/bits"

	"matrix/cortex/keys"
	"matrix/cortex/store"
)

// Domain-separation prefixes. Each MUST be globally unique within the
// matrix.cortex namespace; bumping the version forces a clean re-anchor.
const (
	// MMRNodeDomain is prepended to a parent node's child concatenation.
	MMRNodeDomain = "matrix.cortex.snapshot.mmr.node.v1"
	// MMRBagDomain is prepended when bagging peaks right-to-left (the
	// inner "spine" hashes that fold multiple peaks into one digest).
	MMRBagDomain = "matrix.cortex.snapshot.mmr.bag.v1"
	// MMRRootDomain wraps the bagged peak digest with the leaf count so
	// the journal root commits to N — this prevents a tree-extension
	// attack where two distinct leaf sequences could share a root.
	MMRRootDomain = "matrix.cortex.snapshot.mmr.root.v1"
)

// hashMMRNode = sha256(MMRNodeDomain || left || right).
func hashMMRNode(left, right [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(MMRNodeDomain))
	h.Write(left[:])
	h.Write(right[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// hashMMRBag = sha256(MMRBagDomain || left || right). Used for the
// right-to-left peak fold so internal merge nodes and bag spine nodes
// can never collide.
func hashMMRBag(left, right [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(MMRBagDomain))
	h.Write(left[:])
	h.Write(right[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// hashMMRRoot = sha256(MMRRootDomain || u64BE(leafCount) || bag). The
// final wrap pins the leaf count into the root so the journal commitment
// can't be confused with a smaller or larger journal sharing the same
// peak structure.
func hashMMRRoot(leafCount uint64, bag [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(MMRRootDomain))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], leafCount)
	h.Write(n[:])
	h.Write(bag[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// EmptyMMRRoot is the canonical "no journal entries" MMR root. Computed
// as hashMMRRoot(0, zeros). Cached so callers don't need to re-derive.
var EmptyMMRRoot [32]byte = hashMMRRoot(0, [32]byte{})

// mmrSize returns the number of MMR nodes for n leaves.
//
// Identity: mmr_size(n) = 2n - popcount(n). Each peak of height h
// contributes 2^(h+1) - 1 nodes; popcount(n) gives the number of peaks
// (one per set bit of n), and summing 2^(h+1)-1 over the set bits
// telescopes to 2n - popcount(n).
func mmrSize(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	return 2*n - uint64(bits.OnesCount64(n))
}

// peakPositions returns the 1-indexed positions of MMR peaks for n leaves
// in DESCENDING height order (tallest peak first). For n=4 → [7]. For
// n=3 → [3, 4]. For n=7 → [7, 10, 11].
//
// The high-bit-first walk gives us peak positions in left-to-right
// order (the tallest tree is leftmost in the MMR), which is what
// bag-of-peaks consumes from-the-right-end.
func peakPositions(n uint64) []uint64 {
	if n == 0 {
		return nil
	}
	var peaks []uint64
	var pos uint64
	for h := 63; h >= 0; h-- {
		bit := uint64(1) << uint(h)
		if n&bit == 0 {
			continue
		}
		// A perfect tree of 2^h leaves contributes 2^(h+1) - 1 nodes.
		// Its peak is at the cumulative position so far + that size.
		size := bit<<1 - 1
		pos += size
		peaks = append(peaks, pos)
	}
	return peaks
}

// MMR is a thin handle over the underlying store. It does not cache state
// in memory; each method reads what it needs from accum/. This keeps the
// in-memory model trivially correct under concurrent commits — the only
// authority is the persisted accum/ keys, which advance atomically with
// the journal write that triggered them.
type MMR struct {
	s *store.Store
}

// NewMMR returns a handle bound to s. Cheap, stateless.
func NewMMR(s *store.Store) *MMR { return &MMR{s: s} }

// LeafCount reads the persisted leaf count. Returns 0 if the key is
// absent (fresh actor, no journal entries yet).
func (m *MMR) LeafCount() (uint64, error) {
	raw, ok, err := m.s.Get(keys.AccumMMRLeafCount)
	if err != nil {
		return 0, fmt.Errorf("snapshot.MMR.LeafCount: get: %w", err)
	}
	if !ok {
		return 0, nil
	}
	if len(raw) != 8 {
		return 0, fmt.Errorf("snapshot.MMR.LeafCount: malformed (len=%d)", len(raw))
	}
	return binary.BigEndian.Uint64(raw), nil
}

// Node reads the hash at MMR position pos. Returns ErrNodeMissing when
// pos has not been persisted. Callers should treat that as a programming
// error (the cascade walks committed positions only).
func (m *MMR) Node(pos uint64) ([32]byte, error) {
	raw, ok, err := m.s.Get(keys.AccumMMRNodeKey(pos))
	if err != nil {
		return [32]byte{}, fmt.Errorf("snapshot.MMR.Node(%d): get: %w", pos, err)
	}
	if !ok {
		return [32]byte{}, fmt.Errorf("snapshot.MMR.Node(%d): %w", pos, ErrNodeMissing)
	}
	if len(raw) != 32 {
		return [32]byte{}, fmt.Errorf("snapshot.MMR.Node(%d): malformed (len=%d)", pos, len(raw))
	}
	var out [32]byte
	copy(out[:], raw)
	return out, nil
}

// StageAppend stages writes for one new leaf onto setter. The leaf goes
// at the next free MMR position, then the cascade merges it with any
// equal-height sibling peaks. The leaf-count counter is bumped in the
// same batch so the MMR state is atomically consistent with the journal
// write the caller is also staging.
//
// Sibling reads default to the underlying store (committed state). When
// setter implements BatchedReader (e.g. the PebbleBatchSetter wrapping
// an indexed batch from store.BeginWrite), reads are routed through the
// batch first — required for multi-AppendJournal atomic batches where
// a later leaf's cascade merges with a sibling staged earlier in the
// same batch (Phase 12 cortex.Attest emits KindAttest + KindLearnWeights
// in one BeginWrite).
//
// setter is typically the *store.WriteBatch backing the journal write,
// or a PebbleBatchSetter wrapping a *pebble.Batch when invoked from a
// JournalHook.
func (m *MMR) StageAppend(setter Setter, leafHash [32]byte) error {
	br, _ := setter.(BatchedReader)
	prev, err := m.leafCountVia(br)
	if err != nil {
		return err
	}
	leafNum := prev + 1
	pos := mmrSize(prev) + 1

	// Place the leaf.
	if err := setter.Set(keys.AccumMMRNodeKey(pos), copyHash(leafHash)); err != nil {
		return fmt.Errorf("snapshot.MMR.StageAppend: set leaf pos %d: %w", pos, err)
	}

	// Cascade merges. The number of merges equals the number of trailing
	// zeros in leafNum: appending the (n+1)-th leaf merges once for every
	// tree of equal height to its left, which by induction is exactly the
	// run of low-order zeros in (n+1)'s binary representation.
	merges := bits.TrailingZeros64(leafNum)
	cur := leafHash
	curPos := pos
	for i := 0; i < merges; i++ {
		// Sibling at height i: cur is at relative offset (2^(i+1) - 1)
		// from its same-height neighbour (a height-i tree contains
		// 2^(i+1) - 1 nodes). The neighbour's peak is therefore at
		// curPos - that offset.
		off := uint64(1)<<uint(i+1) - 1
		siblingPos := curPos - off
		siblingHash, err := m.nodeVia(br, siblingPos)
		if err != nil {
			return fmt.Errorf("snapshot.MMR.StageAppend: sibling at pos %d: %w", siblingPos, err)
		}
		parentPos := curPos + 1
		parentHash := hashMMRNode(siblingHash, cur)
		if err := setter.Set(keys.AccumMMRNodeKey(parentPos), copyHash(parentHash)); err != nil {
			return fmt.Errorf("snapshot.MMR.StageAppend: set parent pos %d: %w", parentPos, err)
		}
		cur = parentHash
		curPos = parentPos
	}

	// Persist the new leaf count.
	var lcBuf [8]byte
	binary.BigEndian.PutUint64(lcBuf[:], leafNum)
	if err := setter.Set(keys.AccumMMRLeafCount, lcBuf[:]); err != nil {
		return fmt.Errorf("snapshot.MMR.StageAppend: set leafcount: %w", err)
	}
	return nil
}

// leafCountVia returns the current leaf count, consulting br first if
// non-nil (so in-batch staged leafcount writes are visible). Falls back
// to the committed store on a miss or nil reader.
func (m *MMR) leafCountVia(br BatchedReader) (uint64, error) {
	if br != nil {
		raw, ok, err := br.GetBatched(keys.AccumMMRLeafCount)
		if err != nil {
			return 0, fmt.Errorf("snapshot.MMR.leafCountVia: batched get: %w", err)
		}
		if ok {
			if len(raw) != 8 {
				return 0, fmt.Errorf("snapshot.MMR.leafCountVia: malformed batched (len=%d)", len(raw))
			}
			return binary.BigEndian.Uint64(raw), nil
		}
	}
	return m.LeafCount()
}

// nodeVia reads MMR node at pos, consulting br first if non-nil.
func (m *MMR) nodeVia(br BatchedReader, pos uint64) ([32]byte, error) {
	if br != nil {
		raw, ok, err := br.GetBatched(keys.AccumMMRNodeKey(pos))
		if err != nil {
			return [32]byte{}, fmt.Errorf("snapshot.MMR.nodeVia(%d): batched get: %w", pos, err)
		}
		if ok {
			if len(raw) != 32 {
				return [32]byte{}, fmt.Errorf("snapshot.MMR.nodeVia(%d): malformed batched (len=%d)", pos, len(raw))
			}
			var out [32]byte
			copy(out[:], raw)
			return out, nil
		}
	}
	return m.Node(pos)
}

// Root returns the current MMR root over the committed leaves. For an
// empty MMR (no leaves) returns EmptyMMRRoot.
//
// Algorithm: read the peak hashes in left-to-right order, fold them
// right-to-left via hashMMRBag (so the leftmost peak ends up "outermost"
// in the bag spine), then wrap the result with hashMMRRoot(leafCount, _)
// to commit to the leaf count.
func (m *MMR) Root() ([32]byte, error) {
	n, err := m.LeafCount()
	if err != nil {
		return [32]byte{}, err
	}
	if n == 0 {
		return EmptyMMRRoot, nil
	}
	peaks := peakPositions(n)
	hashes := make([][32]byte, len(peaks))
	for i, p := range peaks {
		h, err := m.Node(p)
		if err != nil {
			return [32]byte{}, err
		}
		hashes[i] = h
	}
	// Right-to-left bag: start with the rightmost peak and fold leftward,
	// hashing left || acc at each step. For a single peak the bag is the
	// peak hash itself (no domain wrap); the outer hashMMRRoot wrap pins
	// leaf count regardless.
	acc := hashes[len(hashes)-1]
	for i := len(hashes) - 2; i >= 0; i-- {
		acc = hashMMRBag(hashes[i], acc)
	}
	return hashMMRRoot(n, acc), nil
}

// Reset clears the persisted MMR state (leafcount + every accum/mmr/n/
// node key). Used by the replay harness (Phase 11) and by tests that
// drop+rebuild the accumulator. Not exposed in the public Cortex API.
//
// The caller is responsible for sequencing this OUTSIDE any batch and
// for re-running StageAppend over journal leaves to repopulate the
// state.
func (m *MMR) Reset() error {
	// Delete every accum/mmr/n/<pos> key.
	prefix := append([]byte{}, keys.PrefixAccum...)
	prefix = append(prefix, []byte("mmr/n/")...)
	if err := m.s.PrefixIter(prefix, func(k, _ []byte) error {
		return m.s.DB().Delete(append([]byte{}, k...), nil)
	}); err != nil {
		return fmt.Errorf("snapshot.MMR.Reset: clear nodes: %w", err)
	}
	// Delete leafcount.
	if err := m.s.DB().Delete(keys.AccumMMRLeafCount, nil); err != nil {
		return fmt.Errorf("snapshot.MMR.Reset: clear leafcount: %w", err)
	}
	return nil
}

// copyHash returns a fresh slice from h so callers can mutate the
// original after staging. wb.Set retains the slice (Pebble batches copy
// at commit, but explicit copy makes intent obvious and resilient to
// future Pebble-internal changes).
func copyHash(h [32]byte) []byte {
	out := make([]byte, 32)
	copy(out, h[:])
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
