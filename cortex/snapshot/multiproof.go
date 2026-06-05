// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Multi-key Merkle proof composition for sub-agent CortexScope dispatch.
//
// Spec: research/04-cortex.md §7.5 ("multi-proof over the include keys
// against the SMT") + Phase 7 deferral note in matrix.ctx phase7_deferrals
// ("multi-proof composition for sub-agent CortexScope → phase 10
// (single-key Prove + ProveWithValue ship now; bundling is mechanical)").
//
// v1 shape: a flat list of single-key MembershipProofs against a shared
// SMT root. We deliberately do NOT do RFC9162-style sibling sharing /
// path compression in v1 — the Phase 7 SMT is a plain shape, the gain
// is small (~30% bytes for tens of keys), and a future bundle format
// can land behind a SchemaVersion bump without changing the cortex API.
//
// Self-containedness: MultiProof embeds Namespace + Root so a sub-agent
// can verify the bundle without re-fetching the snapshot manifest.
// VerifyAgainstManifest re-checks that Root matches the named state root
// in the supplied manifest (the bundle is signed/anchored together with
// the manifest at the dispatch boundary).

package snapshot

import (
	"errors"
	"fmt"
)

// MultiProofSchemaVersion is the wire-format schema for MultiProof.
// Bumped on any shape change that would alter encode/decode bytes;
// downstream consumers (sub-agent verifiers, tools/attest envelopes)
// MUST gate on this.
const MultiProofSchemaVersion uint8 = 1

// MultiProof bundles N MembershipProofs against the same SMT root. Each
// proof verifies independently; the bundle's job is to (a) name the
// namespace, (b) pin the root the proofs were built against, and (c)
// carry the proofs together over the wire.
//
// Encoded as canonical CBOR with integer keys (matches Manifest +
// EdgeRecord style). CBOR keys are stable across schema versions; new
// fields land at unused integer keys, deletions are forbidden without
// a SchemaVersion bump.
type MultiProof struct {
	SchemaVersion uint8             `cbor:"0,keyasint"`
	Namespace     string            `cbor:"1,keyasint"`
	Root          [32]byte          `cbor:"2,keyasint"`
	Proofs        []MembershipProof `cbor:"3,keyasint"`
}

// MultiProofItem pairs a key with its canonical value bytes (or nil for
// non-membership). Used by BuildMultiProofWithValues so the caller
// supplies "what should be at this key" alongside the key itself.
type MultiProofItem struct {
	KeyHash   [32]byte
	Canonical []byte // nil ⇒ non-membership proof (assert key is absent)
}

// ErrNamespaceMismatch is returned by BuildMultiProof / Verify variants
// when the requested namespace is not one of AnchoredNamespaces (or, on
// verify, when MultiProof.Namespace doesn't match the manifest's
// state-roots map).
var ErrNamespaceMismatch = errors.New("snapshot: namespace mismatch")

// EncodeMultiProof returns canonical CBOR bytes for mp. Same encoder as
// Manifest, so determinism rules are identical.
func EncodeMultiProof(mp *MultiProof) ([]byte, error) {
	if mp == nil {
		return nil, errors.New("snapshot: nil MultiProof")
	}
	return canonicalEnc.Marshal(mp)
}

// DecodeMultiProof parses canonical CBOR into out.
func DecodeMultiProof(b []byte, out *MultiProof) error {
	return canonicalDec.Unmarshal(b, out)
}

// BuildMultiProof composes one MembershipProof per keyHash against the
// namespace SMT at its current root. ValueHash fields stay zero — use
// BuildMultiProofWithValues when canonical value bytes are available
// (almost always the case, since the parent agent owns the cortex it's
// granting scope into).
//
// O(K · 256) Pebble reads where K = len(keyHashes). For typical scope
// sizes (≤32 keys) this is microseconds.
func (st *State) BuildMultiProof(namespace string, keyHashes [][32]byte) (*MultiProof, error) {
	smt := st.SMT(namespace)
	if smt == nil {
		return nil, fmt.Errorf("%w: %q (anchored: %v)", ErrUnknownNamespace, namespace, AnchoredNamespaces)
	}
	root, err := smt.Root()
	if err != nil {
		return nil, fmt.Errorf("snapshot.BuildMultiProof: root: %w", err)
	}
	proofs := make([]MembershipProof, 0, len(keyHashes))
	for i, kh := range keyHashes {
		pf, err := smt.Prove(kh)
		if err != nil {
			return nil, fmt.Errorf("snapshot.BuildMultiProof: key %d: %w", i, err)
		}
		proofs = append(proofs, *pf)
	}
	return &MultiProof{
		SchemaVersion: MultiProofSchemaVersion,
		Namespace:     namespace,
		Root:          root,
		Proofs:        proofs,
	}, nil
}

