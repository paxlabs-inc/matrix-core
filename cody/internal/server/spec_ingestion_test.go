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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"matrix/cody/internal/llmtest"
)

// specDoc is a real specification document seeded into the workspace: it names
// an overall goal and two requirements with their own acceptance wording and an
// explicit dependency (R1 after G1). The adopt planner maps THIS document onto
// the plan — the plan's goal and tasks are derived from it, not from the prose
// chat message.
const specDoc = `# Greeting Service Specification

Goal: ship a two-file greeting exchange.

## Requirement G1: greeting file (wave 1)
greet.txt must exist containing "hello cody".

## Requirement R1: reply file (wave 2, after G1)
reply.txt must exist containing "hello back".
`

// adoptedPlanJSON is the plan the scripted adopt-planner returns when handed the
// spec above: the document's goal line becomes the plan goal, and its two
// requirements (with their own wave/dependency) become tasks t1 and t2 with the
// document's acceptance wording. Task ids t1/t2 keep the shared worker loop's
// file mapping (greet.txt / reply.txt) intact.
const adoptedPlanJSON = `{
  "goal": "ship a two-file greeting exchange",
  "tasks": [
    {"id": "t1", "title": "Create greet.txt (G1)", "goal": "greet.txt exists containing 'hello cody'",
     "acceptance": ["greet.txt must exist containing hello cody"], "wave": 1,
     "verify": ["grep -q \"hello cody\" greet.txt"], "deliverable": {"shape": "greet.txt"}},
    {"id": "t2", "title": "Create reply.txt (R1)", "goal": "reply.txt exists containing 'hello back'",
     "acceptance": ["reply.txt must exist containing hello back"], "wave": 2, "requires": ["t1"],
     "verify": ["grep -q \"hello back\" reply.txt"], "deliverable": {"shape": "reply.txt"}}
  ]
}`

// adoptScript answers every model role for an ADOPTED-spec run from one scripted
// endpoint, the same no-fakes recipe as gatewayScript (real clients, real SSE,
// only decisions scripted). The one difference: the planner half is the ADOPT
// planner — it fires only on the adopt system prompt ("ADOPTING an existing
// SPECIFICATION DOCUMENT"), asserts the actual spec document reached it, and
// returns a plan MAPPED from that document. adoptCalls counts adopt-planner
// generations so a resume can prove the durable plan carried it (0 re-adopts);
// blockT2 (optional) hangs t2's worker to simulate a mid-plan kill.
func adoptScript(t *testing.T, adoptCalls *int32, blockT2 chan struct{}) func(step int, req llmtest.Request) llmtest.Turn {
	return func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		switch {
		case strings.Contains(system, "ADOPTING an existing SPECIFICATION DOCUMENT"):
			// The adopt planner: the document must have reached it verbatim.
			user := ""
			for _, m := range req.Messages {
				if m.Role == "user" {
					user = m.Content
				}
			}
			if !strings.Contains(user, "ship a two-file greeting exchange") {
				t.Errorf("adopt prompt did not carry the spec document: %q", user)
			}
			if adoptCalls != nil {
				atomic.AddInt32(adoptCalls, 1)
			}
			return llmtest.Say(adoptedPlanJSON)
		case strings.Contains(system, "decision adjudicator"):
			return llmtest.Say(`{"pick": 0, "rationale": "first faithful adoption"}`)
		case strings.Contains(system, "Cassandra"):
			return llmtest.Say(groundedVerdict)
		}
		// The worker loop — identical to gatewayScript's.
		sheetPrompt := ""
		toolResults := 0
		for _, m := range req.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "TASK SHEET") {
				sheetPrompt = m.Content
			}
			if m.Role == "tool" {
				toolResults++
			}
		}
		file, content := "greet.txt", "hello cody\n"
		if strings.Contains(sheetPrompt, "TASK SHEET t2") {
			if blockT2 != nil {
				<-blockT2
			}
			file, content = "reply.txt", "hello back\n"
		}
		switch toolResults {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": file, "content": content})
		case 1:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "created " + file,
				"changes": []map[string]interface{}{{"path": file, "kind": "create", "why": "the deliverable"}},
			})
		}
	}
}

