// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"testing"
	"time"

	"matrix/cody/internal/llmtest"
)

// TestContinueResumesStoppedRunOverSameConversation drives the REAL continue
// path (req 3.2): a run stopped mid-plan is re-dispatched via a plain /chat
// re-submit on the same conversation, resumes the SAME durable plan under the
// SAME run id at the correct next task (t1 is never re-dispatched), re-emits
// live activity, streams live on the reopened topic, and finishes honestly.
func TestContinueResumesStoppedRunOverSameConversation(t *testing.T) {
	workspaceRoot, dataDir := t.TempDir(), t.TempDir()
	seedExistingProject(t, workspaceRoot)
	blockT2 := make(chan struct{})
	gw := llmtest.NewServer(t, gatewayScript(t, blockT2))
	t.Cleanup(gw.Close)

	e := newEngine(t, workspaceRoot, dataDir, gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(e.Close)

	conv := "conv-continue"
	runID, fresh, err := e.Submit(conv, "seed the demo workspace", "", "", "", "")
	if err != nil || !fresh {
		t.Fatalf("Submit: %v fresh=%v", err, fresh)
	}

	// t1 lands; t2 wedges on the blocked gateway turn. Stop mid-plan.
	waitUntil(t, "t1 accepted", func() bool {
		events, _ := e.trace.load(runID)
		for _, ev := range events {
			if ev.Type == "task.accepted" && ev.Fields["task_id"] == "t1" {
				return true
			}
		}
		return false
	})
	if !e.Stop(runID) {
		t.Fatal("Stop returned false")
	}
	waitUntil(t, "ledger to record the stop", func() bool {
		led, err := e.readLedger(conv)
		return err == nil && led.Status == "stopped"
	})
	// The stopped drive goroutine has fully unwound before the continue.
	r := e.lookupRun(runID)
	if r == nil {
		t.Fatal("stopped run vanished from the registry")
	}
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("stopped run never unwound")
	}
	close(blockT2) // the continued run's t2 turns proceed

	// Continue: a plain re-submit on the same conversation.
	preContinue, _ := e.trace.load(runID)
	contID, fresh, err := e.Submit(conv, "continue", "", "", "", "")
	if err != nil || !fresh {
		t.Fatalf("continue Submit: %v fresh=%v", err, fresh)
	}
	if contID != runID {
		t.Fatalf("continue run id = %q, want the same %q (trace + attach continuity)", contID, runID)
	}

	// The reopened topic streams live: a subscriber attached after the
	// continue receives events (the topic is no longer terminally closed).
	_, live, cancelSub := e.broker.subscribe(runID, len(preContinue))
	if live == nil {
		t.Fatal("topic still closed after continue: live subscribe refused")
	}
	cancelSub()

	waitUntil(t, "continued plan to complete", func() bool {
		led, err := e.readLedger(conv)
		return err == nil && led.Status == "completed"
	})

	events, err := e.trace.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	// The continued run re-emitted its acknowledgment + activity after the
	// stop, resumed at t2 (the correct next task), and never re-ran t1.
	t1Accepts, t2Accepts, startsAfter, activityAfter := 0, 0, 0, 0
	for i, ev := range events {
		switch ev.Type {
		case "task.accepted":
			switch ev.Fields["task_id"] {
			case "t1":
				t1Accepts++
			case "t2":
				t2Accepts++
			}
		case "run.started":
			if i >= len(preContinue) {
				startsAfter++
			}
		case "run.activity":
			if i >= len(preContinue) {
				activityAfter++
			}
		}
	}
	if t1Accepts != 1 {
		t.Fatalf("t1 accepted %d times, want exactly 1 (continue must not start over)", t1Accepts)
	}
	if t2Accepts != 1 {
		t.Fatalf("t2 accepted %d times, want 1", t2Accepts)
	}
	if startsAfter != 1 {
		t.Fatalf("continued run emitted %d run.started after the stop, want 1", startsAfter)
	}
	if activityAfter == 0 {
		t.Fatal("continued run re-emitted no run.activity")
	}
	// The conversation keeps its original title across the continue.
	led, err := e.readLedger(conv)
	if err != nil {
		t.Fatal(err)
	}
	if led.Title != "seed the demo workspace" {
		t.Fatalf("title after continue = %q, want the original", led.Title)
	}
}

// TestContinueCompletedRunIsHonest asserts continuing a COMPLETED conversation
// re-dispatches over the same durable plan and terminates honestly (everything
// already done — nothing re-runs) rather than dead-ending.
func TestContinueCompletedRunIsHonest(t *testing.T) {
	workspaceRoot, dataDir := t.TempDir(), t.TempDir()
	seedExistingProject(t, workspaceRoot)
	gw := llmtest.NewServer(t, gatewayScript(t, nil))
	t.Cleanup(gw.Close)

	e := newEngine(t, workspaceRoot, dataDir, gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(e.Close)

	conv := "conv-continue-done"
	runID, _, err := e.Submit(conv, "seed the demo workspace", "", "", "", "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntil(t, "first run to complete", func() bool {
		led, err := e.readLedger(conv)
		return err == nil && led.Status == "completed"
	})
	if r := e.lookupRun(runID); r != nil {
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Fatal("first run never unwound")
		}
	}

	before, _ := e.trace.load(runID)
	contID, fresh, err := e.Submit(conv, "continue", "", "", "", "")
	if err != nil || !fresh || contID != runID {
		t.Fatalf("continue = (%q, %v, %v), want (%q, true, nil)", contID, fresh, err, runID)
	}
	waitUntil(t, "continued run to terminate", func() bool {
		led, lerr := e.readLedger(conv)
		if lerr != nil || led.Status == "running" {
			return false
		}
		events, _ := e.trace.load(runID)
		return len(events) > len(before)
	})
	led, err := e.readLedger(conv)
	if err != nil {
		t.Fatal(err)
	}
	if led.Status != "completed" {
		t.Fatalf("continued completed run terminal = %q, want completed", led.Status)
	}
	// Nothing re-ran: no new task.accepted after the continue.
	events, _ := e.trace.load(runID)
	for _, ev := range events[len(before):] {
		if ev.Type == "task.accepted" {
			t.Fatalf("continue of a completed plan re-ran a task: %+v", ev.Fields)
		}
	}
}
