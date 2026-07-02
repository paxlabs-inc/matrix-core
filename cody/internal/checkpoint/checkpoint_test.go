// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	cortex "matrix/cortex"
	"matrix/cortex/store"
)

func seed(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "main.go", "package main\n")
	seed(t, root, "pkg/util.go", "package pkg\n")
	seed(t, root, "node_modules/dep/index.js", "module.exports = 1\n")

	s, err := NewSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Snapshot("before-refactor")
	if err != nil {
		t.Fatal(err)
	}

	// Risky multi-file change: mutate, delete, create.
	seed(t, root, "main.go", "package main // broken\n")
	if err := os.Remove(filepath.Join(root, "pkg/util.go")); err != nil {
		t.Fatal(err)
	}
	seed(t, root, "pkg/new_junk.go", "package pkg // junk\n")

	if err := s.Restore(id); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "main.go"); got != "package main\n" {
		t.Fatalf("mutated file not restored: %q", got)
	}
	if got := read(t, root, "pkg/util.go"); got != "package pkg\n" {
		t.Fatalf("deleted file not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "pkg/new_junk.go")); !os.IsNotExist(err) {
		t.Fatal("file created after the snapshot survived restore")
	}
	// Skip dirs are untouched by snapshot/restore.
	if got := read(t, root, "node_modules/dep/index.js"); got != "module.exports = 1\n" {
		t.Fatalf("skip dir touched: %q", got)
	}
}

func TestSnapshotList(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "a.txt", "a\n")
	s, err := NewSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	if infos, err := s.List(); err != nil || len(infos) != 0 {
		t.Fatalf("List() on fresh workspace = %v, %v", infos, err)
	}
	id1, err := s.Snapshot("first")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Snapshot("second")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != id1 || infos[1].ID != id2 {
		t.Fatalf("List() = %+v", infos)
	}
	if infos[0].Label != "first" || infos[0].Files != 1 {
		t.Fatalf("meta mismatch: %+v", infos[0])
	}
	if err := s.Restore("no-such-snapshot"); err == nil {
		t.Fatal("Restore accepted a missing snapshot id")
	}
}

func TestSnapshotsExcludedFromSnapshots(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "a.txt", "a\n")
	s, err := NewSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot("one"); err != nil {
		t.Fatal(err)
	}
	id2, err := s.Snapshot("two")
	if err != nil {
		t.Fatal(err)
	}
	// The second snapshot must not have recursed into .cody/snapshots.
	tree := filepath.Join(root, snapshotsDir, id2, "tree")
	if _, err := os.Stat(filepath.Join(tree, ".cody")); !os.IsNotExist(err) {
		t.Fatal("snapshot captured the .cody state dir")
	}
	if read(t, tree, "a.txt") != "a\n" {
		t.Fatal("snapshot tree missing workspace file")
	}
}

// openCortex opens a REAL cortex over a real pebble store.
func openCortex(t *testing.T, root string) *cortex.Cortex {
	t.Helper()
	s, err := store.Open(root, "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return cortex.New(s)
}

func TestProgressCheckpointsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := openCortex(t, dir)
	p := NewProgress(c, "plan-demo")

	if latest, err := p.Latest(); err != nil || latest != nil {
		t.Fatalf("Latest() on empty plan = %v, %v", latest, err)
	}
	if err := p.Record(Checkpoint{TaskID: "t1", Attempt: 1, Status: "done", Summary: "greet helper landed"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Record(Checkpoint{TaskID: "t2", Attempt: 2, Status: "partial", Summary: "lint red"}); err != nil {
		t.Fatal(err)
	}
	// Unrelated session traffic must not corrupt checkpoint reads.
	if _, err := c.AppendMessage(cortex.Message{ConversationID: "plan-demo", Role: cortex.RoleUser, Content: "how is it going?"}); err != nil {
		t.Fatal(err)
	}

	all, err := p.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].TaskID != "t1" || all[1].TaskID != "t2" {
		t.Fatalf("All() = %+v", all)
	}
	latest, err := p.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if latest.TaskID != "t2" || latest.Status != "partial" || latest.Attempt != 2 {
		t.Fatalf("Latest() = %+v", latest)
	}
	done, err := p.Done()
	if err != nil {
		t.Fatal(err)
	}
	if !done["t1"] || done["t2"] {
		t.Fatalf("Done() = %v", done)
	}
	if err := p.Record(Checkpoint{Status: "done"}); err == nil {
		t.Fatal("Record accepted an empty task_id")
	}
}

func TestProgressSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	s1, err := store.Open(dir, "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1 := NewProgress(cortex.New(s1), "plan-demo")
	if err := p1.Record(Checkpoint{TaskID: "t1", Attempt: 1, Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh process over the same durable store (the codyd-restart path).
	s2, err := store.Open(dir, "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	p2 := NewProgress(cortex.New(s2), "plan-demo")
	done, err := p2.Done()
	if err != nil {
		t.Fatal(err)
	}
	if !done["t1"] {
		t.Fatalf("checkpoint lost across restart: %v", done)
	}
}