// TestSpecIngestionAdoptThenResume proves reqs 11.1 + 11.2: a SPEC.md seeded in
// the workspace is ingested by path over POST /chat, the planner ADOPTS it (the
// plan.adopted event names the source path and the plan goal is derived from the
// document — not from the prose message), and the spec is durable. Then codyd is
// killed mid-plan (t2 wedged, ledger still "running" — a hard process death) and
// a FRESH engine over the SAME dataDir/workspace/cortex resumes the ADOPTED plan
// via ResumeOrphanedPlans, continuing at the correct next task WITHOUT ever
// re-invoking the adopt planner: the durable plan carries the resume.
func TestSpecIngestionAdoptThenResume(t *testing.T) {
	workspaceRoot := t.TempDir()
	seedExistingProject(t, workspaceRoot) // a spec adoption never gates on the greenfield SDR
	if err := os.WriteFile(filepath.Join(workspaceRoot, "SPEC.md"), []byte(specDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	ctx := openCortex(t, t.TempDir())

	// --- engine #1: adopt over HTTP, then wedge t2 -------------------------
	var adoptCalls1 int32
	blockT2 := make(chan struct{})
	gw1 := llmtest.NewServer(t, adoptScript(t, &adoptCalls1, blockT2))
	t.Cleanup(gw1.Close)

	engine1 := newEngine(t, workspaceRoot, dataDir, gw1.URL, ctx)
	// LIFO cleanup: unwedge the worker first, then Close cancels + waits.
	t.Cleanup(engine1.Close)
	t.Cleanup(func() { close(blockT2) })
	srv1 := httptest.NewServer(New(engine1).Handler())
	t.Cleanup(srv1.Close)

	// Ingest by PATH: /chat resolves spec_path against the project workspace.
	out := postChat(t, srv1.URL, `{"message": "adopt the attached spec", "conversation_id": "conv-adopt", "spec_path": "SPEC.md"}`)
	runID1 := out["intent_id"].(string)

	// Wait until t1 is accepted (t2 is wedged mid-flight).
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("t1 never accepted")
		}
		events, err := engine1.trace.load(runID1)
		accepted := false
		if err == nil {
			for _, ev := range events {
				if ev.Type == "task.accepted" && ev.Fields["task_id"] == "t1" {
					accepted = true
				}
			}
		}
		if accepted {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The plan was ADOPTED from the document: the event names the source path
	// and the goal is the document's goal, not the prose message.
	events, _ := engine1.trace.load(runID1)
	var adoptedEv *Event
	for i := range events {
		if events[i].Type == "plan.adopted" {
			adoptedEv = &events[i]
		}
	}
	if adoptedEv == nil {
		t.Fatalf("no plan.adopted event: %v", eventTypes(events))
	}
	if src, _ := adoptedEv.Fields["source"].(string); src != "SPEC.md" {
		t.Fatalf("plan.adopted source = %q, want SPEC.md", src)
	}
	if goal, _ := adoptedEv.Fields["goal"].(string); goal != "ship a two-file greeting exchange" {
		t.Fatalf("plan.adopted goal = %q, want the document's goal (not the prose message)", goal)
	}
	if n := atomic.LoadInt32(&adoptCalls1); n == 0 {
		t.Fatal("the adopt planner was never invoked on the first run")
	}

	// The kill: engine #1 is abandoned with t2 wedged; the ledger still says
	// running (the exact durable state a hard process death leaves) and it
	// records the adopted spec so a resume re-grounds against it.
	led, err := engine1.readLedger("conv-adopt")
	if err != nil || led.Status != "running" {
		t.Fatalf("pre-kill ledger = %+v, %v (want running)", led, err)
	}
	if !strings.Contains(led.Spec, "ship a two-file greeting exchange") || led.SpecSource != "SPEC.md" {
		t.Fatalf("ledger did not durably record the adopted spec: spec=%q source=%q", led.Spec, led.SpecSource)
	}

	// --- engine #2: a fresh codyd resumes the ADOPTED plan -----------------
	var adoptCalls2 int32
	gw2 := llmtest.NewServer(t, adoptScript(t, &adoptCalls2, nil))
	t.Cleanup(gw2.Close)

	engine2 := newEngine(t, workspaceRoot, dataDir, gw2.URL, ctx)
	t.Cleanup(engine2.Close)
	if n := engine2.ResumeOrphanedPlans(); n != 1 {
		t.Fatalf("ResumeOrphanedPlans = %d, want 1", n)
	}

	// The resumed run reuses the durable run id and completes.
	deadline = time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("resumed run never completed")
		}
		if led, err := engine2.readLedger("conv-adopt"); err == nil && led.Status != "running" {
			if led.Status != "completed" {
				t.Fatalf("resumed terminal = %q", led.Status)
			}
			if led.RunID != runID1 {
				t.Fatalf("resumed run id = %q, want the durable %q", led.RunID, runID1)
			}
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The durable plan carried the resume: the adopt planner was NOT re-invoked.
	if n := atomic.LoadInt32(&adoptCalls2); n != 0 {
		t.Fatalf("adopt planner re-invoked %d time(s) on resume; the durable plan must carry it", n)
	}
	// Both deliverables are real.
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(filepath.Join(workspaceRoot, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}
}

// steerGateScript is gatewayScript with a gate on t1's first worker turn: it
// signals t1Started (once) and blocks on releaseT1, holding the live t1 worker
// open so the test can POST a steer over HTTP and prove the steer lands in the
// NEXT sheet (t2) at the orchestrator boundary — after t1 returns — never
// killing the live worker.
func steerGateScript(t *testing.T, t1Started *sync.Once, started, releaseT1 chan struct{}) func(step int, req llmtest.Request) llmtest.Turn {
	base := gatewayScript(t, nil)
	return func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		// Only the worker loop gates; the planner/adjudicator/Cassandra pass through.
		if !strings.Contains(system, "planner") && !strings.Contains(system, "adjudicator") && !strings.Contains(system, "Cassandra") {
			sheetPrompt, toolResults := "", 0
			for _, m := range req.Messages {
				if m.Role == "user" && strings.Contains(m.Content, "TASK SHEET") {
					sheetPrompt = m.Content
				}
				if m.Role == "tool" {
					toolResults++
				}
			}
			if strings.Contains(sheetPrompt, "TASK SHEET t1") && toolResults == 0 {
				t1Started.Do(func() { close(started) })
				<-releaseT1 // hold t1's worker alive across the steer POST
			}
		}
		return base(step, req)
	}
}

// TestSteerOverHTTPFoldsIntoNextSheet is req 12.2's server/HTTP arm (the
// orchestrator-level fold is proven in orchestrator.TestSteerFoldsIntoNextSheet-
// WithoutKillingWorker). A steer POSTed over the live /intents/{id}/steer route
// while t1's worker is alive returns 200, never interrupts that worker (t1 and
// t2 both complete), and surfaces exactly once as a durable steer.folded event.
func TestSteerOverHTTPFoldsIntoNextSheet(t *testing.T) {
	workspaceRoot := t.TempDir()
	seedExistingProject(t, workspaceRoot)

	var once sync.Once
	started := make(chan struct{})
	releaseT1 := make(chan struct{})
	gw := llmtest.NewServer(t, steerGateScript(t, &once, started, releaseT1))
	t.Cleanup(gw.Close)

	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	// Ensure the wedged t1 worker is released even if an assertion fails early.
	t.Cleanup(func() { once.Do(func() { close(started) }); safeClose(releaseT1) })
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "seed the demo workspace", "conversation_id": "conv-steer"}`)
	runID := out["intent_id"].(string)

	// Wait until t1's worker is alive, then steer WHILE it runs.
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("t1 worker never started")
	}
	const steerText = "prefer tabs over spaces"
	resp, err := http.Post(srv.URL+"/intents/"+runID+"/steer", "application/json",
		strings.NewReader(`{"text": "`+steerText+`"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("steer POST = %v %v", resp, err)
	}
	var steerOut struct {
		Steered bool `json:"steered"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&steerOut)
	resp.Body.Close()
	if !steerOut.Steered {
		t.Fatal("steer POST did not report steered:true")
	}
	// The steer is now durably in the inbox — release the live t1 worker so the
	// fold happens at the t2 boundary that follows.
	close(releaseT1)

	if status := waitTerminal(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal after steer = %q (the steer must not kill the live worker)", status)
	}

	events, _ := engine.trace.load(runID)
	var folds int
	sawT1, sawT2 := false, false
	for _, ev := range events {
		switch ev.Type {
		case "steer.folded":
			if txt, _ := ev.Fields["text"].(string); txt == steerText {
				folds++
			}
		case "task.accepted":
			switch ev.Fields["task_id"] {
			case "t1":
				sawT1 = true
			case "t2":
				sawT2 = true
			}
		}
	}
	if !sawT1 || !sawT2 {
		t.Fatalf("both tasks must complete despite the steer: t1=%v t2=%v (%v)", sawT1, sawT2, eventTypes(events))
	}
	if folds != 1 {
		t.Fatalf("steer.folded events for %q = %d, want exactly 1", steerText, folds)
	}
	// Both deliverables are real — the live worker ran to completion.
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(filepath.Join(workspaceRoot, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}
}

// safeClose closes a channel at most once, tolerating an already-closed channel.
func safeClose(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}
