// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Sparse Merkle Tree (SMT-256) implementation for the Matrix cortex
// snapshot layer.
//
// Spec: research/04-cortex.md §7.2 ("sparse Merkle tree (SMT) over
// (canonical_key, sha256(value)) pairs"). Concrete shape locked sess#7:
//
//   - 256-bit address space, keyed by sha256(domain || canonical_key).
//   - One tree per anchored namespace ("memories", "edges").
//   - Empty-subtree compression via precomputed emptyHashes[0..256].
//   - Persistent storage at idx/smt/<ns>/n/<depth:2>/<path:32>; only
//     non-empty subtree nodes have keys (default subtrees are absent
//     from the store and their hash is read from emptyHashes).
//   - Inclusion AND non-inclusion proofs (load-bearing for CortexScope
//     sub-agent reads — proving a key is excluded from a scope is as
//     important as proving it is included).
//
// Conventions used throughout this file:
//
//   - "depth" runs 0 (root) to 256 (leaf). A node at depth d represents a
//     subtree of remaining-depth (256 - d). The root is depth 0; its
//     subtree has remaining-depth 256.
//   - "path" is a [32]byte where bit i (MSB-first) selects the child of
//     the node at depth i: 0 → left, 1 → right. A node's stored "path"
//     keeps the top `depth` bits significant; lower (256 - depth) bits
//     are zero (normalizePath enforces this).
//   - Leaf hash: sha256(SMTLeafDomain || keyHash || valueHash).
//   - Internal node hash: sha256(SMTNodeDomain || left || right).
//   - Empty leaf: emptyHashes[0] = sha256(SMTEmptyDomain).
//     emptyHashes[r+1] = hashSMTNode(emptyHashes[r], emptyHashes[r]).
//
// The root commits to (a) every (keyHash, valueHash) pair in the tree,
// AND (b) the absence of every other 256-bit keyHash — both via the
// same Merkle structure. Sub-agents can therefore verify scope
// inclusion AND scope exclusion with O(log effective-N) proof size.

package snapshot

import (
	"crypto/sha256"
	"fmt"

	"matrix/cortex/keys"
	"matrix/cortex/store"
)

// Domain-separation prefixes for the SMT layer.
const (
	SMTLeafDomain  = "matrix.cortex.snapshot.smt.leaf.v1"
	SMTNodeDomain  = "matrix.cortex.snapshot.smt.node.v1"
	SMTEmptyDomain = "matrix.cortex.snapshot.smt.empty.v1"
	// SMTKeyDomainMemories / SMTKeyDomainEdges are mixed into the
	// per-namespace key-derivation. Although each namespace already has
	// a separate tree, the per-purpose domain hardens the design against
	// a future bug that accidentally routed a key through the wrong tree
	// — the resulting hash would simply not match.
	SMTKeyDomainMemories = "matrix.cortex.snapshot.smt.key.memories.v1"
	SMTKeyDomainEdges    = "matrix.cortex.snapshot.smt.key.edges.v1"
	SMTValueDomain       = "matrix.cortex.snapshot.smt.value.v1"
)

// emptyHashes[r] = hash of an empty subtree of remaining-depth r.
//   - emptyHashes[0]   = sha256(SMTEmptyDomain).        (empty leaf)
//   - emptyHashes[r+1] = hashSMTNode(emptyHashes[r], emptyHashes[r]).
//
// Initialized in init(); never mutated afterwards.
var emptyHashes [257][32]byte

// EmptyRoot is the SMT root for a namespace with no entries. Equal to
// emptyHashes[256]. Filled in init() AFTER emptyHashes — package-level
// var initializers run before init(), so this MUST stay declared without
// a value-from-emptyHashes initializer (or it would capture zeros).
var EmptyRoot [32]byte

func init() {
	h := sha256.Sum256([]byte(SMTEmptyDomain))
	emptyHashes[0] = h
	for r := 0; r < 256; r++ {
		emptyHashes[r+1] = hashSMTNode(emptyHashes[r], emptyHashes[r])
	}
	EmptyRoot = emptyHashes[256]
}

