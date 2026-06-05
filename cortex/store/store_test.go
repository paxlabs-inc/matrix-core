// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"matrix/cortex/journal"
)

// openTempStore opens a fresh Store under t.TempDir(). The store is closed
// automatically via t.Cleanup.
func openTempStore(t *testing.T, actor string) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})
	return s
}

func TestOpenAndClose(t *testing.T) {
	s := openTempStore(t, "alice")
	if s.NextSeq() != 0 {
		t.Fatalf("fresh store: NextSeq()=%d want 0", s.NextSeq())
	}
	if s.JournalCount() != 0 {
		t.Fatalf("fresh store: JournalCount()=%d want 0", s.JournalCount())
	}
}

func TestOpenRejectsBadActor(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, "alice/bob", nil); err == nil {
		t.Fatalf("expected error for actor containing '/'")
	}
}

func TestAppendJournalAndIterate(t *testing.T) {
	s := openTempStore(t, "alice")

	want := []journal.Entry{
		{Kind: journal.KindRaw, CreatedAt: 100, Payload: []byte("first")},
		{Kind: journal.KindRaw, CreatedAt: 200, Payload: []byte("second")},
		{Kind: journal.KindWrite, CreatedAt: 300, Payload: []byte("third")},
	}

	for i := range want {
		seq, err := s.AppendJournal(&want[i])
		if err != nil {
			t.Fatalf("AppendJournal #%d: %v", i, err)
		}
		if seq != uint64(i) {
			t.Fatalf("AppendJournal #%d: seq=%d want %d", i, seq, i)
		}
		if want[i].Seq != uint64(i) {
			t.Fatalf("entry Seq not backfilled: got %d want %d", want[i].Seq, i)
		}
	}

	if got := s.NextSeq(); got != uint64(len(want)) {
		t.Fatalf("NextSeq=%d want %d", got, len(want))
	}

	var got []journal.Entry
	err := s.IterJournal(func(e *journal.Entry) error {
		// copy because IterJournal reuses the struct in principle
		cp := *e
		cp.Payload = append([]byte(nil), e.Payload...)
		cp.CreatedBy = append([]byte(nil), e.CreatedBy...)
		got = append(got, cp)
		return nil
	})
	if err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("iter count: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Seq != want[i].Seq || got[i].Kind != want[i].Kind ||
			got[i].CreatedAt != want[i].CreatedAt ||
			!bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Fatalf("iter entry #%d mismatch: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// TestJournalHeadPersistsAcrossReopen proves Open recovers the next-seq value
// from meta/journal_head, so appends after reopen don't overwrite prior keys.
func TestJournalHeadPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	actor := "alice"

	s, err := Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.AppendJournal(&journal.Entry{Kind: journal.KindRaw, CreatedAt: int64(i)}); err != nil {
			t.Fatalf("AppendJournal: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dir, actor, nil)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	defer s2.Close()
	if got := s2.NextSeq(); got != 5 {
		t.Fatalf("after reopen: NextSeq=%d want 5", got)
	}
	seq, err := s2.AppendJournal(&journal.Entry{Kind: journal.KindRaw, CreatedAt: 999})
	if err != nil {
		t.Fatalf("AppendJournal post-reopen: %v", err)
	}
	if seq != 5 {
		t.Fatalf("post-reopen seq=%d want 5", seq)
	}

	var count int
	if err := s2.IterJournal(func(e *journal.Entry) error { count++; return nil }); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if count != 6 {
		t.Fatalf("post-reopen journal entry count=%d want 6", count)
	}
}

// TestPerActorIsolation: two actors under the same root must not see each
// other's journal entries. This is the §1 invariant.
func TestPerActorIsolation(t *testing.T) {
	dir := t.TempDir()

	alice, err := Open(dir, "alice", nil)
	if err != nil {
		t.Fatalf("Open alice: %v", err)
	}
	defer alice.Close()
	bob, err := Open(dir, "bob", nil)
	if err != nil {
		t.Fatalf("Open bob: %v", err)
	}
	defer bob.Close()

	if _, err := alice.AppendJournal(&journal.Entry{Kind: journal.KindWrite, CreatedAt: 1, Payload: []byte("a-secret")}); err != nil {
		t.Fatalf("alice append: %v", err)
	}
	if _, err := bob.AppendJournal(&journal.Entry{Kind: journal.KindWrite, CreatedAt: 2, Payload: []byte("b-secret")}); err != nil {
		t.Fatalf("bob append: %v", err)
	}

	// They must be in different directories.
	aPath := filepath.Join(alice.Path(), "store")
	bPath := filepath.Join(bob.Path(), "store")
	if aPath == bPath {
		t.Fatalf("alice and bob share a DB path: %s", aPath)
	}

	// Each sees only their own entry.
	var aSaw, bSaw [][]byte
	if err := alice.IterJournal(func(e *journal.Entry) error {
		aSaw = append(aSaw, append([]byte(nil), e.Payload...))
		return nil
	}); err != nil {
		t.Fatalf("alice iter: %v", err)
	}
	if err := bob.IterJournal(func(e *journal.Entry) error {
		bSaw = append(bSaw, append([]byte(nil), e.Payload...))
		return nil
	}); err != nil {
		t.Fatalf("bob iter: %v", err)
	}
	if len(aSaw) != 1 || !bytes.Equal(aSaw[0], []byte("a-secret")) {
		t.Fatalf("alice saw: %v", aSaw)
	}
	if len(bSaw) != 1 || !bytes.Equal(bSaw[0], []byte("b-secret")) {
		t.Fatalf("bob saw: %v", bSaw)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
