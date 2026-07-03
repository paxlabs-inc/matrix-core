// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"testing"
	"time"
)

// collectHeartbeats drains a broker subscription for up to wait, returning the
// run.activity events flagged heartbeat=true.
func collectHeartbeats(e *Engine, runID string, sinceSeq int, wait time.Duration) []Event {
	replay, live, cancel := e.broker.subscribe(runID, sinceSeq)
	defer cancel()
	var out []Event
	keep := func(ev Event) {
		if ev.Type != "run.activity" {
			return
		}
		if hb, _ := ev.Fields["heartbeat"].(bool); hb {
			out = append(out, ev)
		}
	}
	for _, ev := range replay {
		keep(ev)
	}
	deadline := time.After(wait)
	for {
		select {
		case ev, ok := <-live:
			if !ok {
				return out
			}
			keep(ev)
		case <-deadline:
			return out
		}
	}
}

// TestHeartbeatEmitsCurrentPhaseDuringLongPhase drives the REAL heartbeat
// ticker over the real run + broker: during a long silent stretch it re-emits
// the run's current phase as run.activity liveness events (heartbeat=true)
// carrying the label and a growing elapsed_ms, and it tracks a phase change at
// the next boundary (req 2.2).
func TestHeartbeatEmitsCurrentPhaseDuringLongPhase(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 15 * time.Millisecond
	defer func() { heartbeatInterval = prev }()

	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	r := &run{id: "run-hb", convID: "conv-hb", done: make(chan struct{}), status: "running", started: time.Now(), cancel: func() {}}
	e.registerRun(r)
	e.broker.ensure(r.id)

	e.activity(r, phaseUnderstanding, "")
	stop := e.heartbeat(context.Background(), r)
	defer stop()

	beats := collectHeartbeats(e, r.id, 0, 400*time.Millisecond)
	if len(beats) < 2 {
		t.Fatalf("expected >=2 heartbeats during a long phase, got %d", len(beats))
	}
	for _, ev := range beats {
		if got, _ := ev.Fields["phase"].(string); got != phaseUnderstanding {
			t.Fatalf("heartbeat phase = %q, want %q", got, phaseUnderstanding)
		}
		if got, _ := ev.Fields["label"].(string); got != phaseLabels[phaseUnderstanding] {
			t.Fatalf("heartbeat label = %q, want %q", got, phaseLabels[phaseUnderstanding])
		}
		if _, ok := ev.Fields["elapsed_ms"]; !ok {
			t.Fatalf("heartbeat missing elapsed_ms: %+v", ev.Fields)
		}
	}
	first, _ := beats[0].Fields["elapsed_ms"].(int64)
	last, _ := beats[len(beats)-1].Fields["elapsed_ms"].(int64)
	if last < first {
		t.Fatalf("elapsed_ms went backwards: first=%d last=%d", first, last)
	}

	// The next boundary moves the phase; subsequent ticks carry it.
	lastSeq := beats[len(beats)-1].Seq
	e.activity(r, phasePlanning, "")
	planned := collectHeartbeats(e, r.id, lastSeq, 400*time.Millisecond)
	var sawPlanning bool
	for _, ev := range planned {
		if got, _ := ev.Fields["phase"].(string); got == phasePlanning {
			sawPlanning = true
		}
	}
	if !sawPlanning {
		t.Fatalf("heartbeat never picked up the planning boundary; got %d ticks", len(planned))
	}
}

