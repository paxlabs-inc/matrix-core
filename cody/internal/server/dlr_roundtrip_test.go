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

	"matrix/cody/internal/decide"
	"matrix/cody/internal/llmtest"
)

// uiPlanJSON is a scripted UI plan: a single landing-page task whose title
// carries UI keywords ("landing"/"page") so orchestrator.IsUITask fires and the
// run becomes UI-bearing — the signal that gates the Design Language Record.
const uiPlanJSON = `{
  "goal": "build the marketing landing page",
  "tasks": [
    {"id": "t1", "title": "Build the landing page", "goal": "index.html renders the landing page",
     "acceptance": ["index.html exists"], "wave": 1,
     "verify": ["test -f index.html"], "deliverable": {"shape": "index.html"}}
  ]
}`

// dlrCleanJSON is a Design Language Record with a specific, product-fitted
// direction: it clears the deterministic banned-defaults screen AND reads as
// intentional to the anti-default lens.
const dlrCleanJSON = `{
  "profile": {"product_kind": "realtime chat", "audience": "consumer beta testers", "brand_adjectives": "warm, direct, human", "surface": "web app"},
  "style_direction": "warm editorial with disciplined density",
  "typography": "Söhne for UI, Tiempos for display — a grotesque/serif pairing",
  "palette": ["ink #14110f", "bone #f4f1ea", "clay #b8975a"],
  "spacing": "8pt rhythm with generous editorial gutters",
  "motion": "restrained ease-out on message arrival",
  "component_idiom": "surfaces separated by tone, no border strokes",
  "rationale": "a consumer chat wants human warmth and legibility, not template polish"
}`

// dlrBannedJSON leans on a banned AI-tell (a purple gradient hero) so the
// deterministic banned-defaults screen (ScreenDesignBanned) rejects it.
const dlrBannedJSON = `{
  "profile": {"product_kind": "realtime chat", "audience": "consumer beta testers", "brand_adjectives": "modern", "surface": "web app"},
  "style_direction": "hero section with a purple gradient background",
  "typography": "Inter",
  "palette": ["violet", "white"],
  "spacing": "12pt",
  "motion": "none",
  "component_idiom": "stock cards",
  "rationale": "looks modern and clean"
}`

// cleanUIHTML is intentionally free of every banned tell so it survives the DLR
// drift screen (gate.ScreenDesign) over the changed .html file.
const cleanUIHTML = "<!doctype html><html><head><style>.btn{color:#111;background:#b8975a}</style></head><body><h1>Landing</h1></body></html>\n"

// dlrScript answers the DLR-specific model roles — the design lead author and
// the anti-AI-default design adjudicator — plus the planner (a UI plan), then
// drives a REAL worker turn (write the UI file + a screenshot artifact, verify,
// turn_in with the screenshot). Every other role delegates to the shared
// no-fakes recipe: real clients, real SSE, only decisions scripted.
//
// bannedFirst makes the design lead emit a banned record on the FIRST authoring
// pass and a clean one on the re-author (keyed on the rejection pressure the
// re-author brief carries), exercising the banned-defaults screen's real path.
// antiAccept toggles the anti-default lens verdict.
func dlrScript(t *testing.T, bannedFirst, antiAccept bool) func(step int, req llmtest.Request) llmtest.Turn {
	return func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		userText := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				userText += "\n" + m.Content
			}
		}
		switch {
		case strings.Contains(system, "planner"):
			return llmtest.Say(uiPlanJSON)
		case strings.Contains(system, "design lead"):
			if bannedFirst && !strings.Contains(userText, "prior attempt was rejected") {
				return llmtest.Say(dlrBannedJSON)
			}
			return llmtest.Say(dlrCleanJSON)
		case strings.Contains(system, "anti-AI-default design adjudicator"):
			if antiAccept {
				return llmtest.Say(`{"accept": true, "reason": "the direction turns on the consumer-chat profile"}`)
			}
			return llmtest.Say(`{"accept": false, "reason": "identical regardless of product"}`)
		case strings.Contains(system, "decision adjudicator"):
			return llmtest.Say(`{"pick": 0, "rationale": "first valid candidate"}`)
		case strings.Contains(system, "Cassandra"):
			return llmtest.Say(groundedVerdict)
		}
		// The worker loop: write the UI file, write a real screenshot artifact,
		// verify, then turn_in carrying the screenshot as evidence.
		toolResults := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults++
			}
		}
		switch toolResults {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "index.html", "content": cleanUIHTML})
		case 1:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "shot.png", "content": "PNGSCREENSHOTBYTES"})
		case 2:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "built the landing page",
				"screenshots": []string{"shot.png"},
				"changes":     []map[string]interface{}{{"path": "index.html", "kind": "create", "why": "the landing page"}},
			})
		}
	}
}

