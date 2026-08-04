// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"matrix/cortex"
	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// TestMorpheusColdStartSmoke is the cold-start E2E smoke (MORPHEUS req.9.3):
// the full reassembled agent — real cortex on a temp store, a real spawned MCP
// fetch tool on a real tools.Manager (with the production recall/todo seams
// wired the way cmd/neo wires them), a real httptest SSE model — through a
// MULTI-TURN conversation exercising activation, memory_recall, the epistemic
// gate (a real extracted premise on the ledger at every dispatch), todo
// projection (the task graph renders resident), tool dispatch, and delivery —
// ending with a coherent final answer and a consolidated durable transcript.
// Real seams throughout; nothing is faked.
func TestMorpheusColdStartSmoke(t *testing.T) {
	// The real endpoint the real fetch tool probes.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"open":1,"close":2}]`)
	}))
	t.Cleanup(api.Close)

	sse := func(w http.ResponseWriter, content, finish string, toolCalls string) {
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"role": "assistant"}
		if content != "" {
			delta["content"] = content
		}
		if toolCalls != "" {
			var tcs []any
			if err := json.Unmarshal([]byte("["+toolCalls+"]"), &tcs); err != nil {
				panic(err)
			}
			delta["tool_calls"] = tcs
		}
		frame := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		fb, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n", fb)
		fmt.Fprint(w, "data: [DONE]\n")
	}
	call := func(id, name string, args map[string]any) string {
		ab, _ := json.Marshal(args)
		return fmt.Sprintf(`{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, id, name, string(ab))
	}

	var (
		mu       sync.Mutex
		bodies   []string
		mainCall int
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		switch {
		case strings.Contains(body, "load-bearing FACTUAL PREMISES"):
			// A REAL extracted premise: the ledger is non-empty, so the
			// check-before-act gate genuinely evaluates every dispatch.
			sse(w, `[{"statement":"the data API serves JSON candles","basis":"assumption","citation":""}]`, "stop", "")
			return
		case strings.Contains(body, "stated expectation"):
			sse(w, "match", "stop", "")
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mainCall++
		n := mainCall
		mu.Unlock()
		switch n {
		case 1:
			sse(w, "Plan: fetch the market data from the data API, then summarize it.", "tool_calls",
				call("t1", tools.TodoTool, map[string]any{"items": []any{
					map[string]any{"text": "fetch the market data", "status": "in_progress"},
					map[string]any{"text": "summarize it", "status": "pending"},
				}}))
		case 2:
			sse(w, "", "tool_calls",
				call("r1", tools.MemoryRecallTool, map[string]any{"query": "market data candles", "expect": "any prior notes about the data, or nothing"}))
		case 3:
			sse(w, "", "tool_calls",
				call("f1", "fetch__fetch", map[string]any{"url": api.URL + "/data", "expect": "HTTP 200 with a JSON array of candles"}))
		case 4:
			sse(w, "The data shows a single candle: open 1, close 2.", "stop", "")
		default:
			sse(w, "Earlier the data showed one candle with open 1 and close 2.", "stop", "")
		}
	}))
	t.Cleanup(model.Close)

	cfg := config.Default() // every governor layer at production defaults, Cassandra ON
	cfg.EpistemicPremises = true
	cfg.EpistemicPredictions = true
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "morpheus-smoke"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	mgr := replayFetchManager(t)
	mgr.SetRecall(pager.Recall) // the production wiring (cmd/neo/serve.go)
	var todoMu sync.Mutex
	var todoItems []tools.TodoItem
	mgr.SetTodo(func(_ context.Context, items []tools.TodoItem) error {
		todoMu.Lock()
		todoItems = append([]tools.TodoItem(nil), items...)
		todoMu.Unlock()
		return nil
	})

	client := revisionTestClient(t, model.URL)
	rep := &chokeReporter{}
	rec := &recordingConsolidator{}
	const conv = "conv-morpheus-smoke"
	a := New(Options{
		Config:       cfg,
		Main:         client,
		Cheap:        client,
		Tools:        mgr,
		Pager:        pager,
		Reporter:     rep,
		Consolidator: rec,
		ConvID:       conv,
	})

	// Turn 1: plan → todo projection → recall → real fetch → delivery.
	if err := a.Chat(context.Background(), "fetch the market data and summarize it for me"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// Turn 2: continuity — the answer must come from carried context.
	if err := a.Chat(context.Background(), "what did the data show earlier?"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	// Coherent final answers delivered on the user channel.
	said := rep.all()
	turn1, turn2 := false, false
	for _, line := range said {
		if strings.Contains(line, "open 1, close 2") {
			turn1 = true
		}
		if strings.Contains(line, "Earlier the data showed") {
			turn2 = true
		}
	}
	if !turn1 || !turn2 {
		t.Fatalf("both turns must deliver coherent answers (turn1=%v turn2=%v):\n%v", turn1, turn2, said)
	}

	// Todo projection reached the real seam AND the task graph rendered
	// resident in a later window.
	todoMu.Lock()
	gotTodos := len(todoItems)
	todoMu.Unlock()
	if gotTodos != 2 {
		t.Fatalf("todo projection must reach the wired seam with both items, got %d", gotTodos)
	}

	mu.Lock()
	captured := append([]string(nil), bodies...)
	mu.Unlock()
	if len(captured) != 5 {
		t.Fatalf("the conversation must take exactly 5 main model calls, got %d", len(captured))
	}
	// The extracted premise renders resident (the gate had a real ledger)...
	if !strings.Contains(captured[1], "the data API serves JSON candles") {
		t.Fatal("the extracted premise must render resident in the window after plan formation")
	}
	// ...and the projected task graph renders resident after the todo call.
	if !strings.Contains(captured[1], "Task graph") || !strings.Contains(captured[1], "fetch the market data") {
		t.Fatal("the projected task graph must render resident after the todo call")
	}
	// The real recall and fetch results reached the transcript the model reads.
	if !strings.Contains(captured[2], "memory_recall") {
		t.Fatal("the memory_recall result must be in the window")
	}
	if !strings.Contains(captured[3], "open") || !strings.Contains(captured[3], "close") {
		t.Fatal("the real fetch result must be in the window")
	}
	// Turn 2 carries turn 1's content — memory continuity.
	if !strings.Contains(captured[4], "open 1, close 2") {
		t.Fatal("turn 2's window must carry turn 1's delivered content")
	}

	// The durable cortex transcript is the consolidated record: both user
	// turns, both answers, and every tool interaction.
	msgs, err := pager.Transcript(conv, 0, 500)
	if err != nil {
		t.Fatalf("pager.Transcript: %v", err)
	}
	var users, answers int
	toolsSeen := map[string]bool{}
	for _, m := range msgs {
		switch m.Role {
		case cortex.RoleUser:
			users++
		case cortex.RoleAssistant:
			answers++
		}
		if m.ToolName != "" {
			toolsSeen[m.ToolName] = true
		}
	}
	if users < 2 || answers < 2 {
		t.Fatalf("durable transcript incomplete: %d user, %d assistant messages", users, answers)
	}
	for _, want := range []string{tools.TodoTool, tools.MemoryRecallTool, "fetch__fetch"} {
		if !toolsSeen[want] {
			t.Fatalf("durable transcript missing tool record for %s (saw %v)", want, toolsSeen)
		}
	}
	// Consolidation ran exactly once per turn.
	if got := rec.count(); got != 2 {
		t.Fatalf("consolidation must run exactly once per turn, got %d", got)
	}
}
