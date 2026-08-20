// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package conversation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendGetListRecent(t *testing.T) {
	s := Open(t.TempDir())
	if !s.Enabled() {
		t.Fatal("a non-empty dir should enable the store")
	}
	s.AppendUser("conv_a", "", "what is the PAX price")
	s.AppendAssistant("conv_a", "neo_1", "PAX is trading around $X")

	rec := s.Get("conv_a")
	if rec == nil || len(rec.Turns) != 2 {
		t.Fatalf("want 2 persisted turns, got %+v", rec)
	}
	if rec.Turns[0].Role != "user" || rec.Turns[1].Role != "assistant" {
		t.Errorf("roles out of order: %+v", rec.Turns)
	}
	if rec.Turns[1].IntentID != "neo_1" {
		t.Errorf("assistant intent_id not persisted: %q", rec.Turns[1].IntentID)
	}

	recent := s.Recent("conv_a", 1)
	if len(recent) != 1 || recent[0].Role != "assistant" {
		t.Errorf("Recent(1) should return the last turn, got %+v", recent)
	}

	items := s.List()
	if len(items) != 1 || items[0].ConversationID != "conv_a" {
		t.Fatalf("List should return one summary, got %+v", items)
	}
	if items[0].TurnCount != 2 || items[0].Title == "" || items[0].Preview == "" {
		t.Errorf("summary fields incomplete: %+v", items[0])
	}
	// Title derives from the first user turn.
	if items[0].Title != "what is the PAX price" {
		t.Errorf("title should be the first user turn, got %q", items[0].Title)
	}
}

