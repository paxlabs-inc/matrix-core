// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSnapshotRestoreOverHTTP proves req 14.1 end-to-end over the real routes:
// POST /workspace/snapshot captures the tree, GET /workspace/snapshots lists it,
// mutating the workspace and POST /workspace/restore rolls the bytes back, and a
// restore is refused while a run is live in the project.
func TestSnapshotRestoreOverHTTP(t *testing.T) {
	workspaceRoot := t.TempDir()
	seedFile := filepath.Join(workspaceRoot, "app.txt")
	if err := os.WriteFile(seedFile, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, workspaceRoot, t.TempDir(), "http://unused", openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	// Snapshot.
	resp, err := http.Post(srv.URL+"/workspace/snapshot", "application/json",
		strings.NewReader(`{"label": "before edit"}`))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot: %v %v", resp, err)
	}
	var snapOut struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&snapOut)
	resp.Body.Close()
	if snapOut.ID == "" {
		t.Fatal("snapshot returned no id")
	}

	// List.
	resp, _ = http.Get(srv.URL + "/workspace/snapshots")
	var listOut struct {
		Snapshots []map[string]interface{} `json:"snapshots"`
	}
	json.NewDecoder(resp.Body).Decode(&listOut)
	resp.Body.Close()
	if len(listOut.Snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(listOut.Snapshots))
	}

	// Mutate: change the file and add a new one.
	if err := os.WriteFile(seedFile, []byte("MUTATED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "new.txt"), []byte("added\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Restore rolls the bytes back: app.txt is original again, new.txt is gone.
	resp, err = http.Post(srv.URL+"/workspace/restore", "application/json",
		strings.NewReader(`{"id": "`+snapOut.ID+`"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("restore: %v %v", resp, err)
	}
	resp.Body.Close()
	if data, _ := os.ReadFile(seedFile); string(data) != "original\n" {
		t.Fatalf("app.txt after restore = %q, want original", data)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("new.txt survived the restore")
	}

	// Restore is refused while a run is live in the project.
	done := make(chan struct{})
	close(done) // so engine.Close() does not block on this synthetic run
	live := &run{id: "run-live", convID: "conv-x", root: mustAbs(t, workspaceRoot), done: done, status: "running", cancel: func() {}}
	engine.registerRun(live)
	resp, _ = http.Post(srv.URL+"/workspace/restore", "application/json",
		strings.NewReader(`{"id": "`+snapOut.ID+`"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("restore while live = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
