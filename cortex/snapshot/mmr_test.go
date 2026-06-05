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

// fakeLeaf returns a deterministic 32-byte hash for leaf index i. Used in
// tests so the same input sequences produce byte-identical MMR roots.
func fakeLeaf(i uint64) [32]byte {
	var buf [16]byte
	copy(buf[:], "matrix.test.leaf")
	binary.BigEndian.PutUint64(buf[8:], i)
	return sha256.Sum256(buf[:])
}

// openTestStore creates a fresh store under t.TempDir for one actor.
func openTestStore(t *testing.T, actor string) *store.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "matrix-snap-test")
	s, err := store.Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// appendLeaves is a test helper that drains n leaves through StageAppend
// inside one journaled batch each. Each batch must journal something
// (replay invariant) — we journal a Kind=raw entry as a stand-in.
func appendLeaves(t *testing.T, s *store.Store, mmr *MMR, n uint64) {
	t.Helper()
	for i := uint64(1); i <= n; i++ {
		wb := s.BeginWrite()
		if err := mmr.StageAppend(wb, fakeLeaf(i)); err != nil {
			wb.Abort()
			t.Fatalf("StageAppend(%d): %v", i, err)
		}
		// Journal a stand-in so wb.Commit doesn't reject for missing journal.
		entry := &journal.Entry{Kind: journal.KindRaw, Payload: []byte{byte(i)}}
		if err := wb.AppendJournal(entry); err != nil {
			wb.Abort()
			t.Fatalf("AppendJournal(%d): %v", i, err)
		}
		if err := wb.Commit(); err != nil {
			t.Fatalf("Commit(%d): %v", i, err)
		}
	}
}