// BuildMultiProofWithValues is the variant that fills ValueHash from
// canonical value bytes per item. Pass Canonical=nil to assert non-
// membership (proof.ValueHash stays zero).
//
// IMPORTANT: the caller must ensure each Canonical matches the bytes
// that were last staged via StageMemoryUpdate / StageEdgeUpdate for
// that key. Stale bytes produce proofs that fail to verify against
// the current root.
func (st *State) BuildMultiProofWithValues(namespace string, items []MultiProofItem) (*MultiProof, error) {
	smt := st.SMT(namespace)
	if smt == nil {
		return nil, fmt.Errorf("%w: %q (anchored: %v)", ErrUnknownNamespace, namespace, AnchoredNamespaces)
	}
	root, err := smt.Root()
	if err != nil {
		return nil, fmt.Errorf("snapshot.BuildMultiProofWithValues: root: %w", err)
	}
	proofs := make([]MembershipProof, 0, len(items))
	for i, it := range items {
		var pf *MembershipProof
		if len(it.Canonical) == 0 {
			pf, err = smt.Prove(it.KeyHash)
		} else {
			pf, err = smt.ProveWithValue(it.KeyHash, it.Canonical)
		}
		if err != nil {
			return nil, fmt.Errorf("snapshot.BuildMultiProofWithValues: key %d: %w", i, err)
		}
		proofs = append(proofs, *pf)
	}
	return &MultiProof{
		SchemaVersion: MultiProofSchemaVersion,
		Namespace:     namespace,
		Root:          root,
		Proofs:        proofs,
	}, nil
}

// Verify checks every proof in mp against mp.Root via VerifyMembership.
// Returns the first failure with key index in the error chain.
//
// Self-contained: does NOT cross-check mp.Root against a snapshot
// manifest. Pair with VerifyAgainstManifest when the bundle ships with
// a manifest (the standard sub-agent dispatch path).
func (mp *MultiProof) Verify() error {
	if mp == nil {
		return fmt.Errorf("%w: nil MultiProof", ErrInvalidProof)
	}
	if mp.SchemaVersion != MultiProofSchemaVersion {
		return fmt.Errorf("%w: unknown MultiProof schema %d", ErrInvalidProof, mp.SchemaVersion)
	}
	for i := range mp.Proofs {
		if err := VerifyMembership(mp.Root, &mp.Proofs[i]); err != nil {
			return fmt.Errorf("snapshot.MultiProof.Verify: proof %d: %w", i, err)
		}
	}
	return nil
}

// VerifyAgainstManifest is the convenience verifier for the standard
// sub-agent dispatch path: it cross-checks mp.Root against
// manifest.StateRoots[mp.Namespace] before calling mp.Verify. This is
// the only verification path that establishes "the proven keys are
// proven against the snapshot the parent claims" — without it, a
// compromised parent could ship a MultiProof whose Root is unrelated
// to the manifest it was bundled with.
func (mp *MultiProof) VerifyAgainstManifest(m *Manifest) error {
	if mp == nil {
		return fmt.Errorf("%w: nil MultiProof", ErrInvalidProof)
	}
	if m == nil {
		return fmt.Errorf("%w: nil Manifest", ErrInvalidProof)
	}
	want, ok := m.StateRoots[mp.Namespace]
	if !ok {
		return fmt.Errorf("%w: manifest has no state root for namespace %q", ErrNamespaceMismatch, mp.Namespace)
	}
	if want != mp.Root {
		return fmt.Errorf("%w: manifest root != proof root for namespace %q", ErrInvalidProof, mp.Namespace)
	}
	return mp.Verify()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