// TestDLRRoundTripEngineer proves reqs 9.1-9.4 + 12.1: a UI-bearing Engineer run
// authors a Design Language Record, PAUSES on the answer channel (blocking
// approve/override card), and only proceeds once the human approves — after
// which the DLR binds the UI sheet, the worker delivers a real UI file + a
// screenshot, and the run completes. The resolution is durable (dlr.json) and
// the decision.design / decision.resolved trace events are emitted.
func TestDLRRoundTripEngineer(t *testing.T) {
	workspaceRoot := t.TempDir()
	seedExistingProject(t, workspaceRoot) // non-greenfield → SDR OFF, only the DLR gates
	gw := llmtest.NewServer(t, dlrScript(t, false, true))
	t.Cleanup(gw.Close)
	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "build a landing page for a realtime chat app", "conversation_id": "conv-dlr"}`)
	runID := out["intent_id"].(string)

	// It gates: the run parks on needs_input having emitted decision.design, and
	// no UI task has been accepted yet — the DLR blocks the first UI task.
	waitAwaiting(t, engine, runID)
	events, _ := engine.trace.load(runID)
	if !hasType(events, "decision.design") {
		t.Fatalf("no decision.design before the pause: %v", eventTypes(events))
	}
	if hasType(events, "task.accepted") {
		t.Fatalf("a UI task was accepted before the DLR resolved: %v", eventTypes(events))
	}
	// The blocking card must be marked blocking in the technical register.
	var blocking interface{}
	for _, ev := range events {
		if ev.Type == "decision.design" {
			blocking = ev.Fields["blocking"]
		}
	}
	if blocking != true {
		t.Fatalf("Engineer decision.design blocking = %v, want true", blocking)
	}

	// Approve on the answer channel; the run binds the DLR and completes.
	resp, err := http.Post(srv.URL+"/intents/"+runID+"/answer", "application/json",
		strings.NewReader(`{"verdict": {"decision": "approve"}}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("answer: %v %v", resp, err)
	}
	resp.Body.Close()

	if status := waitCompleted(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal after approve = %q", status)
	}

	// The deliverables are real: the UI file and the screenshot artifact.
	if data, err := os.ReadFile(filepath.Join(workspaceRoot, "index.html")); err != nil || string(data) != cleanUIHTML {
		t.Fatalf("index.html = %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(workspaceRoot, "shot.png")); err != nil || info.Size() == 0 {
		t.Fatalf("screenshot artifact missing: %v", err)
	}

	// The decision resolved and the run reached its UI task.
	final, _ := engine.trace.load(runID)
	if !hasType(final, "decision.resolved") || !hasType(final, "task.accepted") || !hasType(final, "plan.completed") {
		t.Fatalf("post-approve trace missing resolution/acceptance/completion: %v", eventTypes(final))
	}

	// The resolution is durable so a resume never re-asks, and it records approve.
	sd, ok := engine.loadDesignDecision("conv-dlr")
	if !ok {
		t.Fatal("design decision was not persisted durably")
	}
	if sd.Decision != "approve" {
		t.Fatalf("persisted DLR decision = %q, want approve", sd.Decision)
	}
	if sd.Record == nil || !strings.Contains(sd.Record.StyleDirection, "editorial") {
		t.Fatalf("persisted DLR record = %+v", sd.Record)
	}
	// The bound constraint is the record's summary the UI sheet inherits.
	if !strings.Contains(sd.Constraint, "editorial") {
		t.Fatalf("DLR constraint did not bind the record: %q", sd.Constraint)
	}
}

