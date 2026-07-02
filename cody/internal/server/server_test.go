// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/cody/internal/llmtest"
	"matrix/cody/internal/mode"
	cortex "matrix/cortex"
	"matrix/cortex/store"
)

// planJSON is the scripted planner output: the two-file demo plan.
const planJSON = `{
  "goal": "seed the demo workspace",
  "tasks": [
    {"id": "t1", "title": "Create greet.txt", "goal": "greet.txt exists containing 'hello cody'",
     "acceptance": ["greet.txt contains hello cody"], "wave": 1,
     "verify": ["grep -q \"hello cody\" greet.txt"], "deliverable": {"shape": "greet.txt"}},
    {"id": "t2", "title": "Create reply.txt", "goal": "reply.txt exists containing 'hello back'",
     "acceptance": ["reply.txt contains hello back"], "wave": 2, "requires": ["t1"],
     "verify": ["grep -q \"hello back\" reply.txt"], "deliverable": {"shape": "reply.txt"}}
  ]
}`

const groundedVerdict = `{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": [], "certainty": 0.9}`

// gatewayScript answers every role of the cody slot from ONE scripted
// endpoint: the planner (by its system prompt), the Cassandra adjudicator (by
// its system prompt), and the worker tool loop (by the live message window) —
// the same no-fakes recipe as everywhere else: real clients, real SSE, only
// the model's decisions are scripted. blockT2 (optional) hangs the worker's
// t2 turns to simulate a mid-plan kill.
func gatewayScript(t *testing.T, blockT2 chan struct{}) func(step int, req llmtest.Request) llmtest.Turn {
	return func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		switch {
		case strings.Contains(system, "planner"):
			return llmtest.Say(planJSON)
		case strings.Contains(system, "Cassandra"):
			return llmtest.Say(groundedVerdict)
		}
		// The worker loop.
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
				<-blockT2 // the "kill" lands while t2 is mid-flight
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

func openCortex(t *testing.T, dir string) *cortex.Cortex {
	t.Helper()
	s, err := store.Open(dir, "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return cortex.New(s)
}

func newEngine(t *testing.T, workspaceRoot, dataDir, gatewayURL string, ctx *cortex.Cortex) *Engine {
	t.Helper()
	e, err := NewEngine(EngineOptions{
		WorkspaceRoot:     workspaceRoot,
		DataDir:           dataDir,
		Cortex:            ctx,
		GatewayURL:        gatewayURL,
		DefaultMode:       mode.Engineer,
		OrchestratorModel: "accounts/fireworks/models/test-model",
		WorkerModel:       "accounts/fireworks/models/test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func postChat(t *testing.T, base, body string) map[string]interface{} {
	t.Helper()
	resp, err := http.Post(base+"/chat", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /chat = %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitTerminal(t *testing.T, base, runID string) string {
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
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if out.Status != "running" {
			return out.Status
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run never reached a terminal status")
	return ""
}

func readEvents(t *testing.T, base, runID string) []Event {
	t.Helper()
	resp, err := http.Get(base + "/events/replay/" + runID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	var events []Event
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("bad SSE frame %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func eventTypes(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

// TestCodydEndToEnd proves the serving shape: POST /chat dispatches a real
// plan run (planner -> orchestrator -> real workers -> acceptance gate with
// real adjudication), the SSE broker streams the workspace events with seq +
// replay, the durable trace rebuilds the timeline, and the terminal report is
// honest — all over real HTTP against the real engine.
func TestCodydEndToEnd(t *testing.T) {
	workspaceRoot := t.TempDir()
	dataDir := t.TempDir()
	gw := llmtest.NewServer(t, gatewayScript(t, nil))
	t.Cleanup(gw.Close)

	engine := newEngine(t, workspaceRoot, dataDir, gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	// Health.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Dispatch.
	out := postChat(t, srv.URL, `{"message": "seed the demo workspace", "conversation_id": "conv-e2e"}`)
	runID, _ := out["intent_id"].(string)
	if runID == "" || out["events_url"] != "/events?intent_id="+runID {
		t.Fatalf("dispatch envelope = %v", out)
	}
	if status := waitTerminal(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal status = %q", status)
	}

	// The work is real.
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(filepath.Join(workspaceRoot, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}

	// The stream carries the workspace timeline + an honest final message.
	events := readEvents(t, srv.URL, runID)
	types := strings.Join(eventTypes(events), ",")
	for _, want := range []string{"plan.created", "task.started", "sheet.authored", "task.turnin", "task.accepted", "plan.completed", "chat.assistant", "message.complete"} {
		if !strings.Contains(types, want) {
			t.Fatalf("stream missing %s: %s", want, types)
		}
	}
	var finalText string
	for _, ev := range events {
		if ev.Type == "chat.assistant" {
			finalText, _ = ev.Fields["text"].(string)
		}
		if ev.Type == "message.complete" && ev.Fields["status"] != "completed" {
			t.Fatalf("terminal event status = %v", ev.Fields["status"])
		}
	}
	// Result, not protocol: no orchestrator/worker/sheet jargon reaches the user.
	lower := strings.ToLower(finalText)
	for _, banned := range []string{"orchestrator", "worker", "sheet", "turn-in"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("protocol jargon %q leaked into the user-facing report: %q", banned, finalText)
		}
	}
	if !strings.Contains(lower, "done") {
		t.Fatalf("final report = %q", finalText)
	}

	// since_seq replay: asking past the last seq returns nothing new.
	last := events[len(events)-1].Seq
	resp2, err := http.Get(fmt.Sprintf("%s/events/replay/%s?since_seq=%d", srv.URL, runID, last))
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 4096)
	n, _ := resp2.Body.Read(body)
	resp2.Body.Close()
	if strings.Contains(string(body[:n]), "data: ") {
		t.Fatalf("since_seq=%d replayed events: %s", last, body[:n])
	}

	// The durable trace rebuilds the workspace timeline on reopen.
	resp3, err := http.Get(srv.URL + "/conversations/conv-e2e/trace")
	if err != nil {
		t.Fatal(err)
	}
	var traceOut struct {
		IntentID string  `json:"intent_id"`
		Status   string  `json:"status"`
		Events   []Event `json:"events"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&traceOut); err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if traceOut.IntentID != runID || traceOut.Status != "completed" {
		t.Fatalf("trace envelope = %+v", traceOut)
	}
	traceTypes := strings.Join(eventTypes(traceOut.Events), ",")
	for _, want := range []string{"plan.created", "task.accepted", "plan.completed"} {
		if !strings.Contains(traceTypes, want) {
			t.Fatalf("durable trace missing %s: %s", want, traceTypes)
		}
	}
	for _, ev := range traceOut.Events {
		if !traceWorkspaceTypes[ev.Type] {
			t.Fatalf("non-whitelisted event %q persisted to the trace", ev.Type)
		}
	}
}

// TestCodydStop proves POST /intents/{id}/stop interrupts a live run and the
// terminal is an honest "stopped", never a fake completion.
func TestCodydStop(t *testing.T) {
	workspaceRoot := t.TempDir()
	dataDir := t.TempDir()
	blockT2 := make(chan struct{})
	gw := llmtest.NewServer(t, gatewayScript(t, blockT2))
	t.Cleanup(gw.Close)
	// Registered AFTER gw.Close so it runs FIRST (LIFO): the blocked t2
	// handler must be released before the server can close.
	t.Cleanup(func() { close(blockT2) })

	engine := newEngine(t, workspaceRoot, dataDir, gw.URL, openCortex(t, t.TempDir()))
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "seed the demo workspace", "conversation_id": "conv-stop"}`)
	runID := out["intent_id"].(string)

	// Wait until t1 is accepted (t2 blocks), then stop.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("t1 never accepted")
		}
		events, err := engine.trace.load(runID)
		if err == nil {
			accepted := false
			for _, ev := range events {
				if ev.Type == "task.accepted" {
					accepted = true
				}
			}
			if accepted {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	resp, err := http.Post(srv.URL+"/intents/"+runID+"/stop", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stop: %v %v", resp, err)
	}
	resp.Body.Close()
	if status := waitTerminal(t, srv.URL, runID); status != "stopped" {
		t.Fatalf("terminal after stop = %q", status)
	}
}
