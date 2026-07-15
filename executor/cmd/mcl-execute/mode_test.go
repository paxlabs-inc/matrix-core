package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDaemonStoreModes proves the executor daemon's durable stores create their
// files 0600 and directories 0700 on really-created paths: the transcript file,
// the per-user conversation-thread store, and the async-job registry.
func TestDaemonStoreModes(t *testing.T) {
	base := t.TempDir()

	// Transcript file.
	tp := filepath.Join(base, "transcript.jsonl")
	tr, err := openTranscript(tp)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	tr.Event("test", "phase", map[string]interface{}{"k": "v"})
	_ = tr.Close()
	if got := permOfT(t, tp); got != 0o600 {
		t.Fatalf("transcript mode = %o, want 0600", got)
	}

	// Conversation-thread store.
	convDir := filepath.Join(base, "conversations")
	cs := newConversationStore(convDir)
	cs.Append("conv-1", convTurn{Role: "user", Text: "hello"})
	if got := permOfT(t, convDir); got != 0o700 {
		t.Fatalf("conversation dir mode = %o, want 0700", got)
	}
	assertFiles0600(t, convDir)

	// Async-job registry.
	asyncDir := filepath.Join(base, "async")
	r := newAsyncRegistry(16, asyncDir)
	if _, err := r.CreateQueued("intent-1", "user-1", messageRequest{}); err != nil {
		t.Fatalf("create queued: %v", err)
	}
	if got := permOfT(t, asyncDir); got != 0o700 {
		t.Fatalf("async dir mode = %o, want 0700", got)
	}
	assertFiles0600(t, asyncDir)
}

func permOfT(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode().Perm()
}

func assertFiles0600(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		seen++
		if got := permOfT(t, filepath.Join(dir, e.Name())); got != 0o600 {
			t.Fatalf("file %s mode = %o, want 0600", e.Name(), got)
		}
	}
	if seen == 0 {
		t.Fatal("no files created to assert modes on")
	}
}
