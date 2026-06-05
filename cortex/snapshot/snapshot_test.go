// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package snapshot

import (
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"matrix/cortex/journal"
	"matrix/cortex/store"
)

func openStateStore(t *testing.T, actor string) *store.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "matrix-state-test")
	s, err := store.Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStateEmptyRoots(t *testing.T) {
	s := openStateStore(t, "empty")
	st := New(s)
	jr, sr, overall, err := st.CurrentRoots()
	if err != nil {
		t.Fatal(err)
	}
	if jr != EmptyMMRRoot {
		t.Errorf("empty journalRoot != EmptyMMRRoot")
	}
	for _, ns := range AnchoredNamespaces {
		if sr[ns] != EmptyRoot {
			t.Errorf("empty stateRoots[%q] != EmptyRoot", ns)
		}
	}
	want := ComputeOverallRoot(EmptyMMRRoot, sr)
	if overall != want {
		t.Errorf("overallRoot mismatch with ComputeOverallRoot")
	}
}

func TestStateSnapshotPersists(t *testing.T) {
	s := openStateStore(t, "snap1")
	st := New(s)

	// Insert some memory + edge state.
	stageMemory(t, s, st, [16]byte{1}, []byte("head1-bytes"))
	stageEdge(t, s, st, [16]byte{1}, 0x03, [16]byte{2}, []byte("edge1-bytes"))

	now := time.Unix(1700000000, 0).UTC()
	m, err := st.Snapshot("alice", TriggerExplicit, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if m.Actor != "alice" {
		t.Errorf("Actor = %q, want alice", m.Actor)
	}
	if m.Trigger != TriggerExplicit {
		t.Errorf("Trigger = %q, want %q", m.Trigger, TriggerExplicit)
	}
	if m.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", m.SchemaVersion, SchemaVersion)
	}
	if m.JournalSeq != 2 {
		t.Errorf("JournalSeq = %d, want 2 (one head + one edge)", m.JournalSeq)
	}

	// Re-load and compare bytes.
	loaded, err := st.LoadSnapshot(m.SeqAtSnapshot)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if loaded.OverallRoot != m.OverallRoot {
		t.Errorf("Persisted OverallRoot mismatch after reload")
	}
}

func TestStateSnapshotSeqMonotonic(t *testing.T) {
	s := openStateStore(t, "monotonic")
	st := New(s)
	now := time.Unix(1700000000, 0)

	a, err := st.Snapshot("a", TriggerExplicit, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Snapshot("a", TriggerExplicit, now)
	if err != nil {
		t.Fatal(err)
	}
	if b.SeqAtSnapshot != a.SeqAtSnapshot+1 {
		t.Errorf("Seq not monotonic: a=%d, b=%d", a.SeqAtSnapshot, b.SeqAtSnapshot)
	}
}

func TestComputeOverallRootDeterministic(t *testing.T) {
	jr := sha256.Sum256([]byte("journal-root"))
	sr1 := map[string][32]byte{
		"memories": sha256.Sum256([]byte("mem")),
		"edges":    sha256.Sum256([]byte("edge")),
	}
	// Different map insertion order, same content.
	sr2 := map[string][32]byte{}
	for k, v := range sr1 {
		sr2[k] = v
	}
	o1 := ComputeOverallRoot(jr, sr1)
	o2 := ComputeOverallRoot(jr, sr2)
	if o1 != o2 {
		t.Errorf("ComputeOverallRoot order-dependent")
	}
}

func TestComputeOverallRootCommitsToJournalRoot(t *testing.T) {
	jr1 := sha256.Sum256([]byte("a"))
	jr2 := sha256.Sum256([]byte("b"))
	sr := map[string][32]byte{
		"memories": EmptyRoot, "edges": EmptyRoot,
	}
	if ComputeOverallRoot(jr1, sr) == ComputeOverallRoot(jr2, sr) {
		t.Errorf("OverallRoot insensitive to journalRoot")
	}
}

func TestComputeOverallRootCommitsToNamespaceRoots(t *testing.T) {
	jr := sha256.Sum256([]byte("j"))
	sr1 := map[string][32]byte{
		"memories": sha256.Sum256([]byte("m1")),
		"edges":    EmptyRoot,
	}
	sr2 := map[string][32]byte{
		"memories": sha256.Sum256([]byte("m2")),
		"edges":    EmptyRoot,
	}
	if ComputeOverallRoot(jr, sr1) == ComputeOverallRoot(jr, sr2) {
		t.Errorf("OverallRoot insensitive to memoriesRoot")
	}
}

func TestStateChangesPropagateToOverallRoot(t *testing.T) {
	s := openStateStore(t, "propagate")
	st := New(s)
	_, _, before, err := st.CurrentRoots()
	if err != nil {
		t.Fatal(err)
	}
	stageMemory(t, s, st, [16]byte{1}, []byte("v1"))
	_, _, after, err := st.CurrentRoots()
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Errorf("OverallRoot unchanged after staging memory")
	}
}