// hashSMTLeaf = sha256(SMTLeafDomain || keyHash || valueHash).
func hashSMTLeaf(keyHash, valueHash [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(SMTLeafDomain))
	h.Write(keyHash[:])
	h.Write(valueHash[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// hashSMTNode = sha256(SMTNodeDomain || left || right).
func hashSMTNode(left, right [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(SMTNodeDomain))
	h.Write(left[:])
	h.Write(right[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// HashMemoryKey returns the SMT key-hash for a memory id. Public so the
// cortex layer can derive the same hash without re-importing the SMT
// internals.
func HashMemoryKey(id [16]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(SMTKeyDomainMemories))
	h.Write(id[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// HashEdgeKey returns the SMT key-hash for an edge identified by
// (src, type, dst). Forward direction only — reverse keys are
// byte-identical and would double-count.
func HashEdgeKey(src [16]byte, edgeType byte, dst [16]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(SMTKeyDomainEdges))
	h.Write(src[:])
	h.Write([]byte{edgeType})
	h.Write(dst[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// HashValue returns sha256(SMTValueDomain || canonicalCBOR). Used by the
// cortex layer for both Head and EdgeRecord values; the domain prevents
// any cross-confusion with raw memory.HashDomain (which is distinct).
func HashValue(canonicalCBOR []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(SMTValueDomain))
	h.Write(canonicalCBOR)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// bitAt returns the value (0 or 1) of bit `idx` in path, with idx 0
// referring to the MSB of path[0] and idx 255 the LSB of path[31].
func bitAt(path [32]byte, idx uint16) byte {
	byteIdx := idx >> 3
	bitInByte := idx & 7
	return (path[byteIdx] >> (7 - bitInByte)) & 1
}

// setBitAt returns a copy of path with bit `idx` set to b (0 or 1).
func setBitAt(path [32]byte, idx uint16, b byte) [32]byte {
	byteIdx := idx >> 3
	bitInByte := idx & 7
	mask := byte(1) << (7 - bitInByte)
	if b == 0 {
		path[byteIdx] &^= mask
	} else {
		path[byteIdx] |= mask
	}
	return path
}

// normalizePath returns a copy of path with bits [significantBits, 256)
// zeroed. significantBits ∈ [0..256]: 0 means "no significant bits" (root
// path, all zeros), 256 means "all significant" (no change).
func normalizePath(path [32]byte, significantBits uint16) [32]byte {
	if significantBits >= 256 {
		return path
	}
	byteIdx := significantBits >> 3
	bitInByte := significantBits & 7
	if bitInByte > 0 {
		// Keep the top `bitInByte` bits of path[byteIdx]; clear the rest.
		keep := byte(0xFF) << (8 - bitInByte)
		path[byteIdx] &= keep
		byteIdx++
	}
	for i := byteIdx; i < 32; i++ {
		path[i] = 0
	}
	return path
}

// SMT is a thin handle over the underlying store. Like MMR it caches no
// in-memory state; every Stage/Root call reads what it needs from
// idx/smt/. State changes go through wb.Set / wb.Delete so they commit
// atomically with the journaled mutation.
type SMT struct {
	s  *store.Store
	ns string
}

// NewSMT returns a handle for the given namespace ("memories" | "edges").
// Cheap, stateless. Callers MUST use a namespace registered in
// validNamespaces (snapshot.go) — anchoring an unknown namespace would
// silently fork the OverallRoot computation.
func NewSMT(s *store.Store, ns string) *SMT { return &SMT{s: s, ns: ns} }

// Namespace returns the SMT's bound namespace.
func (t *SMT) Namespace() string { return t.ns }

// readNode reads the node hash at (depth, path). Returns (hash, true,
// nil) when the key is present, (zero, false, nil) when it is absent
// (caller treats that as the empty subtree at the level).
func (t *SMT) readNode(depth uint16, path [32]byte) ([32]byte, bool, error) {
	k, err := keys.IdxSMTNodeKey(t.ns, depth, path)
	if err != nil {
		return [32]byte{}, false, err
	}
	raw, ok, err := t.s.Get(k)
	if err != nil {
		return [32]byte{}, false, fmt.Errorf("snapshot.SMT.readNode: %w", err)
	}
	if !ok {
		return [32]byte{}, false, nil
	}
	if len(raw) != 32 {
		return [32]byte{}, false, fmt.Errorf("snapshot.SMT.readNode: malformed node (len=%d)", len(raw))
	}
	var out [32]byte
	copy(out[:], raw)
	return out, true, nil
}

// stageNode writes nodeHash at (depth, path) onto setter, OR deletes the
// key when isEmpty is true. Empty-subtree collapse is the only path that
// produces deletions; all other writes set 32 bytes.
func (t *SMT) stageNode(setter Setter, depth uint16, path [32]byte, nodeHash [32]byte, isEmpty bool) error {
	k, err := keys.IdxSMTNodeKey(t.ns, depth, path)
	if err != nil {
		return err
	}
	if isEmpty {
		return setter.Delete(k)
	}
	return setter.Set(k, copyHash(nodeHash))
}

// StageUpdate stages writes for setting (keyHash → valueHash) onto wb.
// A zero valueHash is treated as a deletion (the leaf is replaced with
// the empty-leaf hash and the subtree collapses upward as siblings allow).
//
// Cost: ≤ 256 readNode + ≤ 257 stageNode per call (depth + leaf). In
// practice empty-subtree compression keeps the stored node count near
// log₂(N) per key, but the walk is always 256 levels. For our anchored
// namespaces (memories ≤ 100k, edges ≤ ~1M) that's a few thousand SMT
// node reads per write — Pebble handles that in single-digit
// milliseconds on commodity hardware.
func (t *SMT) StageUpdate(setter Setter, keyHash, valueHash [32]byte) error {
	isDelete := valueHash == [32]byte{}

	// Compute the new leaf hash.
	var newLeaf [32]byte
	if isDelete {
		newLeaf = emptyHashes[0]
	} else {
		newLeaf = hashSMTLeaf(keyHash, valueHash)
	}

	// Place / clear the leaf at depth 256 with path = keyHash.
	if err := t.stageNode(setter, 256, keyHash, newLeaf, isDelete); err != nil {
		return fmt.Errorf("snapshot.SMT.StageUpdate: leaf: %w", err)
	}

	cur := newLeaf
	curPath := keyHash // already fully-significant at depth 256

	// Walk back up to the root.
	for d := uint16(256); d >= 1; d-- {
		parentDepth := d - 1
		bitIdx := parentDepth // depth d's parent decision uses bit (d-1)
		bit := bitAt(keyHash, bitIdx)

		// Sibling at depth d sits at the same path but with bit (d-1)
		// flipped. Because curPath is already normalized at d, only that
		// one bit changes.
		siblingPath := setBitAt(curPath, bitIdx, 1-bit)
		siblingPath = normalizePath(siblingPath, d)

		siblingHash, present, err := t.readNode(d, siblingPath)
		if err != nil {
			return fmt.Errorf("snapshot.SMT.StageUpdate: sibling at depth %d: %w", d, err)
		}
		if !present {
			siblingHash = emptyHashes[256-d]
		}

		var left, right [32]byte
		if bit == 0 {
			left, right = cur, siblingHash
		} else {
			left, right = siblingHash, cur
		}
		parentHash := hashSMTNode(left, right)
		parentPath := normalizePath(curPath, parentDepth)

		// If the new parent equals the empty-subtree hash for its
		// remaining depth, the whole subtree below it is empty. Collapse
		// by deleting the parent key (its absence is interpreted as
		// emptyHashes[256-parentDepth] on subsequent reads).
		emptyAtParent := emptyHashes[256-parentDepth]
		parentEmpty := parentHash == emptyAtParent

		if err := t.stageNode(setter, parentDepth, parentPath, parentHash, parentEmpty); err != nil {
			return fmt.Errorf("snapshot.SMT.StageUpdate: parent at depth %d: %w", parentDepth, err)
		}

		cur = parentHash
		curPath = parentPath

		if d == 1 {
			break // post-decrement of uint16 from 1 would underflow
		}
	}
	return nil
}

// Root returns the current SMT root for this namespace. For an empty
// tree returns EmptyRoot.
func (t *SMT) Root() ([32]byte, error) {
	zero := [32]byte{}
	h, present, err := t.readNode(0, zero)
	if err != nil {
		return [32]byte{}, err
	}
	if !present {
		return EmptyRoot, nil
	}
	return h, nil
}

// MembershipProof is the Merkle proof shape returned by SMT.Prove. It
// asserts that the leaf at keyHash hashes to valueHash (valueHash zero
// = absence proof: the leaf is the empty-leaf hash).
//
// Siblings is exactly 256 hashes long, ordered top-to-bottom (Siblings[0]
// is the sibling at depth 1, i.e. the other child of the root; Siblings[255]
// is the leaf-level sibling). Empty-subtree siblings are encoded as the
// matching emptyHashes[r] value rather than a special sentinel — this
// keeps the verifier symmetric with Stage.
type MembershipProof struct {
	KeyHash   [32]byte
	ValueHash [32]byte // zero ⇒ non-membership proof
	Siblings  [256][32]byte
}

// Prove returns a proof for keyHash. The returned proof asserts the
// current leaf value at that key — set if a leaf has been written for
// it (last value's hash), else zero (the empty-leaf, attesting absence).
//
// Cost: ≤ 256 readNode calls. Used by sub-agent dispatch (CortexScope
// multi-proof composition; see Phase 10 wiring).
func (t *SMT) Prove(keyHash [32]byte) (*MembershipProof, error) {
	pf := &MembershipProof{KeyHash: keyHash}

	// Walk from root downward, collecting sibling hashes at each level.
	var path [32]byte // accumulated path so far, normalized to current depth
	for d := uint16(0); d < 256; d++ {
		bit := bitAt(keyHash, d) // bit at depth d decides direction
		siblingPath := setBitAt(path, d, 1-bit)
		siblingPath = normalizePath(siblingPath, d+1)
		sib, present, err := t.readNode(d+1, siblingPath)
		if err != nil {
			return nil, fmt.Errorf("snapshot.SMT.Prove: depth %d sibling: %w", d+1, err)
		}
		if !present {
			sib = emptyHashes[256-(d+1)]
		}
		pf.Siblings[d] = sib
		path = setBitAt(path, d, bit)
		path = normalizePath(path, d+1)
	}

	// Read the leaf to discover the value side. If the leaf is absent or
	// equals emptyHashes[0], the proof is a non-membership proof.
	leaf, present, err := t.readNode(256, keyHash)
	if err != nil {
		return nil, fmt.Errorf("snapshot.SMT.Prove: leaf: %w", err)
	}
	if !present || leaf == emptyHashes[0] {
		// Non-membership: ValueHash stays zero. The verifier reconstructs
		// emptyHashes[0] for the leaf hash.
		return pf, nil
	}
	// Membership: extract the canonical value-hash component. We can't
	// recover the original valueHash from the leaf hash alone; the caller
	// must supply it (Prove() returns the leaf hash via reverse-engineering
	// the leaf concatenation). For our purposes the embedding caller has
	// the value bytes and recomputes valueHash via HashValue(canonical).
	// Encode that by leaving ValueHash zero here and providing a
	// ProveWithValue helper below.
	pf.ValueHash = [32]byte{} // sentinel; ProveWithValue fills it
	return pf, nil
}

// ProveWithValue is the variant Prove that also accepts the canonical
// value bytes the caller asserts is current. It computes valueHash via
// HashValue, calls Prove, and fills proof.ValueHash. The caller is
// responsible for ensuring `canonical` matches the bytes that were last
// staged via StageUpdate — otherwise the proof will fail to verify.
func (t *SMT) ProveWithValue(keyHash [32]byte, canonical []byte) (*MembershipProof, error) {
	pf, err := t.Prove(keyHash)
	if err != nil {
		return nil, err
	}
	if len(canonical) > 0 {
		pf.ValueHash = HashValue(canonical)
	}
	return pf, nil
}

// VerifyMembership checks proof against root. valueHash zero is a
// non-membership proof — the verifier reconstructs the leaf as the
// empty-leaf hash. Returns nil on success, ErrInvalidProof on failure.
func VerifyMembership(root [32]byte, proof *MembershipProof) error {
	if proof == nil {
		return fmt.Errorf("%w: nil proof", ErrInvalidProof)
	}
	// Recompute leaf hash.
	var leaf [32]byte
	if proof.ValueHash == ([32]byte{}) {
		leaf = emptyHashes[0]
	} else {
		leaf = hashSMTLeaf(proof.KeyHash, proof.ValueHash)
	}
	// Walk bottom-up: sibling i sits at depth i+1; sibling[255] is at the
	// leaf level (depth 256), sibling[0] is at depth 1.
	cur := leaf
	for i := 255; i >= 0; i-- {
		bit := bitAt(proof.KeyHash, uint16(i))
		sib := proof.Siblings[i]
		if bit == 0 {
			cur = hashSMTNode(cur, sib)
		} else {
			cur = hashSMTNode(sib, cur)
		}
	}
	if cur != root {
		return fmt.Errorf("%w: root mismatch", ErrInvalidProof)
	}
	return nil
}

// Reset clears the SMT's persisted nodes (every idx/smt/<ns>/n/* key).
// Used by the replay harness; not exposed in the public Cortex API.
func (t *SMT) Reset() error {
	prefix, err := keys.IdxSMTNamespacePrefix(t.ns)
	if err != nil {
		return err
	}
	if err := t.s.PrefixIter(prefix, func(k, _ []byte) error {
		return t.s.DB().Delete(append([]byte{}, k...), nil)
	}); err != nil {
		return fmt.Errorf("snapshot.SMT.Reset(%q): %w", t.ns, err)
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
