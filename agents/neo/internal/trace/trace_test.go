// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package trace

// Tests for the F3 durable workspace-trace sidecar store: async persistence of
// the workspace SSE frames, the retained-event cap, and the sidecar-not-Neocortex
// guarantee. Every test drives the REAL store against a real temp dir — no
// fakes (the store's whole job is real disk persistence).

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestTrace_PersistsStepsSearchesMediaAsync records the three core workspace
// event families through the async path and reads them back, in order, after a
// Flush. This is the heart of F3: the workspace survives because it is written
// to disk, not just streamed to a dropped broker buffer.
func TestTrace_PersistsStepsSearchesMediaAsync(t *testing.T) {
	st := Open(t.TempDir())
	defer st.Close()
	if !st.Enabled() {
		t.Fatal("store with a non-empty dir must be enabled")
	}

	const runID = "neo_run_1"
	st.Record(runID, Event{Seq: 1, Type: "tool.step", Phase: "neo", Fields: map[string]interface{}{"id": "s1", "surface": "terminal", "command": "ls -la"}})
	st.Record(runID, Event{Seq: 2, Type: "tool.search", Phase: "neo", Fields: map[string]interface{}{"query": "paxeer block height"}})
	st.Record(runID, Event{Seq: 3, Type: "tool.media", Phase: "neo", Fields: map[string]interface{}{"kind": "image", "url": "/media/x.png"}})

	// Record is async: nothing is guaranteed on disk until Flush drains the
	// writer queue.
	st.Flush()

	got := st.Load(runID)
	if len(got) != 3 {
		t.Fatalf("want 3 persisted events, got %d (%+v)", len(got), got)
	}
	wantTypes := []string{"tool.step", "tool.search", "tool.media"}
	for i, w := range wantTypes {
		if got[i].Type != w {
			t.Errorf("event %d: want type %q got %q", i, w, got[i].Type)
		}
		if got[i].Seq != i+1 {
			t.Errorf("event %d: want seq %d got %d (order must be preserved)", i, i+1, got[i].Seq)
		}
	}
	// Fields round-trip intact so the client reducer rebuilds the viewport.
	if got[0].Fields["command"] != "ls -la" {
		t.Errorf("tool.step command field lost: %+v", got[0].Fields)
	}

	// A different run is isolated (no cross-run bleed) and an unknown run is
	// empty (not an error).
	if other := st.Load("neo_run_other"); other != nil {
		t.Errorf("unknown run must Load to nil, got %+v", other)
	}
}

func TestTraceRemoveWaitsForQueuedWrites(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	defer st.Close()

	const runID = "neo_delete"
	st.Record(runID, Event{Seq: 1, Type: "tool.step"})
	if !st.Remove(runID) {
		t.Fatal("remove should report the real file deletion")
	}
	if got := st.Load(runID); got != nil {
		t.Fatalf("removed trace is still readable: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, runID+".trace.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("trace file still exists after synchronous remove: %v", err)
	}
	if st.Remove(runID) {
		t.Fatal("removing a missing trace must not be reported as a deletion")
	}
}

// TestTrace_RetainedCapEnforced confirms the per-run JSONL is trimmed to the
// newest `retain` events (bounded durable record, like the broker's replay
// buffer but larger) — no unbounded growth on a long task.
func TestTrace_RetainedCapEnforced(t *testing.T) {
	const cap = 5
	st := openWithCap(t.TempDir(), cap)
	defer st.Close()

	const runID = "neo_capped"
	const total = 23
	for i := 1; i <= total; i++ {
		st.Record(runID, Event{Seq: i, Type: "tool.step", Fields: map[string]interface{}{"id": "s", "n": i}})
	}
	st.Flush()

	got := st.Load(runID)
	if len(got) != cap {
		t.Fatalf("retained-cap not enforced: want %d events, got %d", cap, len(got))
	}
	// The NEWEST cap events are kept (oldest trimmed): seq total-cap+1 .. total.
	wantFirst := total - cap + 1
	if got[0].Seq != wantFirst {
		t.Errorf("oldest retained event: want seq %d got %d (newest must be kept)", wantFirst, got[0].Seq)
	}
	if got[len(got)-1].Seq != total {
		t.Errorf("newest retained event: want seq %d got %d", total, got[len(got)-1].Seq)
	}
}

// TestTrace_NotJournaledIntoCortex proves the store is a pure SIDECAR (m9/f4):
// every byte it writes lands under its OWN dir, never in a sibling Neocortex tree,
// and the package has no Neocortex coupling (it imports none — a structural
// guarantee this test backs with a runtime artifact check).
func TestTrace_NotJournaledIntoCortex(t *testing.T) {
	root := t.TempDir()
	traceDir := filepath.Join(root, "trace")
	cortexDir := filepath.Join(root, "Neocortex") // sibling that MUST stay untouched

	st := Open(traceDir)
	defer st.Close()
	st.Record("neo_sidecar", Event{Seq: 1, Type: "tool.step", Fields: map[string]interface{}{"id": "s1"}})
	st.Flush()

	// The trace file exists under the trace dir.
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("trace dir unreadable: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "neo_sidecar.trace.jsonl" {
		t.Fatalf("trace dir must contain exactly the run's JSONL, got %v", names)
	}

	// No Neocortex tree was created as a side effect — the store touches only its
	// own dir.
	if _, err := os.Stat(cortexDir); !os.IsNotExist(err) {
		t.Errorf("sidecar store must NEVER create a Neocortex tree (stat %s: %v)", cortexDir, err)
	}

	// A disabled store (empty dir) is a total no-op: Record/Load/Flush/Close
	// never touch the filesystem or panic.
	dis := Open("")
	if dis.Enabled() {
		t.Fatal("empty-dir store must be disabled")
	}
	dis.Record("x", Event{Type: "tool.step"})
	dis.Flush()
	dis.Close()
	if got := dis.Load("x"); got != nil {
		t.Errorf("disabled store must Load to nil, got %+v", got)
	}
}

// TestTrace_RecordNeverBlocks confirms Record returns promptly even under a
// burst far larger than the writer can drain synchronously — it enqueues
// (dropping on a full queue) and never blocks the caller (the broker publish
// path). A blocking implementation would exceed the deadline.
func TestTrace_RecordNeverBlocks(t *testing.T) {
	st := Open(t.TempDir())
	defer st.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < queueDepth*4; i++ {
			st.Record("neo_burst", Event{Seq: i, Type: "tool.step", Fields: map[string]interface{}{"id": "s"}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked under a burst — it must be non-blocking (drop-on-full), never stall the publish path")
	}
}
