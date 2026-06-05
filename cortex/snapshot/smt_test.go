// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"

	"matrix/cortex/journal"
	"matrix/cortex/store"
)

// keyHashFor returns a deterministic 32-byte key-hash for test index i.
func keyHashFor(i uint64) [32]byte {
	var buf [16]byte
	copy(buf[:], "matrix.test.key.")
	binary.BigEndian.PutUint64(buf[8:], i)
	return sha256.Sum256(buf[:])
}

// valueHashFor returns a deterministic 32-byte value-hash for test index i.
func valueHashFor(i uint64) [32]byte {
	var buf [16]byte
	copy(buf[:], "matrix.test.val.")
	binary.BigEndian.PutUint64(buf[8:], i)
	return sha256.Sum256(buf[:])
}

func openStoreSMT(t *testing.T, actor string) *store.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "matrix-smt-test")
	s, err := store.Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// applyUpdate is a test helper that wraps StageUpdate in a journaled batch.
func applyUpdate(t *testing.T, s *store.Store, smt *SMT, keyHash, valueHash [32]byte) {
	t.Helper()
	wb := s.BeginWrite()
	if err := smt.StageUpdate(wb, keyHash, valueHash); err != nil {
		wb.Abort()
		t.Fatalf("StageUpdate: %v", err)
	}
	entry := &journal.Entry{Kind: journal.KindRaw, Payload: keyHash[:8]}
	if err := wb.AppendJournal(entry); err != nil {
		wb.Abort()
		t.Fatalf("AppendJournal: %v", err)
	}
	if err := wb.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestSMTEmptyTreeHasEmptyRoot(t *testing.T) {
	s := openStoreSMT(t, "smtA")
	smt := NewSMT(s, "memories")
	got, err := smt.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != EmptyRoot {
		t.Errorf("Empty Root = %x, want EmptyRoot %x", got[:8], EmptyRoot[:8])
	}
}

func TestSMTSingleInsertChangesRoot(t *testing.T) {
	s := openStoreSMT(t, "smtB")
	smt := NewSMT(s, "memories")
	applyUpdate(t, s, smt, keyHashFor(1), valueHashFor(1))
	got, err := smt.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got == EmptyRoot {
		t.Errorf("Root unchanged after insert")
	}
}

func TestSMTInsertOrderIndependent(t *testing.T) {
	// Two trees, same key set inserted in different orders → same root.
	s1 := openStoreSMT(t, "ord1")
	s2 := openStoreSMT(t, "ord2")
	smt1 := NewSMT(s1, "memories")
	smt2 := NewSMT(s2, "memories")

	for _, i := range []uint64{1, 2, 3, 4, 5} {
		applyUpdate(t, s1, smt1, keyHashFor(i), valueHashFor(i))
	}
	for _, i := range []uint64{5, 3, 1, 4, 2} {
		applyUpdate(t, s2, smt2, keyHashFor(i), valueHashFor(i))
	}
	r1, _ := smt1.Root()
	r2, _ := smt2.Root()
	if r1 != r2 {
		t.Errorf("SMT root depends on insertion order:\n got1 %x\n got2 %x", r1[:8], r2[:8])
	}
}

func TestSMTUpdateChangesRoot(t *testing.T) {
	s := openStoreSMT(t, "upd")
	smt := NewSMT(s, "memories")
	applyUpdate(t, s, smt, keyHashFor(1), valueHashFor(1))
	r1, _ := smt.Root()
	applyUpdate(t, s, smt, keyHashFor(1), valueHashFor(2))
	r2, _ := smt.Root()
	if r1 == r2 {
		t.Errorf("Root unchanged after value update")
	}
}

func TestSMTDeleteRestoresEarlierRoot(t *testing.T) {
	s := openStoreSMT(t, "del")
	smt := NewSMT(s, "memories")
	applyUpdate(t, s, smt, keyHashFor(1), valueHashFor(1))
	r1, _ := smt.Root()
	applyUpdate(t, s, smt, keyHashFor(2), valueHashFor(2))
	applyUpdate(t, s, smt, keyHashFor(2), [32]byte{}) // delete
	rRestored, _ := smt.Root()
	if r1 != rRestored {
		t.Errorf("Delete did not restore the prior root:\n want %x\n got  %x", r1[:8], rRestored[:8])
	}
}

func TestSMTAllDeletesEqualEmptyRoot(t *testing.T) {
	s := openStoreSMT(t, "del2")
	smt := NewSMT(s, "memories")
	for i := uint64(1); i <= 5; i++ {
		applyUpdate(t, s, smt, keyHashFor(i), valueHashFor(i))
	}
	for i := uint64(1); i <= 5; i++ {
		applyUpdate(t, s, smt, keyHashFor(i), [32]byte{})
	}
	r, _ := smt.Root()
	if r != EmptyRoot {
		t.Errorf("After deleting all keys, Root != EmptyRoot")
	}
}

func TestSMTMembershipProofVerifies(t *testing.T) {
	s := openStoreSMT(t, "proof")
	smt := NewSMT(s, "memories")
	keys := make([][32]byte, 8)
	vals := make([][32]byte, 8)
	for i := uint64(0); i < 8; i++ {
		keys[i] = keyHashFor(i + 1)
		vals[i] = valueHashFor(i + 1)
		applyUpdate(t, s, smt, keys[i], vals[i])
	}
	root, _ := smt.Root()
	for i := range keys {
		pf, err := smt.Prove(keys[i])
		if err != nil {
			t.Fatalf("Prove(%d): %v", i, err)
		}
		pf.ValueHash = vals[i]
		if err := VerifyMembership(root, pf); err != nil {
			t.Errorf("VerifyMembership(%d): %v", i, err)
		}
	}
}

func TestSMTNonMembershipProofVerifies(t *testing.T) {
	s := openStoreSMT(t, "nonmem")
	smt := NewSMT(s, "memories")
	for i := uint64(1); i <= 5; i++ {
		applyUpdate(t, s, smt, keyHashFor(i), valueHashFor(i))
	}
	root, _ := smt.Root()
	// keyHashFor(99) was never inserted.
	pf, err := smt.Prove(keyHashFor(99))
	if err != nil {
		t.Fatalf("Prove(99): %v", err)
	}
	if pf.ValueHash != ([32]byte{}) {
		t.Errorf("non-membership proof has non-zero ValueHash")
	}
	if err := VerifyMembership(root, pf); err != nil {
		t.Errorf("VerifyMembership(non-member): %v", err)
	}
}

func TestSMTProofRejectsWrongValue(t *testing.T) {
	s := openStoreSMT(t, "wrongval")
	smt := NewSMT(s, "memories")
	applyUpdate(t, s, smt, keyHashFor(1), valueHashFor(1))
	root, _ := smt.Root()
	pf, _ := smt.Prove(keyHashFor(1))
	pf.ValueHash = valueHashFor(99) // tampered
	if err := VerifyMembership(root, pf); err == nil {
		t.Errorf("VerifyMembership accepted tampered ValueHash")
	}
}

func TestSMTProofRejectsWrongRoot(t *testing.T) {
	s := openStoreSMT(t, "wrongroot")
	smt := NewSMT(s, "memories")
	applyUpdate(t, s, smt, keyHashFor(1), valueHashFor(1))
	pf, _ := smt.Prove(keyHashFor(1))
	pf.ValueHash = valueHashFor(1)
	wrongRoot := sha256.Sum256([]byte("not the real root"))
	if err := VerifyMembership(wrongRoot, pf); err == nil {
		t.Errorf("VerifyMembership accepted wrong root")
	}
}

func TestSMTNamespacesIsolated(t *testing.T) {
	s := openStoreSMT(t, "iso")
	memSMT := NewSMT(s, "memories")
	edgeSMT := NewSMT(s, "edges")
	applyUpdate(t, s, memSMT, keyHashFor(1), valueHashFor(1))
	if r, _ := edgeSMT.Root(); r != EmptyRoot {
		t.Errorf("edges SMT polluted by memories write")
	}
	applyUpdate(t, s, edgeSMT, keyHashFor(2), valueHashFor(2))
	r1, _ := memSMT.Root()
	r2, _ := edgeSMT.Root()
	if r1 == r2 {
		t.Errorf("Distinct namespace inputs produce equal roots")
	}
}

func TestSMTNormalizePath(t *testing.T) {
	full := [32]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	for i := range full[8:] {
		full[8+i] = 0xFF
	}
	cases := []struct {
		bits    uint16
		want    [32]byte
		comment string
	}{
		{0, [32]byte{}, "no significant bits → all zero"},
		{8, [32]byte{0xFF}, "8 bits → top byte preserved"},
		{4, [32]byte{0xF0}, "4 bits → top nibble preserved"},
		{256, full, "all bits → unchanged"},
	}
	for _, c := range cases {
		got := normalizePath(full, c.bits)
		if got != c.want {
			t.Errorf("normalizePath(0xFF×32, %d): %s\n got  %x\n want %x", c.bits, c.comment, got[:], c.want[:])
		}
	}
}

func TestSMTBitAt(t *testing.T) {
	// Path where bit 0 = 1, bit 1 = 0, bit 7 = 0, bit 8 = 1, ...
	// path[0] = 0b1000_0000 → bit 0 = 1
	// path[1] = 0b1100_0000 → bit 8 = 1, bit 9 = 1
	var p [32]byte
	p[0] = 0x80
	p[1] = 0xC0
	if bitAt(p, 0) != 1 {
		t.Errorf("bitAt(0) wrong")
	}
	if bitAt(p, 1) != 0 {
		t.Errorf("bitAt(1) wrong")
	}
	if bitAt(p, 8) != 1 {
		t.Errorf("bitAt(8) wrong")
	}
	if bitAt(p, 9) != 1 {
		t.Errorf("bitAt(9) wrong")
	}
	if bitAt(p, 10) != 0 {
		t.Errorf("bitAt(10) wrong")
	}
}

func TestSMTEmptyHashesRecursion(t *testing.T) {
	// emptyHashes[r+1] must equal hashSMTNode(emptyHashes[r], emptyHashes[r])
	// for every r ∈ [0, 256). Verifies the init-time precomputation.
	for r := 0; r < 256; r++ {
		want := hashSMTNode(emptyHashes[r], emptyHashes[r])
		if emptyHashes[r+1] != want {
			t.Errorf("emptyHashes[%d] != hashSMTNode(emptyHashes[%d], emptyHashes[%d])", r+1, r, r)
		}
	}
}

func TestSMTKeyDomainsDistinct(t *testing.T) {
	var id [16]byte
	id[0] = 1
	memKey := HashMemoryKey(id)
	edgeKey := HashEdgeKey(id, 0x01, [16]byte{2})
	if memKey == edgeKey {
		t.Errorf("memory and edge key domains collide")
	}
}

// hashHex is a tiny test helper to print failure context.
func hashHex(h [32]byte) string { return fmt.Sprintf("%x", h[:8]) }

// Sanity: ensure imports stay used after future refactors.
var _ = bytes.Equal

// Copyright © 2026 Paxlabs Inc. All rights reserved.
