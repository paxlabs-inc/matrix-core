// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package task

import (
	"path/filepath"
	"testing"
)

// TestBeginGetRoundtrip: a begun task is durable, running, and readable back.
func TestBeginGetRoundtrip(t *testing.T) {
	s := Open(t.TempDir())
	s.Begin("conv1", "run1", "build the thing")

	got, ok := s.Get("conv1")
	if !ok {
		t.Fatal("task should exist after Begin")
	}
	if got.RunID != "run1" || got.Objective != "build the thing" || got.Status != StatusRunning {
		t.Fatalf("unexpected record: %+v", got)
	}
	if got.Created.IsZero() || got.Updated.IsZero() {
		t.Error("timestamps must be set")
	}
}

// TestFinishCAS: Finish only applies for the run that currently owns the
// record — a superseded older run can never clobber its replacement.
func TestFinishCAS(t *testing.T) {
	s := Open(t.TempDir())
	s.Begin("conv1", "run1", "objective A")
	// A new dispatch supersedes run1 with run2 (barge-in).
	s.Begin("conv1", "run2", "objective B")

	// The superseded run1 now reports interrupted — must be a NO-OP.
	s.Finish("conv1", "run1", StatusInterrupted)
	got, _ := s.Get("conv1")
	if got.RunID != "run2" || got.Status != StatusRunning {
		t.Fatalf("stale run's Finish clobbered the live record: %+v", got)
	}

	// The owning run2 can finish it.
	s.Finish("conv1", "run2", StatusDone)
	got, _ = s.Get("conv1")
	if got.Status != StatusDone {
		t.Fatalf("owning run's Finish should apply: %+v", got)
	}
}

// TestCheckpointCAS: Checkpoint advances the attempt counter only for the
// owning run.
func TestCheckpointCAS(t *testing.T) {
	s := Open(t.TempDir())
	s.Begin("conv1", "run1", "obj")
	s.Checkpoint("conv1", "run1", 2, "retrying after a snag")
	s.Checkpoint("conv1", "stale", 9, "should be ignored")

	got, _ := s.Get("conv1")
	if got.Attempt != 2 {
		t.Fatalf("attempt should be 2 (stale checkpoint ignored): %+v", got)
	}
	if got.Note != "retrying after a snag" {
		t.Fatalf("note not recorded: %+v", got)
	}
}

// TestRunningOnlyRunning: Running returns running tasks across conversations
// and excludes finished/interrupted/empty-objective ones.
func TestRunningOnlyRunning(t *testing.T) {
	s := Open(t.TempDir())
	s.Begin("convA", "rA", "task A")
	s.Begin("convB", "rB", "task B")
	s.Begin("convC", "rC", "task C")
	s.Finish("convB", "rB", StatusDone)
	s.Finish("convC", "rC", StatusInterrupted)

	running := s.Running()
	if len(running) != 1 {
		t.Fatalf("exactly one task should be running; got %d: %+v", len(running), running)
	}
	if running[0].ConvID != "convA" || running[0].Objective != "task A" {
		t.Fatalf("the running task should be convA: %+v", running[0])
	}
}

// TestBeginPreservesCreatedOnResume: a same-objective re-Begin (a resume)
// preserves Created; a different objective resets it.
func TestBeginPreservesCreatedOnResume(t *testing.T) {
	s := Open(t.TempDir())
	s.Begin("conv1", "run1", "same objective")
	first, _ := s.Get("conv1")

	s.Begin("conv1", "run2", "same objective") // resume
	resumed, _ := s.Get("conv1")
	if !resumed.Created.Equal(first.Created) {
		t.Errorf("resume must preserve Created: %v vs %v", resumed.Created, first.Created)
	}

	s.Begin("conv1", "run3", "different objective") // new task
	fresh, _ := s.Get("conv1")
	if fresh.Created.Equal(first.Created) {
		t.Error("a different objective should reset Created")
	}
}

// TestDisabledStoreNoop: an empty dir disables the store; every method is a
// safe no-op (dev/CLI runs unchanged).
func TestDisabledStoreNoop(t *testing.T) {
	s := Open("")
	if s.Enabled() {
		t.Fatal("empty dir must be disabled")
	}
	s.Begin("c", "r", "o") // must not panic
	if _, ok := s.Get("c"); ok {
		t.Error("disabled store must hold nothing")
	}
	if len(s.Running()) != 0 {
		t.Error("disabled store has no running tasks")
	}
}

// TestDir derives a tasks dir beside the cortex root, override wins.
func TestDir(t *testing.T) {
	if got := Dir("", "/data/.cortex"); got != filepath.FromSlash("/data/tasks") {
		t.Errorf("derived dir = %q, want /data/tasks", got)
	}
	if got := Dir("/custom/tasks", "/data/.cortex"); got != "/custom/tasks" {
		t.Errorf("override should win, got %q", got)
	}
	if got := Dir("", ""); got != "" {
		t.Errorf("no inputs => disabled (empty), got %q", got)
	}
}