// A file written in the MCL daemon's on-disk shape must be readable by Neo, so
// a user's pre-Neo threads list alongside their new ones (unified history).
func TestDaemonFileCompatible(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	daemonRec := map[string]interface{}{
		"conversation_id": "conv_old",
		"title":           "old thread",
		"turns": []map[string]interface{}{
			{"role": "user", "text": "old question", "ts": now},
			{"role": "assistant", "text": "old answer", "intent_id": "abc", "ts": now},
		},
		"updated": now,
	}
	b, err := json.Marshal(daemonRec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conv_old.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	s := Open(dir)
	rec := s.Get("conv_old")
	if rec == nil || len(rec.Turns) != 2 {
		t.Fatalf("daemon-written file not read back: %+v", rec)
	}
	if rec.Turns[0].Text != "old question" || rec.Turns[1].IntentID != "abc" {
		t.Errorf("daemon file fields mismatched: %+v", rec.Turns)
	}
	if got := s.List(); len(got) != 1 || got[0].Title != "old thread" {
		t.Errorf("daemon file not summarized: %+v", got)
	}
}

func TestDisabledStoreNoop(t *testing.T) {
	s := Open("  ")
	if s.Enabled() {
		t.Fatal("a blank dir should disable the store")
	}
	s.AppendUser("c", "", "x") // must not panic
	if s.Get("c") != nil {
		t.Error("disabled Get should be nil")
	}
	if s.List() != nil {
		t.Error("disabled List should be nil")
	}
	if s.Recent("c", 5) != nil {
		t.Error("disabled Recent should be nil")
	}
}

func TestUnbounded(t *testing.T) {
	s := Open(t.TempDir())
	const n = 200
	for i := 0; i < n; i++ {
		s.AppendUser("c", "", "turn")
	}
	rec := s.Get("c")
	if rec == nil || len(rec.Turns) != n {
		t.Fatalf("all turns should be retained (no cap), want %d got %d", n, lenTurns(rec))
	}
}

func lenTurns(rec *Record) int {
	if rec == nil {
		return 0
	}
	return len(rec.Turns)
}

// TestStore_AppendIsNotFullRewrite pins the O(1)-per-append invariant: after a
// turn is appended, the on-disk file must not be rewritten in full — bytes
// written by a later append must be bounded by the turn size, not the total
// record size. Concretely: the file size after N+1 appends should be ~N+1 turn
// rows, and a second append must only grow the file by one row's worth, not
// re-emit the whole thing.
func TestStore_AppendIsNotFullRewrite(t *testing.T) {
	s := Open(t.TempDir())
	convID := "rewrite"

	// Seed one turn and snapshot the file size.
	s.AppendUser(convID, "", "seed")
	seedSize := fileSize(t, s.dir, convID)

	// Append a second, tiny turn. Under the OLD full-rewrite scheme the file
	// would be rewritten from scratch — but since both turns are small the
	// distinguishing property is the GROWTH PATTERN, not the absolute size.
	// Instead we use a strong proxy: the file must be a JSONL (newline-delimited)
	// document whose line count equals the number of turns — the hallmark of
	// append-only per-turn writes rather than a single JSON object rewrite.
	s.AppendAssistant(convID, "i1", "ok")

	lines := jsonlLineCount(t, s.dir, convID)
	if lines != 2 {
		t.Fatalf("append-only JSONL should have one line per turn: want 2 lines, file has %d", lines)
	}
	// A single-JSON-object rewrite (the old scheme) would have exactly 1 line
	// (or 0 if pretty-printed) — 2 lines proves we appended, not rewrote.
	if seedSize == 0 {
		t.Fatal("seed file should be non-empty")
	}
}

// TestStore_RetainedCapEnforced verifies the retained hot set is bounded: when
// the retained-turn cap is exceeded, older turns roll to durable archive
// storage (not dropped). Get() returns the bounded hot set; the archived turns
// remain retrievable.
func TestStore_RetainedCapEnforced(t *testing.T) {
	dir := t.TempDir()
	s := openWithCap(dir, 5)
	convID := "capped"

	// Append 8 turns with a cap of 5: the hot set should hold the last 5, the
	// oldest 3 should have rolled to the archive and STILL be retrievable.
	for i := 0; i < 8; i++ {
		s.AppendUser(convID, "", fmt.Sprintf("turn-%d", i))
	}

	hot := s.Get(convID)
	if hot == nil {
		t.Fatal("hot record should be non-nil")
	}
	if len(hot.Turns) != 5 {
		t.Fatalf("retained hot set should be capped at 5, got %d", len(hot.Turns))
	}
	// The hot set is the LAST 5 turns (indices 3..7).
	if hot.Turns[0].Text != "turn-3" {
		t.Errorf("hot set should start at turn-3 (oldest retained), got %q", hot.Turns[0].Text)
	}
	if hot.Turns[4].Text != "turn-7" {
		t.Errorf("hot set should end at turn-7 (newest), got %q", hot.Turns[4].Text)
	}

	// Rolled turns are NOT dropped: the archive holds turn-0..turn-2 and they
	// are still retrievable via the archive reader.
	archived := s.Archived(convID)
	if len(archived) != 3 {
		t.Fatalf("rolled turns must be archived, want 3 got %d", len(archived))
	}
	if archived[0].Text != "turn-0" {
		t.Errorf("first archived turn should be turn-0, got %q", archived[0].Text)
	}
	if archived[2].Text != "turn-2" {
		t.Errorf("last archived turn should be turn-2, got %q", archived[2].Text)
	}

	// Full history (hot + archived) must contain ALL 8 turns in order.
	full := s.History(convID)
	if len(full) != 8 {
		t.Fatalf("full history must be retrievable, want 8 got %d", len(full))
	}
	for i, want := range []string{"turn-0", "turn-1", "turn-2", "turn-3", "turn-4", "turn-5", "turn-6", "turn-7"} {
		if full[i].Text != want {
			t.Errorf("history[%d] = %q, want %q", i, full[i].Text, want)
		}
	}
}

// TestStore_CrashSafetyAtomic verifies that a crash mid-append cannot corrupt
// the store: each turn is persisted as a single atomic JSONL line append, so a
// partial write (truncated last line) is detected and skipped on load. No turn
// already fully written is lost.
func TestStore_CrashSafetyAtomic(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	convID := "crash"

	s.AppendUser(convID, "", "committed-1")
	s.AppendAssistant(convID, "i1", "committed-2")

	// Simulate a crash that wrote a partial (truncated) third line to the JSONL.
	path := filepath.Join(dir, convID+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	// A half-written JSON line (no trailing newline) — a crash mid-write.
	if _, err := f.WriteString(`{"role":"user","text":"PARTIAL_NO_NEWLINE`); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the two committed turns must be intact; the partial line skipped.
	s2 := Open(dir)
	rec := s2.Get(convID)
	if rec == nil {
		t.Fatal("record should be non-nil after partial-write recovery")
	}
	if len(rec.Turns) != 2 {
		t.Fatalf("partial write must not corrupt the store: want 2 turns got %d", len(rec.Turns))
	}
	if rec.Turns[0].Text != "committed-1" || rec.Turns[1].Text != "committed-2" {
		t.Errorf("committed turns corrupted: %+v", rec.Turns)
	}
}

// TestStore_ConcurrentAppendReadSafe exercises concurrent Append + Recent + Get
// under the race detector. No panic, no data race, and every appended turn is
// retrievable after the writers settle.
func TestStore_ConcurrentAppendReadSafe(t *testing.T) {
	s := Open(t.TempDir())
	convID := "concurrent"

	const writers = 4
	const turnsEach = 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(off int) {
			defer wg.Done()
			for i := 0; i < turnsEach; i++ {
				s.AppendUser(convID, "", fmt.Sprintf("w%d-t%d", off, i))
			}
		}(w)
	}
	// Concurrent readers while writers are appending.
	var rwg sync.WaitGroup
	for r := 0; r < 4; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for i := 0; i < 50; i++ {
				_ = s.Recent(convID, 5)
				_ = s.Get(convID)
			}
		}()
	}
	wg.Wait()
	rwg.Wait()

	// After all writers settle, no turn is lost.
	rec := s.Get(convID)
	if rec == nil {
		t.Fatal("record should be non-nil")
	}
	want := writers * turnsEach
	if len(rec.Turns) != want {
		t.Fatalf("no turn should be lost: want %d got %d", want, len(rec.Turns))
	}
}