func TestManifestEncodingRoundTrip(t *testing.T) {
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		Actor:         "alice",
		SeqAtSnapshot: 7,
		JournalSeq:    42,
		JournalRoot:   sha256.Sum256([]byte("j")),
		StateRoots: map[string][32]byte{
			"memories": sha256.Sum256([]byte("m")),
			"edges":    sha256.Sum256([]byte("e")),
		},
		OverallRoot: sha256.Sum256([]byte("o")),
		CreatedAt:   1700000000000000000,
		Trigger:     TriggerCompile,
		Counters:    Counters{Memories: 3, Edges: 2, Tombstoned: 1},
	}
	enc, err := EncodeManifest(m)
	if err != nil {
		t.Fatalf("EncodeManifest: %v", err)
	}
	var decoded Manifest
	if err := DecodeManifest(enc, &decoded); err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if decoded.OverallRoot != m.OverallRoot {
		t.Errorf("OverallRoot lost in round-trip")
	}
	if decoded.JournalSeq != m.JournalSeq {
		t.Errorf("JournalSeq lost")
	}
	if decoded.Counters.Memories != 3 {
		t.Errorf("Counters.Memories lost")
	}
}

// stageMemory + stageEdge are test helpers that drive a full atomic
// batch through State.StageMemoryUpdate / StageEdgeUpdate, journaling a
// stand-in entry to satisfy the replay invariant.
func stageMemory(t *testing.T, s *store.Store, st *State, id [16]byte, head []byte) {
	t.Helper()
	wb := s.BeginWrite()
	if err := st.StageMemoryUpdate(wb, id, head); err != nil {
		wb.Abort()
		t.Fatalf("StageMemoryUpdate: %v", err)
	}
	leafHash := sha256.Sum256(head)
	if err := st.StageJournalLeaf(wb, leafHash); err != nil {
		wb.Abort()
		t.Fatalf("StageJournalLeaf: %v", err)
	}
	if err := wb.AppendJournal(&journal.Entry{Kind: journal.KindWrite, Payload: head}); err != nil {
		wb.Abort()
		t.Fatalf("AppendJournal: %v", err)
	}
	if err := wb.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func stageEdge(t *testing.T, s *store.Store, st *State, src [16]byte, et byte, dst [16]byte, edge []byte) {
	t.Helper()
	wb := s.BeginWrite()
	if err := st.StageEdgeUpdate(wb, src, et, dst, edge); err != nil {
		wb.Abort()
		t.Fatalf("StageEdgeUpdate: %v", err)
	}
	leafHash := sha256.Sum256(edge)
	if err := st.StageJournalLeaf(wb, leafHash); err != nil {
		wb.Abort()
		t.Fatalf("StageJournalLeaf: %v", err)
	}
	if err := wb.AppendJournal(&journal.Entry{Kind: journal.KindAddEdge, Payload: edge}); err != nil {
		wb.Abort()
		t.Fatalf("AppendJournal: %v", err)
	}
	if err := wb.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
