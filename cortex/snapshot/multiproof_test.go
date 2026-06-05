// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package snapshot

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

// buildStateWithMemories applies n memory updates against a fresh State
// using direct Pebble batches (no JournalHook — these tests focus on
// MultiProof shape only). Returns the State plus the per-memory ids and
// canonical value bytes in insertion order.
func buildStateWithMemories(t *testing.T, name string, n int) (*State, [][16]byte, [][]byte) {
	t.Helper()
	s := openStoreSMT(t, name)
	st := New(s)
	ids := make([][16]byte, n)
	vals := make([][]byte, n)
	for i := 0; i < n; i++ {
		var id [16]byte
		// 16 bytes: "mem-" (4) + "%012d" (12) = 16 exactly. Earlier
		// formats overflowed and got truncated — producing duplicate
		// ids that collapsed the SMT to a single leaf.
		copy(id[:], fmt.Sprintf("mem-%012d", i))
		val := []byte(fmt.Sprintf("canonical-head-%d", i))
		ids[i] = id
		vals[i] = val

		b := s.DB().NewBatch()
		setter := NewPebbleBatchSetter(b)
		if err := st.StageMemoryUpdate(setter, id, val); err != nil {
			t.Fatalf("StageMemoryUpdate(%d): %v", i, err)
		}
		if err := b.Commit(nil); err != nil {
			t.Fatalf("Commit(%d): %v", i, err)
		}
	}
	return st, ids, vals
}

// itemsForAll builds MultiProofItems for every (id, val) pair returned
// by buildStateWithMemories.
func itemsForAll(ids [][16]byte, vals [][]byte) []MultiProofItem {
	items := make([]MultiProofItem, len(ids))
	for i := range ids {
		items[i] = MultiProofItem{KeyHash: HashMemoryKey(ids[i]), Canonical: vals[i]}
	}
	return items
}

func TestMultiProofBuildAndVerify(t *testing.T) {
	st, ids, vals := buildStateWithMemories(t, "mp-build-verify", 5)

	mp, err := st.BuildMultiProofWithValues("memories", itemsForAll(ids, vals))
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}
	if mp.SchemaVersion != MultiProofSchemaVersion {
		t.Errorf("SchemaVersion=%d want %d", mp.SchemaVersion, MultiProofSchemaVersion)
	}
	if mp.Namespace != "memories" {
		t.Errorf("Namespace=%q want memories", mp.Namespace)
	}
	if len(mp.Proofs) != len(ids) {
		t.Errorf("Proofs len=%d want %d", len(mp.Proofs), len(ids))
	}
	if err := mp.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestMultiProofVerifyAgainstManifest(t *testing.T) {
	st, ids, vals := buildStateWithMemories(t, "mp-vam", 3)

	mp, err := st.BuildMultiProofWithValues("memories", itemsForAll(ids, vals))
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}

	now := time.Unix(1700000000, 0).UTC()
	m, err := st.Snapshot("test-actor", TriggerExplicit, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := mp.VerifyAgainstManifest(m); err != nil {
		t.Errorf("VerifyAgainstManifest: %v", err)
	}
}

func TestMultiProofRejectsTamperedRoot(t *testing.T) {
	st, ids, vals := buildStateWithMemories(t, "mp-tamper-root", 2)
	mp, err := st.BuildMultiProofWithValues("memories", itemsForAll(ids, vals))
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}
	mp.Root = sha256.Sum256([]byte("evil root"))
	if err := mp.Verify(); err == nil {
		t.Errorf("Verify accepted tampered root")
	}
}

func TestMultiProofRejectsTamperedValue(t *testing.T) {
	st, ids, vals := buildStateWithMemories(t, "mp-tamper-val", 2)
	mp, err := st.BuildMultiProofWithValues("memories", itemsForAll(ids, vals))
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}
	mp.Proofs[1].ValueHash = sha256.Sum256([]byte("wrong value"))
	if err := mp.Verify(); err == nil {
		t.Errorf("Verify accepted tampered ValueHash")
	}
}

