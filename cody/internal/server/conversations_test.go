// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestConversationListFromDurableLedgers exercises GET /conversations against
// the REAL engine + ledgers (req 4.3): every conversation's ledger appears
// with id, title, status, mode, project, and updated_at, sorted newest first;
// a pre-title ledger derives its title from the stored message; an empty
// history is an empty list, not null.
func TestConversationListFromDurableLedgers(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	getList := func() []map[string]interface{} {
		t.Helper()
		resp, err := http.Get(srv.URL + "/conversations")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /conversations = %d", resp.StatusCode)
		}
		var out struct {
			Conversations []map[string]interface{} `json:"conversations"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Conversations
	}

	// Empty history: an empty list.
	if list := getList(); list == nil || len(list) != 0 {
		t.Fatalf("empty history = %v, want []", list)
	}

	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	writeLedger := func(conv string, led ledger) {
		t.Helper()
		if err := e.writeLedger(conv, led); err != nil {
			t.Fatal(err)
		}
	}
	writeLedger("conv-old", ledger{RunID: "run-a", Message: "build a todo app\nwith extra detail", Title: "build a todo app", Mode: "engineer", ProjectID: "todo", Status: "completed", UpdatedAt: base})
	// Pre-title ledger (written before the Title field existed): title derives
	// from the message.
	writeLedger("conv-legacy", ledger{RunID: "run-b", Message: "  fix the login bug  ", Mode: "prototype", Status: "failed", UpdatedAt: base.Add(1 * time.Hour)})
	long := strings.Repeat("x", 200)
	writeLedger("conv-new", ledger{RunID: "run-c", Message: long, Title: conversationTitle(long), Mode: "architect", ProjectID: "site", Status: "running", UpdatedAt: base.Add(2 * time.Hour)})

	list := getList()
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	// Newest first.
	wantOrder := []string{"conv-new", "conv-legacy", "conv-old"}
	for i, want := range wantOrder {
		if got, _ := list[i]["id"].(string); got != want {
			t.Fatalf("order[%d] = %q, want %q (list %v)", i, got, want, list)
		}
	}
	byID := map[string]map[string]interface{}{}
	for _, row := range list {
		byID[row["id"].(string)] = row
	}
	if got := byID["conv-old"]; got["title"] != "build a todo app" || got["status"] != "completed" || got["mode"] != "engineer" || got["project"] != "todo" {
		t.Fatalf("conv-old row = %v", got)
	}
	if got := byID["conv-legacy"]; got["title"] != "fix the login bug" || got["mode"] != "prototype" {
		t.Fatalf("legacy title not derived from message: %v", got)
	}
	if title, _ := byID["conv-new"]["title"].(string); len([]rune(title)) != 80 {
		t.Fatalf("long title not trimmed to 80 runes: %d", len([]rune(title)))
	}
	for _, row := range list {
		if _, err := time.Parse(time.RFC3339, row["updated_at"].(string)); err != nil {
			t.Fatalf("updated_at not RFC3339: %v", row["updated_at"])
		}
	}
}

// TestSubmitWritesDurableTitle asserts a real dispatch writes the trimmed
// title onto the ledger so history reads well without touching the trace.
func TestSubmitWritesDurableTitle(t *testing.T) {
	workspaceRoot := t.TempDir()
	seedExistingProject(t, workspaceRoot)
	e := newEngine(t, workspaceRoot, t.TempDir(), "", openCortex(t, t.TempDir()))
	t.Cleanup(e.Close)

	_, _, err := e.Submit("conv-title", "Add dark mode to the settings page\nplus more context here", "", "", "", "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	led, err := e.readLedger("conv-title")
	if err != nil {
		t.Fatal(err)
	}
	if led.Title != "Add dark mode to the settings page" {
		t.Fatalf("ledger title = %q", led.Title)
	}
}
