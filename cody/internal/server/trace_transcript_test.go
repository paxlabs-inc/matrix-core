// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"testing"
	"time"

	"matrix/cody/internal/llmtest"
)

// TestTranscriptPersistsToDurableTrace drives a COMPLETE real run (real
// engine, real drive, real orchestrator + workers over the scripted gateway)
// and asserts total persistence (req 4.1): the durable trace carries the
// user's initiating turn (chat.user), the run acknowledgment (run.started),
// the milestone activity spine (run.activity, understanding through
// verifying), and Cody's messages (chat.assistant) — with zero heartbeat
// ticks persisted.
func TestTranscriptPersistsToDurableTrace(t *testing.T) {
	workspaceRoot, dataDir := t.TempDir(), t.TempDir()
	seedExistingProject(t, workspaceRoot)
	gw := llmtest.NewServer(t, gatewayScript(t, nil))
	t.Cleanup(gw.Close)

	e := newEngine(t, workspaceRoot, dataDir, gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(e.Close)

	runID, fresh, err := e.Submit("conv-transcript", "seed the demo workspace", "", "", "", "")
	if err != nil || !fresh {
		t.Fatalf("Submit: %v fresh=%v", err, fresh)
	}
	waitUntil(t, "plan to complete", func() bool {
		events, _ := e.trace.load(runID)
		for _, ev := range events {
			if ev.Type == "plan.completed" {
				return true
			}
		}
		return false
	})

	events, err := e.trace.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	var sawUserTurn, sawStarted, sawAssistant bool
	phases := map[string]bool{}
	for _, ev := range events {
		switch ev.Type {
		case "chat.user":
			if kind, _ := ev.Fields["kind"].(string); kind == "message" {
				if text, _ := ev.Fields["text"].(string); text == "seed the demo workspace" {
					sawUserTurn = true
				}
			}
		case "run.started":
			if m, _ := ev.Fields["mode"].(string); m == "engineer" {
				sawStarted = true
			}
		case "run.activity":
			if hb, _ := ev.Fields["heartbeat"].(bool); hb {
				t.Fatalf("heartbeat tick persisted to the durable trace: %+v", ev)
			}
			phase, _ := ev.Fields["phase"].(string)
			phases[phase] = true
			if _, ok := ev.Fields["label"].(string); !ok {
				t.Fatalf("persisted activity missing label: %+v", ev.Fields)
			}
		case "chat.assistant":
			if text, _ := ev.Fields["text"].(string); text != "" {
				sawAssistant = true
			}
		}
	}
	if !sawUserTurn {
		t.Fatal("initiating chat.user turn not persisted")
	}
	if !sawStarted {
		t.Fatal("run.started not persisted")
	}
	if !sawAssistant {
		t.Fatal("chat.assistant not persisted")
	}
	for _, want := range []string{phaseUnderstanding, phasePlanning, phaseWorking, phaseVerifying} {
		if !phases[want] {
			t.Fatalf("milestone activity phase %q not persisted; got %v", want, phases)
		}
	}
	// The user's turn precedes the acknowledgment in the timeline.
	userIdx, startedIdx := -1, -1
	for i, ev := range events {
		if ev.Type == "chat.user" && userIdx == -1 {
			userIdx = i
		}
		if ev.Type == "run.started" && startedIdx == -1 {
			startedIdx = i
		}
	}
	if userIdx > startedIdx {
		t.Fatalf("chat.user (%d) should precede run.started (%d)", userIdx, startedIdx)
	}
}

// TestSteerAndAnswerPersistAsUserTurns asserts the other two user inputs are
// durable chat.user turns (req 4.1): a steer while running, and an answer to
// a needs_input stall (both the hot in-process path and its durable event).
func TestSteerAndAnswerPersistAsUserTurns(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	conv, runID := "conv-turns", "run-turns"
	if err := e.writeLedger(conv, ledger{RunID: runID, Message: "m", Mode: "engineer", Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	e.broker.ensure(runID)
	r := &run{id: runID, convID: conv, done: make(chan struct{}), status: "running", cancel: func() {}}
	e.registerRun(r)

	if err := e.Steer(runID, "use postgres not sqlite"); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	type result struct {
		d  directive
		ok bool
	}
	resCh := make(chan result, 1)
	go func() {
		d, ok := e.pauseForInput(context.Background(), r, "which port?")
		resCh <- result{d, ok}
	}()
	waitUntil(t, "run to park", func() bool { return r.isAwaiting() })
	if err := e.Answer(runID, directive{Text: "8080", Verdict: &verdict{Decision: "approve"}}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got := <-resCh; !got.ok || got.d.Text != "8080" {
		t.Fatalf("resume = %+v", got)
	}

	events, err := e.trace.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	var sawSteer, sawAnswer bool
	for _, ev := range events {
		if ev.Type != "chat.user" {
			continue
		}
		kind, _ := ev.Fields["kind"].(string)
		text, _ := ev.Fields["text"].(string)
		switch kind {
		case "steer":
			if text == "use postgres not sqlite" {
				sawSteer = true
			}
		case "answer":
			decision, _ := ev.Fields["decision"].(string)
			if text == "8080" && decision == "approve" {
				sawAnswer = true
			}
		}
	}
	if !sawSteer {
		t.Fatal("steer chat.user turn not persisted")
	}
	if !sawAnswer {
		t.Fatal("answer chat.user turn not persisted")
	}
}
