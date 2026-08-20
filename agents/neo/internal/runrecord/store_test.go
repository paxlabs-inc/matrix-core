package runrecord

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestStoreLifecycleIsMonotonicAndTimestampsAreStable(t *testing.T) {
	s := Open(t.TempDir())
	rec, created, err := s.Begin("neo_run", "conv_run", "key_run", "build the feature")
	if err != nil || !created {
		t.Fatalf("Begin: created=%v err=%v", created, err)
	}
	createdAt := rec.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")
	loaded, ok, err := s.Get(rec.IntentID)
	if err != nil || !ok {
		t.Fatalf("Get running: ok=%v err=%v", ok, err)
	}
	if got := loaded.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"); got != createdAt {
		t.Fatalf("created_at changed: got %q want %q", got, createdAt)
	}
	finished, transitioned, err := s.Finish(rec.IntentID, StatusCompleted, "done", "", 17)
	if err != nil || !transitioned {
		t.Fatalf("Finish: transitioned=%v err=%v", transitioned, err)
	}
	if finished.EndedAt == nil || finished.Result != "done" || finished.LastEventSeq != 17 {
		t.Fatalf("terminal record incomplete: %+v", finished)
	}
	again, transitioned, err := s.Finish(rec.IntentID, StatusCompleted, "different", "", 99)
	if err != nil || transitioned {
		t.Fatalf("idempotent Finish: transitioned=%v err=%v", transitioned, err)
	}
	if again.Result != "done" || again.LastEventSeq != 17 || again.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00") != createdAt {
		t.Fatalf("terminal record mutated: %+v", again)
	}
	if _, _, err := s.Finish(rec.IntentID, StatusInterrupted, "stopped", "", 18); !errors.Is(err, ErrTerminal) {
		t.Fatalf("terminal transition: got %v want %v", err, ErrTerminal)
	}
}

func TestFinishDetailedPreservesRuntimeIncompleteDiagnostics(t *testing.T) {
	store := Open(t.TempDir())
	record, created, err := store.Begin(
		"neo_diagnostic", "conv_diagnostic", "key_diagnostic", "inspect workspace",
	)
	if err != nil || !created {
		t.Fatalf("Begin: created=%v err=%v", created, err)
	}
	diagnostic := json.RawMessage(`{"phase":"provider","repairs":{"expectation":2},"provider_error":"invalid structured tool call"}`)
	finished, transitioned, err := store.FinishDetailed(
		record.IntentID, StatusFailed, "saved partial", "friendly failure", 5, diagnostic,
	)
	if err != nil || !transitioned {
		t.Fatalf("FinishDetailed: transitioned=%v err=%v", transitioned, err)
	}
	if string(finished.Diagnostics) != string(diagnostic) {
		t.Fatalf("diagnostics = %s, want %s", finished.Diagnostics, diagnostic)
	}
	loaded, ok, err := store.Get(record.IntentID)
	if err != nil || !ok || string(loaded.Diagnostics) != string(diagnostic) {
		t.Fatalf("reloaded diagnostics = %s, ok=%v err=%v", loaded.Diagnostics, ok, err)
	}
}

func TestStoreIdempotencyKeyCannotDuplicateOrChangeRequest(t *testing.T) {
	s := Open(t.TempDir())
	first, created, err := s.Begin("neo_first", "conv_one", "chat_key", "change state once")
	if err != nil || !created {
		t.Fatalf("first Begin: created=%v err=%v", created, err)
	}
	second, created, err := s.Begin("neo_second", "conv_one", "chat_key", "change state once")
	if err != nil || created {
		t.Fatalf("duplicate Begin: created=%v err=%v", created, err)
	}
	if second.IntentID != first.IntentID {
		t.Fatalf("duplicate minted another intent: got %q want %q", second.IntentID, first.IntentID)
	}
	if _, _, err := s.Begin("neo_third", "conv_one", "chat_key", "different request"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed request: got %v want %v", err, ErrIdempotencyConflict)
	}
	if _, _, err := s.Begin("neo_fourth", "conv_two", "chat_key", "change state once"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed conversation: got %v want %v", err, ErrIdempotencyConflict)
	}
}
