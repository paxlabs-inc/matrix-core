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
	"time"

	"matrix/cody/internal/llmtest"
)

// sdrJSON is a scripted Stack Decision Record: a realtime-chat profile with two
// genuinely divergent candidates, picking the doctrine-fit one.
const sdrJSON = `{
  "profile": {"app_class": "realtime chat", "audience": "consumer beta testers", "deployment_target": "Railway", "team_context": "solo"},
  "candidates": [
    {"name": "Remix (React Router framework mode) on Node/TS", "rationale": "doctrine maps realtime/collaborative to React Router framework mode; SSR + nested routing fit a chat surface"},
    {"name": "Astro static site", "rationale": "content-first framework; a poor fit for a stateful realtime chat", "deviates_from_doctrine": true, "deviation_rationale": "chosen only to contrast; not recommended"}
  ],
  "pick": 0,
  "rationale": "the realtime chat profile maps to React Router framework mode per doctrine; Astro is content-first and does not fit a stateful surface"
}`

// sdrScript answers the two SDR-specific model roles (the stack architect author
// and the anti-default adjudicator) and delegates every other role to the
// shared gatewayScript — the same real-clients/real-SSE, only-decisions-scripted
// recipe. accept toggles the anti-default lens verdict.
func sdrScript(t *testing.T, accept bool) func(step int, req llmtest.Request) llmtest.Turn {
	base := gatewayScript(t, nil)
	return func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		switch {
		case strings.Contains(system, "stack architect"):
			return llmtest.Say(sdrJSON)
		case strings.Contains(system, "anti-default stack adjudicator"):
			if accept {
				return llmtest.Say(`{"accept": true, "reason": "the pick turns on the realtime chat profile"}`)
			}
			return llmtest.Say(`{"accept": false, "reason": "the pick would be identical for any profile"}`)
		}
		return base(step, req)
	}
}

// waitCompleted blocks until the run reaches a genuine terminal (completed /
// failed / stopped), treating the transient needs_input and running states as
// non-terminal — a resolved decision briefly re-parks status before drive flips
// it back to running, which the coarse waitTerminal would mis-read as terminal.
func waitCompleted(t *testing.T, base, runID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/messages/async/" + runID)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		switch out.Status {
		case "completed", "failed", "stopped":
			return out.Status
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("run never reached a genuine terminal status")
	return ""
}

// waitAwaiting blocks until the run is parked on the answer channel (so the hot
// /answer path, not the cold restart path, resolves it).
func waitAwaiting(t *testing.T, e *Engine, runID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r := e.lookupRun(runID); r != nil && r.isAwaiting() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run never parked on the answer channel")
}

// TestStackDecisionGatesGreenfieldEngineer proves req 8.1-8.3: a greenfield
// Engineer run authors a Stack Decision Record and PAUSES before any plan
// exists — wave 1 is unreachable until the human resolves it — then an approve
// on the answer channel lets planning proceed and the run completes for real.
func TestStackDecisionGatesGreenfieldEngineer(t *testing.T) {
	workspaceRoot := t.TempDir() // greenfield: no seed
	gw := llmtest.NewServer(t, sdrScript(t, true))
	t.Cleanup(gw.Close)
	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "build a realtime chat app", "conversation_id": "conv-sdr"}`)
	runID := out["intent_id"].(string)

	// It gates: the run parks on needs_input having emitted decision.stack, and
	// NO task has started — there is no plan (hence no wave 1) yet.
	waitAwaiting(t, engine, runID)
	events, _ := engine.trace.load(runID)
	sawStack, sawTaskStarted, sawPlan := false, false, false
	for _, ev := range events {
		switch ev.Type {
		case "decision.stack":
			sawStack = true
		case "task.started":
			sawTaskStarted = true
		case "plan.created":
			sawPlan = true
		}
	}
	if !sawStack {
		t.Fatalf("no decision.stack before the pause: %v", eventTypes(events))
	}
	if sawTaskStarted || sawPlan {
		t.Fatalf("wave 1 was reachable before the SDR resolved: %v", eventTypes(events))
	}

	// Approve on the answer channel; planning proceeds and the run completes.
	resp, err := http.Post(srv.URL+"/intents/"+runID+"/answer", "application/json",
		strings.NewReader(`{"verdict": {"decision": "approve"}}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("answer: %v %v", resp, err)
	}
	resp.Body.Close()

	if status := waitCompleted(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal after approve = %q", status)
	}
	// The decision resolved and the deliverables are real.
	final, _ := engine.trace.load(runID)
	if !hasType(final, "decision.resolved") || !hasType(final, "plan.created") {
		t.Fatalf("post-approve trace missing resolution/plan: %v", eventTypes(final))
	}
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(filepath.Join(workspaceRoot, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}
	// The resolution is durable so a resume never re-asks.
	if _, ok := engine.loadStackDecision("conv-sdr"); !ok {
		t.Fatal("stack decision was not persisted durably")
	}
}

// TestPrototypeSkipsStackDecision proves req 8.4: a greenfield Prototype run
// never authors an SDR — it plans and completes with no decision.stack event.
func TestPrototypeSkipsStackDecision(t *testing.T) {
	workspaceRoot := t.TempDir() // greenfield
	gw := llmtest.NewServer(t, sdrScript(t, true))
	t.Cleanup(gw.Close)
	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "vibe out a quick app", "conversation_id": "conv-proto", "mode": "prototype"}`)
	runID := out["intent_id"].(string)
	if status := waitTerminal(t, srv.URL, runID); status != "completed" {
		t.Fatalf("prototype terminal = %q", status)
	}
	events, _ := engine.trace.load(runID)
	if hasType(events, "decision.stack") {
		t.Fatalf("prototype authored an SDR (it must skip): %v", eventTypes(events))
	}
	if _, ok := engine.loadStackDecision("conv-proto"); ok {
		t.Fatal("prototype persisted a stack decision (it must skip)")
	}
}

func hasType(events []Event, typ string) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}