// TestHeartbeatSuppressedWhileParkedAndBeforeFirstPhase asserts the honest
// edges: no ticks before the first boundary sets a phase, and no ticks while
// the run is parked awaiting human input (needs_input is its own surface).
func TestHeartbeatSuppressedWhileParkedAndBeforeFirstPhase(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 15 * time.Millisecond
	defer func() { heartbeatInterval = prev }()

	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	r := &run{id: "run-hb-park", convID: "conv-hb-park", done: make(chan struct{}), status: "running", started: time.Now(), cancel: func() {}}
	e.registerRun(r)
	e.broker.ensure(r.id)

	stop := e.heartbeat(context.Background(), r)
	defer stop()

	// No phase yet: silent.
	if beats := collectHeartbeats(e, r.id, 0, 100*time.Millisecond); len(beats) != 0 {
		t.Fatalf("heartbeat ticked before any boundary set a phase: %d", len(beats))
	}

	// Parked awaiting input: silent.
	e.activity(r, phaseStack, "")
	r.setStatus("needs_input")
	r.beginAwait()
	lastSeq := e.broker.publish(r.id, "test.marker", "cody", nil).Seq
	if beats := collectHeartbeats(e, r.id, lastSeq, 100*time.Millisecond); len(beats) != 0 {
		t.Fatalf("heartbeat ticked while parked on needs_input: %d", len(beats))
	}

	// Resumed: ticks return.
	r.endAwait()
	r.setStatus("running")
	if beats := collectHeartbeats(e, r.id, lastSeq, 400*time.Millisecond); len(beats) == 0 {
		t.Fatal("heartbeat never resumed after the run un-parked")
	}
}

// TestHeartbeatStopsAtBoundaryAndOnCancel asserts the two shutdown paths are
// cancel-safe: the stop func halts ticking (the terminal boundary), and a ctx
// cancel halts an in-flight ticker with stop still returning cleanly.
func TestHeartbeatStopsAtBoundaryAndOnCancel(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 15 * time.Millisecond
	defer func() { heartbeatInterval = prev }()

	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	r := &run{id: "run-hb-stop", convID: "conv-hb-stop", done: make(chan struct{}), status: "running", started: time.Now(), cancel: func() {}}
	e.registerRun(r)
	e.broker.ensure(r.id)
	e.activity(r, phaseWorking, "task one")

	stop := e.heartbeat(context.Background(), r)
	waitUntil(t, "first heartbeat", func() bool {
		return len(collectHeartbeats(e, r.id, 0, 50*time.Millisecond)) > 0
	})
	stop()
	stop() // idempotent
	lastSeq := e.broker.publish(r.id, "test.marker", "cody", nil).Seq
	if beats := collectHeartbeats(e, r.id, lastSeq, 100*time.Millisecond); len(beats) != 0 {
		t.Fatalf("heartbeat ticked after stop: %d", len(beats))
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop2 := e.heartbeat(ctx, r)
	cancel()
	doneCh := make(chan struct{})
	go func() { stop2(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after ctx cancel")
	}
	lastSeq = e.broker.publish(r.id, "test.marker", "cody", nil).Seq
	if beats := collectHeartbeats(e, r.id, lastSeq, 100*time.Millisecond); len(beats) != 0 {
		t.Fatalf("heartbeat ticked after ctx cancel: %d", len(beats))
	}
}

// TestHeartbeatNeverPersistsToTrace asserts heartbeats are live-only: nothing
// a heartbeat publishes lands in the durable trace (they are liveness signals,
// not milestones — the reopen state comes from milestone activities).
func TestHeartbeatNeverPersistsToTrace(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 15 * time.Millisecond
	defer func() { heartbeatInterval = prev }()

	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	r := &run{id: "run-hb-trace", convID: "conv-hb-trace", done: make(chan struct{}), status: "running", started: time.Now(), cancel: func() {}}
	e.registerRun(r)
	e.broker.ensure(r.id)
	e.activity(r, phaseVerifying, "")

	stop := e.heartbeat(context.Background(), r)
	waitUntil(t, "heartbeats to tick", func() bool {
		return len(collectHeartbeats(e, r.id, 0, 50*time.Millisecond)) >= 2
	})
	stop()

	events, err := e.trace.load(r.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if hb, _ := ev.Fields["heartbeat"].(bool); hb {
			t.Fatalf("heartbeat event persisted to the durable trace: %+v", ev)
		}
	}
}