func TestMultiProofNonMembership(t *testing.T) {
	st, _, _ := buildStateWithMemories(t, "mp-nonmember", 3)

	var ghost [16]byte
	copy(ghost[:], "ghost-memory-id")
	items := []MultiProofItem{
		{KeyHash: HashMemoryKey(ghost), Canonical: nil},
	}
	mp, err := st.BuildMultiProofWithValues("memories", items)
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}
	if mp.Proofs[0].ValueHash != ([32]byte{}) {
		t.Errorf("non-membership ValueHash should be zero, got non-zero")
	}
	if err := mp.Verify(); err != nil {
		t.Errorf("Verify(non-membership): %v", err)
	}
}

func TestMultiProofUnknownNamespace(t *testing.T) {
	s := openStoreSMT(t, "mp-unknown-ns")
	st := New(s)
	_, err := st.BuildMultiProof("nonexistent", [][32]byte{HashMemoryKey([16]byte{})})
	if err == nil {
		t.Fatalf("BuildMultiProof accepted unknown namespace")
	}
}

func TestMultiProofManifestNamespaceMismatch(t *testing.T) {
	st, ids, vals := buildStateWithMemories(t, "mp-ns-mismatch", 2)
	mp, err := st.BuildMultiProofWithValues("memories", itemsForAll(ids, vals))
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}
	forged := &Manifest{
		SchemaVersion: SchemaVersion,
		Actor:         "test",
		StateRoots:    map[string][32]byte{"edges": mp.Root},
	}
	if err := mp.VerifyAgainstManifest(forged); err == nil {
		t.Errorf("VerifyAgainstManifest accepted manifest with no matching namespace")
	}
}

func TestMultiProofManifestRootMismatch(t *testing.T) {
	st, ids, vals := buildStateWithMemories(t, "mp-root-mismatch", 2)
	mp, err := st.BuildMultiProofWithValues("memories", itemsForAll(ids, vals))
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}
	wrong := sha256.Sum256([]byte("not-the-real-root"))
	forged := &Manifest{
		SchemaVersion: SchemaVersion,
		Actor:         "test",
		StateRoots:    map[string][32]byte{"memories": wrong},
	}
	if err := mp.VerifyAgainstManifest(forged); err == nil {
		t.Errorf("VerifyAgainstManifest accepted root mismatch")
	}
}

func TestMultiProofEncodeDecodeRoundTrip(t *testing.T) {
	st, ids, vals := buildStateWithMemories(t, "mp-enc-dec", 4)
	mp, err := st.BuildMultiProofWithValues("memories", itemsForAll(ids, vals))
	if err != nil {
		t.Fatalf("BuildMultiProofWithValues: %v", err)
	}
	enc, err := EncodeMultiProof(mp)
	if err != nil {
		t.Fatalf("EncodeMultiProof: %v", err)
	}
	var dec MultiProof
	if err := DecodeMultiProof(enc, &dec); err != nil {
		t.Fatalf("DecodeMultiProof: %v", err)
	}
	if dec.SchemaVersion != mp.SchemaVersion ||
		dec.Namespace != mp.Namespace ||
		dec.Root != mp.Root ||
		len(dec.Proofs) != len(mp.Proofs) {
		t.Errorf("round-trip mismatch: got %+v want %+v", dec, *mp)
	}
	if err := dec.Verify(); err != nil {
		t.Errorf("Verify(decoded): %v", err)
	}
}

func TestMultiProofWrongSchemaVersionRejected(t *testing.T) {
	mp := &MultiProof{
		SchemaVersion: 99,
		Namespace:     "memories",
		Root:          [32]byte{1, 2, 3},
		Proofs:        nil,
	}
	if err := mp.Verify(); err == nil {
		t.Errorf("Verify accepted unknown schema version")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