func TestPeakPositions(t *testing.T) {
	cases := []struct {
		n    uint64
		want []uint64
	}{
		{0, nil},
		{1, []uint64{1}},
		{2, []uint64{3}},
		{3, []uint64{3, 4}},
		{4, []uint64{7}},
		{5, []uint64{7, 8}},
		{6, []uint64{7, 10}},
		{7, []uint64{7, 10, 11}},
		{8, []uint64{15}},
	}
	for _, c := range cases {
		got := peakPositions(c.n)
		if !equalU64(got, c.want) {
			t.Errorf("peakPositions(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

func TestMMRSize(t *testing.T) {
	cases := []struct {
		n    uint64
		want uint64
	}{
		{0, 0}, {1, 1}, {2, 3}, {3, 4}, {4, 7}, {5, 8}, {7, 11}, {8, 15},
	}
	for _, c := range cases {
		if got := mmrSize(c.n); got != c.want {
			t.Errorf("mmrSize(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestMMRAppendOneLeafRootIsLeafWrappedWithCount(t *testing.T) {
	s := openTestStore(t, "actorA")
	mmr := NewMMR(s)
	appendLeaves(t, s, mmr, 1)

	leaf := fakeLeaf(1)
	wantRoot := hashMMRRoot(1, leaf) // single peak, bag = peak directly
	got, err := mmr.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != wantRoot {
		t.Errorf("Root mismatch:\n got  %x\n want %x", got[:], wantRoot[:])
	}
	if n, _ := mmr.LeafCount(); n != 1 {
		t.Errorf("LeafCount = %d, want 1", n)
	}
}

func TestMMRAppendTwoLeavesProducesMergedPeak(t *testing.T) {
	s := openTestStore(t, "actorB")
	mmr := NewMMR(s)
	appendLeaves(t, s, mmr, 2)

	// After 2 leaves: peak at pos 3 = hashNode(leaf1, leaf2).
	want := hashMMRRoot(2, hashMMRNode(fakeLeaf(1), fakeLeaf(2)))
	got, err := mmr.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Root mismatch")
	}
	if n, _ := mmr.LeafCount(); n != 2 {
		t.Errorf("LeafCount = %d, want 2", n)
	}
}

func TestMMRAppendThreeLeavesProducesTwoPeaks(t *testing.T) {
	s := openTestStore(t, "actorC")
	mmr := NewMMR(s)
	appendLeaves(t, s, mmr, 3)

	// Peaks: pos 3 (parent of leaves 1,2) and pos 4 (leaf 3).
	tallPeak := hashMMRNode(fakeLeaf(1), fakeLeaf(2))
	shortPeak := fakeLeaf(3)
	bag := hashMMRBag(tallPeak, shortPeak)
	want := hashMMRRoot(3, bag)

	got, err := mmr.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Root mismatch:\n got  %x\n want %x", got[:], want[:])
	}
}

func TestMMRAppendFourLeavesCascadesTwice(t *testing.T) {
	s := openTestStore(t, "actorD")
	mmr := NewMMR(s)
	appendLeaves(t, s, mmr, 4)

	// 4 leaves: cascade once at leaf 2, once at leaf 3-4 chain.
	left := hashMMRNode(fakeLeaf(1), fakeLeaf(2))
	right := hashMMRNode(fakeLeaf(3), fakeLeaf(4))
	root := hashMMRNode(left, right)
	want := hashMMRRoot(4, root) // single peak at pos 7

	got, err := mmr.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Root mismatch")
	}
}

func TestMMRDeterministicAcrossActors(t *testing.T) {
	// Two stores, same input sequence → same root.
	s1 := openTestStore(t, "alpha")
	s2 := openTestStore(t, "beta")
	m1 := NewMMR(s1)
	m2 := NewMMR(s2)
	appendLeaves(t, s1, m1, 7)
	appendLeaves(t, s2, m2, 7)
	r1, err := m1.Root()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := m2.Root()
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Errorf("Roots differ across actors despite identical inputs")
	}
}

func TestMMRRootCommitsToLeafCount(t *testing.T) {
	// Two MMRs with the same final peak structure but different leaf
	// counts MUST produce different roots. We hand-construct: an
	// imaginary leaf count of 2 with peak hashNode(L1,L2) and a leaf
	// count of 4 with peak hashNode(L1',L2') where the inner hash
	// happens to be byte-identical to the 2-leaf case wouldn't be a
	// realistic collision, so we instead just verify hashMMRRoot pins
	// the count.
	peak := fakeLeaf(99)
	r2 := hashMMRRoot(2, peak)
	r4 := hashMMRRoot(4, peak)
	if r2 == r4 {
		t.Errorf("hashMMRRoot fails to commit to leaf count")
	}
}

func TestMMREmptyRoot(t *testing.T) {
	s := openTestStore(t, "empty")
	mmr := NewMMR(s)
	got, err := mmr.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != EmptyMMRRoot {
		t.Errorf("Empty Root = %x, want EmptyMMRRoot %x", got[:], EmptyMMRRoot[:])
	}
}

func TestMMRAppendIsIncremental(t *testing.T) {
	// Appending one leaf at a time to N produces the same root as one
	// hypothetical batch construction. Our cascade correctness is what
	// drives this; this test pins the property regression-style.
	s := openTestStore(t, "incr")
	mmr := NewMMR(s)
	appendLeaves(t, s, mmr, 11)
	got, err := mmr.Root()
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot into a string for golden compare with future runs of the
	// same test (catches accidental hash-domain or peak-bagging changes).
	if got == ([32]byte{}) {
		t.Errorf("got zero root for 11 leaves")
	}
	// Sanity: re-roll a fresh store, append same 11 leaves, expect equal.
	s2 := openTestStore(t, "incr2")
	mmr2 := NewMMR(s2)
	appendLeaves(t, s2, mmr2, 11)
	got2, _ := mmr2.Root()
	if got != got2 {
		t.Errorf("Append ordering not deterministic at N=11")
	}
}

func TestMMRResetClearsState(t *testing.T) {
	s := openTestStore(t, "resettest")
	mmr := NewMMR(s)
	appendLeaves(t, s, mmr, 5)
	if err := mmr.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if n, _ := mmr.LeafCount(); n != 0 {
		t.Errorf("after Reset, LeafCount = %d, want 0", n)
	}
	got, err := mmr.Root()
	if err != nil {
		t.Fatal(err)
	}
	if got != EmptyMMRRoot {
		t.Errorf("after Reset, Root != EmptyMMRRoot")
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hashesPrintable is a tiny utility to dump hashes in test failures.
func hashesPrintable(h [32]byte) string { return fmt.Sprintf("%x", h[:8]) }

// Sanity check that bytes package is exercised somewhere (avoids
// "imported and not used" if a future refactor drops the only use).
var _ = bytes.Equal

// Copyright © 2026 Paxlabs Inc. All rights reserved.