// TestDLRPrototypeInformationalNonBlocking proves req 9.4: in Prototype the DLR
// is an informational, non-blocking card — the run authors it, emits a
// non-blocking decision.design, and completes WITHOUT ever parking on the answer
// channel. The record is persisted as "informational".
func TestDLRPrototypeInformationalNonBlocking(t *testing.T) {
	workspaceRoot := t.TempDir()
	seedExistingProject(t, workspaceRoot)
	gw := llmtest.NewServer(t, dlrScript(t, false, true))
	t.Cleanup(gw.Close)
	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "vibe out a landing page", "conversation_id": "conv-dlr-proto", "mode": "prototype"}`)
	runID := out["intent_id"].(string)

	// Prototype never pauses on the DLR: it runs straight to completion.
	if status := waitTerminal(t, srv.URL, runID); status != "completed" {
		t.Fatalf("prototype terminal = %q", status)
	}

	events, _ := engine.trace.load(runID)
	if !hasType(events, "decision.design") {
		t.Fatalf("prototype authored no DLR: %v", eventTypes(events))
	}
	// It never stalled for input — the card was informational.
	if hasType(events, "run.needs_input") {
		t.Fatalf("prototype DLR blocked on the answer channel: %v", eventTypes(events))
	}
	// The decision.design card is marked non-blocking in the outcome register.
	for _, ev := range events {
		if ev.Type == "decision.design" && ev.Fields["blocking"] != false {
			t.Fatalf("prototype decision.design blocking = %v, want false", ev.Fields["blocking"])
		}
	}
	sd, ok := engine.loadDesignDecision("conv-dlr-proto")
	if !ok {
		t.Fatal("prototype DLR was not persisted")
	}
	if sd.Decision != "informational" {
		t.Fatalf("prototype DLR decision = %q, want informational", sd.Decision)
	}
}

// TestDLRBannedDefaultsRejected proves req 9.2 at BOTH levels with real code
// paths: (a) the deterministic banned-defaults screen rejects a banned record
// and does so identically on repeat (no model in the loop), and (b) through the
// real resolveDesignDecision seam (a Prototype run), a design lead that first
// emits a banned record has it rejected and re-authored — the PERSISTED DLR is
// the clean re-authored record, never the banned default.
func TestDLRBannedDefaultsRejected(t *testing.T) {
	// (a) Deterministic screen, real decide code — the banned record cannot pass.
	var banned decide.DesignRecord
	if err := json.Unmarshal([]byte(dlrBannedJSON), &banned); err != nil {
		t.Fatal(err)
	}
	reject := decide.ScreenDesignBanned(&banned)
	if reject == "" {
		t.Fatal("banned record passed the deterministic screen")
	}
	if reject2 := decide.ScreenDesignBanned(&banned); reject2 != reject {
		t.Fatalf("non-deterministic screen: %q != %q", reject2, reject)
	}
	var clean decide.DesignRecord
	if err := json.Unmarshal([]byte(dlrCleanJSON), &clean); err != nil {
		t.Fatal(err)
	}
	if v := decide.ScreenDesignBanned(&clean); v != "" {
		t.Fatalf("clean record rejected by the screen: %s", v)
	}

	// (b) The real server seam: banned first → rejected → re-authored clean.
	workspaceRoot := t.TempDir()
	seedExistingProject(t, workspaceRoot)
	gw := llmtest.NewServer(t, dlrScript(t, true /*bannedFirst*/, true))
	t.Cleanup(gw.Close)
	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "vibe out a landing page", "conversation_id": "conv-dlr-banned", "mode": "prototype"}`)
	runID := out["intent_id"].(string)
	if status := waitTerminal(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal = %q", status)
	}
	sd, ok := engine.loadDesignDecision("conv-dlr-banned")
	if !ok {
		t.Fatal("no DLR persisted")
	}
	if sd.Record == nil {
		t.Fatal("persisted DLR has no record")
	}
	// The banned default did not pass the gate: what persisted is the clean
	// re-authored record, and it independently clears the banned screen.
	if strings.Contains(strings.ToLower(sd.Record.StyleDirection), "purple") {
		t.Fatalf("banned default survived into the persisted DLR: %+v", sd.Record)
	}
	if v := decide.ScreenDesignBanned(sd.Record); v != "" {
		t.Fatalf("persisted DLR still leans on a banned tell: %s", v)
	}
}
