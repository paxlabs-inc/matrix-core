// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/orchestrator"
)

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestPauseForInputResolvedByAnswer drives the REAL pause/resume plumbing: a run
// parked on needs_input blocks until /answer delivers, and both the stall and
// its resolution are durable trace events (req 12.1, 12.3).
func TestPauseForInputResolvedByAnswer(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	conv, runID := "conv-pause", "run-pausetest"
	if err := e.writeLedger(conv, ledger{RunID: runID, Message: "m", Mode: "engineer", Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	e.broker.ensure(runID)
	r := &run{id: runID, convID: conv, done: make(chan struct{}), status: "running", cancel: func() {}}
	e.registerRun(r)

	type result struct {
		d  directive
		ok bool
	}
	resCh := make(chan result, 1)
	go func() {
		d, ok := e.pauseForInput(context.Background(), r, "which database should I use?")
		resCh <- result{d, ok}
	}()

	waitUntil(t, "run to park on needs_input", func() bool {
		return r.getStatus() == "needs_input" && r.isAwaiting()
	})
	// The stall is durable and recorded.
	if in := e.readInbox(conv); in.Pending == nil || in.Pending.Prompt == "" {
		t.Fatalf("pending question not recorded: %+v", in.Pending)
	}

	if err := e.Answer(runID, directive{Text: "postgres"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	got := <-resCh
	if !got.ok || got.d.Text != "postgres" {
		t.Fatalf("resume = %+v, want the answer 'postgres'", got)
	}
	// Pending cleared on resolution.
	if in := e.readInbox(conv); in.Pending != nil {
		t.Fatalf("pending question not cleared after answer: %+v", in.Pending)
	}
	// Both lifecycle events are durable.
	events, err := e.trace.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	var sawNeeds, sawAnswered bool
	for _, ev := range events {
		switch ev.Type {
		case "run.needs_input":
			sawNeeds = true
		case "run.answered":
			sawAnswered = true
		}
	}
	if !sawNeeds || !sawAnswered {
		t.Fatalf("durable trace missing lifecycle events: needs_input=%v answered=%v", sawNeeds, sawAnswered)
	}
}

// TestAnswerRejectedWhenNotAwaiting proves /answer on a running (not stalled)
// run is a 409, never a silent no-op.
func TestAnswerRejectedWhenNotAwaiting(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	conv, runID := "conv-run", "run-running"
	if err := e.writeLedger(conv, ledger{RunID: runID, Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := e.Answer(runID, directive{Text: "x"}); err == nil {
		t.Fatal("answering a running (non-stalled) run should error")
	}
}

// TestApplyAnswerResetsFailedTasks proves the resume computation: an answer
// records the directive durably and resets the failed task so the orchestrator
// re-picks it.
func TestApplyAnswerResetsFailedTasks(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	conv := "conv-apply"
	st, err := contract.OpenStore(e.planDir(conv))
	if err != nil {
		t.Fatal(err)
	}
	plan := &orchestrator.Plan{Goal: "g", Tasks: []*orchestrator.Task{
		{ID: "t1", Title: "x", Wave: 1, Status: orchestrator.TaskFailed,
			Goal: "g", Acceptance: []string{"a"}, Verify: []string{"true"},
			Deliverable: contract.Deliverable{Shape: "s"}},
	}}
	if err := orchestrator.SavePlan(st.Root(), plan); err != nil {
		t.Fatal(err)
	}
	if err := e.applyAnswer(conv, st, plan, directive{Kind: "answer", Text: "try sqlite instead"}); err != nil {
		t.Fatal(err)
	}
	if plan.Tasks[0].Status != orchestrator.TaskPending {
		t.Fatalf("failed task not reset: %s", plan.Tasks[0].Status)
	}
	if texts := e.directiveTexts(conv); len(texts) != 1 || texts[0] != "try sqlite instead" {
		t.Fatalf("directive not recorded: %v", texts)
	}
	// The reset is durable.
	reloaded, err := orchestrator.LoadPlan(st.Root())
	if err != nil || reloaded.Tasks[0].Status != orchestrator.TaskPending {
		t.Fatalf("reset not persisted: %+v, %v", reloaded, err)
	}
}

// TestSteerRecordsDurableDirective proves POST-level Steer folds onto the
// durable inbox and surfaces via the orchestrator's Steers provider, resolving
// the run id through the durable-ledger scan (cold path).
func TestSteerRecordsDurableDirective(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	conv, runID := "conv-steer", "run-steer"
	if err := e.writeLedger(conv, ledger{RunID: runID, Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := e.Steer(runID, "  use React Router, not Next  "); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	texts := e.directiveTexts(conv)
	if len(texts) != 1 || texts[0] != "use React Router, not Next" {
		t.Fatalf("steer not folded: %v", texts)
	}
	if err := e.Steer("run-nope", "x"); err == nil {
		t.Fatal("steer on unknown intent should error")
	}
}

// TestAnswerSteerRoutesValidate proves the HTTP surface: unknown intent 404s,
// an empty answer 400s, a bad verdict 400s, and a valid steer 200s.
func TestAnswerSteerRoutesValidate(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	conv, runID := "conv-http", "run-http"
	if err := e.writeLedger(conv, ledger{RunID: runID, Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	post := func(path, body string) int {
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("/intents/ghost/answer", `{"text":"x"}`); code != http.StatusNotFound {
		t.Fatalf("answer unknown intent = %d, want 404", code)
	}
	if code := post("/intents/ghost/steer", `{"text":"x"}`); code != http.StatusNotFound {
		t.Fatalf("steer unknown intent = %d, want 404", code)
	}
	if code := post("/intents/"+runID+"/answer", `{}`); code != http.StatusBadRequest {
		t.Fatalf("empty answer = %d, want 400", code)
	}
	if code := post("/intents/"+runID+"/answer", `{"verdict":{"decision":"bogus"}}`); code != http.StatusBadRequest {
		t.Fatalf("bad verdict = %d, want 400", code)
	}
	// A running (not stalled) run cannot be answered → 409.
	if code := post("/intents/"+runID+"/answer", `{"text":"go"}`); code != http.StatusConflict {
		t.Fatalf("answer on running run = %d, want 409", code)
	}
	// A steer on a known run is accepted.
	if code := post("/intents/"+runID+"/steer", `{"text":"use tabs"}`); code != http.StatusOK {
		t.Fatalf("valid steer = %d, want 200", code)
	}
	if texts := e.directiveTexts(conv); len(texts) != 1 || texts[0] != "use tabs" {
		t.Fatalf("steer not recorded via HTTP: %v", texts)
	}
}
