// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendGetListRecent(t *testing.T) {
	s := Open(t.TempDir())
	if !s.Enabled() {
		t.Fatal("a non-empty dir should enable the store")
	}
	s.AppendUser("conv_a", "what is the PAX price")
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
	s.AppendUser("c", "x") // must not panic
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

func TestRetentionBound(t *testing.T) {
	s := Open(t.TempDir())
	for i := 0; i < maxTurns+25; i++ {
		s.AppendUser("c", "turn")
	}
	rec := s.Get("c")
	if rec == nil || len(rec.Turns) != maxTurns {
		t.Fatalf("turns should be bounded to %d, got %d", maxTurns, len(rec.Turns))
	}
}

func TestDir(t *testing.T) {
	if got := Dir("/explicit", "/data/cortex"); got != "/explicit" {
		t.Errorf("explicit override should win, got %q", got)
	}
	if got := Dir("", "/data/cortex"); got != "/data/conversations" {
		t.Errorf("should derive sibling of cortex root, got %q", got)
	}
	if got := Dir("", ""); got != "" {
		t.Errorf("no roots should disable, got %q", got)
	}
}
