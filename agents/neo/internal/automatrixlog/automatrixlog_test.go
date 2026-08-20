// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package automatrixlog

// Tests for the durable Automatrix completion inbox (req 6.1 / 6.5 / 8.5).
// Every test drives the REAL store against a real temp dir — no fakes (the
// store's whole job is real disk persistence): round-trip, unread count,
// mark-read, newest-first ordering, the no-secrets record shape, and the
// disabled-store no-op.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendListRoundTrip writes real completion records and reads them back,
// newest-first, with every field intact. This is the heart of the store: a
// surprise result survives because it is written to disk, not just streamed.
func TestAppendListRoundTrip(t *testing.T) {
	st := Open(t.TempDir())
	if !st.Enabled() {
		t.Fatal("store with a non-empty dir must be enabled")
	}

	first, err := st.Append(Record{
		OpportunitySummary: "Draft the quarterly update doc you mentioned",
		ResultSummary:      "Drafted a 1-page quarterly update in the thread",
		ConversationID:     "conv_a",
	})
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if first.ID == "" {
		t.Fatal("Append must assign a non-empty id")
	}
	if first.CreatedAt == "" {
		t.Fatal("Append must stamp created_at")
	}
	if first.Read {
		t.Fatal("a new completion record must be unread")
	}

	second, err := st.Append(Record{
		OpportunitySummary: "Summarize the three articles you saved",
		ResultSummary:      "Wrote a combined summary of the three articles",
		ConversationID:     "conv_b",
	})
	if err != nil {
		t.Fatalf("append second: %v", err)
	}

	got := st.List()
	if len(got) != 2 {
		t.Fatalf("want 2 records, got %d (%+v)", len(got), got)
	}
	// Newest-first: the most recent completion is at index 0.
	if got[0].ID != second.ID {
		t.Errorf("List must be newest-first: want %s at index 0, got %s", second.ID, got[0].ID)
	}
	if got[1].ID != first.ID {
		t.Errorf("List must be newest-first: want %s at index 1, got %s", first.ID, got[1].ID)
	}
	// Fields round-trip intact so the client can render the inbox.
	if got[1].OpportunitySummary != "Draft the quarterly update doc you mentioned" ||
		got[1].ResultSummary != "Drafted a 1-page quarterly update in the thread" ||
		got[1].ConversationID != "conv_a" {
		t.Errorf("record fields lost on round-trip: %+v", got[1])
	}
}

// TestUnreadCountAndMarkRead proves the badge count and the mark-read mutation
// against the real on-disk store: a fresh inbox is all-unread, marking one read
// decrements the count and persists, and marking an unknown / already-read id
// is a no-op.
func TestUnreadCountAndMarkRead(t *testing.T) {
	st := Open(t.TempDir())

	a, _ := st.Append(Record{OpportunitySummary: "task a", ResultSummary: "done a", ConversationID: "c1"})
	_, _ = st.Append(Record{OpportunitySummary: "task b", ResultSummary: "done b", ConversationID: "c2"})
	_, _ = st.Append(Record{OpportunitySummary: "task c", ResultSummary: "done c", ConversationID: "c3"})

	if n := st.Unread(); n != 3 {
		t.Fatalf("fresh inbox: want 3 unread, got %d", n)
	}

	changed, err := st.MarkRead(a.ID)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if !changed {
		t.Fatal("marking an unread record must report changed=true")
	}
	if n := st.Unread(); n != 2 {
		t.Fatalf("after one mark-read: want 2 unread, got %d", n)
	}

	// Marking the same id again is a no-op (already read).
	if changed, _ := st.MarkRead(a.ID); changed {
		t.Error("marking an already-read record must report changed=false")
	}
	// Marking an unknown id is a no-op.
	if changed, _ := st.MarkRead("ax_nope"); changed {
		t.Error("marking an unknown id must report changed=false")
	}
	if n := st.Unread(); n != 2 {
		t.Fatalf("no-op mark-reads must not change the count: got %d", n)
	}

	// The read flag persists: a fresh store over the SAME dir sees it read.
	reopened := Open(filepathDir(st))
	for _, r := range reopened.List() {
		if r.ID == a.ID && !r.Read {
			t.Error("mark-read must persist across a store reopen")
		}
	}
}

// TestNoSecretsInRecord proves the record carries only the result-not-protocol
// fields (req 6.5 / 8.5): the on-disk JSON has exactly the whitelisted keys and
// nothing that could hold a token/key/secret or Chronos/alarm jargon.
func TestNoSecretsInRecord(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	if _, err := st.Append(Record{
		OpportunitySummary: "task",
		ResultSummary:      "result",
		ConversationID:     "conv",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		t.Fatalf("read inbox file: %v", err)
	}
	line := strings.TrimSpace(string(raw))

	var keyed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &keyed); err != nil {
		t.Fatalf("inbox line is not valid JSON: %v", err)
	}
	allowed := map[string]bool{
		"id": true, "opportunity_summary": true, "result_summary": true,
		"conversation_id": true, "created_at": true, "read": true,
	}
	for k := range keyed {
		if !allowed[k] {
			t.Errorf("unexpected field %q in durable record — only result-not-protocol fields are allowed", k)
		}
	}
	for _, banned := range []string{"token", "secret", "key", "chronos", "alarm", "marker"} {
		if strings.Contains(strings.ToLower(line), banned) {
			t.Errorf("durable record must not contain %q jargon/secret: %s", banned, line)
		}
	}
}

// TestDisabledStoreIsNoOp confirms an empty-dir store never touches the
// filesystem and stays safe: Append returns the (id-stamped) record with no
// error, and the readers return empty.
func TestDisabledStoreIsNoOp(t *testing.T) {
	st := Open("")
	if st.Enabled() {
		t.Fatal("empty-dir store must be disabled")
	}
	rec, err := st.Append(Record{OpportunitySummary: "x", ResultSummary: "y", ConversationID: "c"})
	if err != nil {
		t.Fatalf("disabled Append must not error: %v", err)
	}
	if rec.ID == "" || rec.CreatedAt == "" {
		t.Error("disabled Append should still fill id/created_at so the caller has a record")
	}
	if got := st.List(); got != nil {
		t.Errorf("disabled store must List to nil, got %+v", got)
	}
	if n := st.Unread(); n != 0 {
		t.Errorf("disabled store Unread must be 0, got %d", n)
	}
	if changed, _ := st.MarkRead("ax_x"); changed {
		t.Error("disabled store MarkRead must report changed=false")
	}
}

// filepathDir returns the store's directory (test helper for the reopen check).
func filepathDir(s *Store) string { return s.dir }