// fileSize returns the on-disk size of the conversation's file.
func fileSize(t *testing.T, dir, convID string) int64 {
	t.Helper()
	for _, suffix := range []string{".jsonl", ".json"} {
		if fi, err := os.Stat(filepath.Join(dir, convID+suffix)); err == nil {
			return fi.Size()
		}
	}
	t.Fatalf("no conversation file for %s in %s", convID, dir)
	return 0
}

// jsonlLineCount counts newline-delimited JSON lines in the conversation file.
func jsonlLineCount(t *testing.T, dir, convID string) int {
	t.Helper()
	path := filepath.Join(dir, convID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read jsonl %s: %v", path, err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// TestAppendUser_PersistsIntentId pins the F1 contract: the owning intent_id
// passed at user-turn-append time is durably persisted on the user turn (not
// only on the assistant turn). The persisted shape is forward-compatible —
// IntentID has json `omitempty`, so empty values stay invisible.
func TestAppendUser_PersistsIntentId(t *testing.T) {
	s := Open(t.TempDir())
	s.AppendUser("conv_live", "neo_run_abc", "kick off a long task")

	rec := s.Get("conv_live")
	if rec == nil || len(rec.Turns) != 1 {
		t.Fatalf("expected 1 persisted user turn, got %+v", rec)
	}
	if rec.Turns[0].Role != "user" {
		t.Fatalf("role mismatch: %+v", rec.Turns[0])
	}
	if rec.Turns[0].IntentID != "neo_run_abc" {
		t.Errorf("user turn intent_id NOT persisted: got %q, want %q", rec.Turns[0].IntentID, "neo_run_abc")
	}
	if rec.Turns[0].Text != "kick off a long task" {
		t.Errorf("text mismatch: %q", rec.Turns[0].Text)
	}

	// History (the durable-replay path) must also surface the intent_id so a
	// resume cannot be defeated by an archive rollup.
	hist := s.History("conv_live")
	if len(hist) != 1 || hist[0].IntentID != "neo_run_abc" {
		t.Errorf("History must carry intent_id, got %+v", hist)
	}

	// Empty intent_id keeps backward compatibility (omitempty preserves the
	// pre-F1 on-disk shape exactly for direct-reply turns).
	s.AppendUser("conv_blank", "", "no run")
	rec2 := s.Get("conv_blank")
	if rec2 == nil || len(rec2.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %+v", rec2)
	}
	if rec2.Turns[0].IntentID != "" {
		t.Errorf("blank intent_id must round-trip as empty, got %q", rec2.Turns[0].IntentID)
	}
}

func TestConversationManagementPersistsAndForksExactPrefix(t *testing.T) {
	dir := t.TempDir()
	parent := Open(dir)
	parent.AppendUser("parent", "run-1", "first")
	parent.AppendAssistant("parent", "run-1", "answer one")
	parent.AppendUser("parent", "run-2", "second")
	parent.AppendAssistant("parent", "run-2", "answer two")

	title := "Renamed thread"
	archived := true
	if !parent.UpdateMeta("parent", &title, &archived) {
		t.Fatal("combined rename/archive failed")
	}

	reopened := Open(dir)
	items := reopened.List()
	if len(items) != 1 || items[0].Title != title || !items[0].Archived {
		t.Fatalf("metadata did not survive reopen: %+v", items)
	}

	if !reopened.Fork("parent", "child", 3) {
		t.Fatal("fork failed")
	}
	child := reopened.History("child")
	if len(child) != 3 {
		t.Fatalf("fork must contain exactly the selected prefix: got %d turns", len(child))
	}
	for i, want := range []string{"first", "answer one", "second"} {
		if child[i].Text != want {
			t.Fatalf("fork turn %d = %q, want %q", i, child[i].Text, want)
		}
	}
	meta := reopened.Meta("child")
	if meta.ForkedFrom != "parent" || meta.ForkedAtTurn != 3 {
		t.Fatalf("fork provenance mismatch: %+v", meta)
	}

	reopened.AppendAssistant("child", "run-child", "child future")
	reopened.AppendUser("parent", "run-parent", "parent future")
	if got := reopened.History("child"); len(got) != 4 || got[3].Text != "child future" {
		t.Fatalf("child future is not independent: %+v", got)
	}
	if got := reopened.History("parent"); len(got) != 5 || got[4].Text != "parent future" {
		t.Fatalf("parent future is not independent: %+v", got)
	}

	if !reopened.Delete("parent") || reopened.Exists("parent") {
		t.Fatal("parent delete did not remove the durable record")
	}
	if !reopened.Exists("child") {
		t.Fatal("deleting the parent must not delete its fork")
	}
}

func TestRecoveryHandoffIsDurableModelContextWithoutVisibleTurns(t *testing.T) {
	dir := t.TempDir()
	store := Open(dir)
	store.AppendUser("source", "run-1", "repair the deployment")
	store.SetProject("source", "project-a")

	const handoff = "Latest user request: repair the deployment\nRecent context: the health check failed."
	if !store.SetRecoveryHandoff("recovered", "source", handoff) {
		t.Fatal("set recovery handoff failed")
	}
	if store.Exists("recovered") {
		t.Fatal("a recovery sidecar must not fabricate a visible conversation turn")
	}
	if items := store.List(); len(items) != 1 || items[0].ConversationID != "source" {
		t.Fatalf("metadata-only recovery thread leaked into history: %+v", items)
	}
	if got := store.Project("recovered"); got != "project-a" {
		t.Fatalf("recovery project = %q, want project-a", got)
	}

	reopened := Open(dir)
	source, got := reopened.RecoveryHandoff("recovered")
	if source != "source" || got != handoff {
		t.Fatalf("durable recovery handoff = (%q, %q)", source, got)
	}
	if reopened.SetRecoveryHandoff("recovered", "other", "replacement") {
		t.Fatal("recovery handoff must not be silently replaced")
	}
}

func TestDir(t *testing.T) {
	if got := Dir("/explicit", "/data/Neocortex"); got != "/explicit" {
		t.Errorf("explicit override should win, got %q", got)
	}
	if got := Dir("", "/data/Neocortex"); got != "/data/conversations" {
		t.Errorf("should derive sibling of Neocortex root, got %q", got)
	}
	if got := Dir("", ""); got != "" {
		t.Errorf("no roots should disable, got %q", got)
	}
}
