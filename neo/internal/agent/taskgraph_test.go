// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

// TestTaskGraphTracksRealRun proves req.7.1/7.3/7.4 on the real loop: the
// model's todo call seeds the graph's subgoals (the todo surface is a
// projection — one vocabulary), every dispatched action links to the
// in_progress subgoal, evidence is a computed set (a repeated identical
// result is NOT growth), and the graph renders resident in the actual request
// bytes the step it changes.
func TestTaskGraphTracksRealRun(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(tc string) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
		switch call {
		case 1:
			emit(`{"index":0,"id":"c1","type":"function","function":{"name":"todo","arguments":"{\"items\":[{\"text\":\"find the data\",\"status\":\"in_progress\"},{\"text\":\"report\",\"status\":\"pending\"}]}"}}`)
		case 2, 3:
			// The SAME recall twice: the second discharges no new evidence.
			emit(`{"index":0,"id":"c2","type":"function","function":{"name":"memory_recall","arguments":"{\"query\":\"the data\"}"}}`)
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(srv.Close)

	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	mgr := &tools.Manager{}
	var (
		todoMu    sync.Mutex
		lastItems []tools.TodoItem
	)
	mgr.SetTodo(func(_ context.Context, items []tools.TodoItem) error {
		todoMu.Lock()
		lastItems = append([]tools.TodoItem(nil), items...)
		todoMu.Unlock()
		return nil
	})
	mgr.SetRecall(func(_ context.Context, query string, types []string, k int, asOf *time.Time) (string, error) {
		return "durable fact: the data lives at /data/ohlc.json", nil
	})

	cfg := config.Default()
	cfg.CassandraEnabled = false
	a := New(Options{Config: cfg, Main: client, Tools: mgr})

	if err := a.Chat(context.Background(), "find the data and report"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	g := a.graph
	if g == nil {
		t.Fatal("a real run must seed the task graph")
	}

	// Projection consistency (req.7.3): the graph's subgoals ARE the todo
	// surface's items — same texts, same statuses, same order.
	todoMu.Lock()
	items := lastItems
	todoMu.Unlock()
	if len(items) != len(g.Subgoals) {
		t.Fatalf("todo projection out of sync: surface has %d items, graph has %d subgoals", len(items), len(g.Subgoals))
	}
	for i, it := range items {
		if g.Subgoals[i].Text != it.Text || g.Subgoals[i].Status != it.Status {
			t.Fatalf("subgoal %d diverged from the todo surface: %+v vs %+v", i, g.Subgoals[i], it)
		}
	}

	// Action linking + computed evidence delta (req.7.1/7.2).
	find := g.Subgoals[0]
	if find.Actions != 2 {
		t.Fatalf("both recalls must link to the in_progress subgoal; actions = %d, want 2", find.Actions)
	}
	if len(g.evidenceSet) != 1 || len(find.Evidence) != 1 {
		t.Fatalf("identical results must discharge ONE evidence item (set=%d subgoal=%d)", len(g.evidenceSet), len(find.Evidence))
	}
	if g.actionsSinceGrowth != 1 {
		t.Fatalf("the repeated recall added nothing: actionsSinceGrowth = %d, want 1", g.actionsSinceGrowth)
	}

	// Resident rendering (req.7.4): the graph rides the later request bytes.
	mu.Lock()
	defer mu.Unlock()
	last := bodies[len(bodies)-1]
	if !strings.Contains(last, "Task graph") || !strings.Contains(last, "find the data") {
		t.Fatal("the task graph must render resident in the window the step it changes")
	}
	if !strings.Contains(last, "added NO new evidence") {
		t.Fatal("the resident graph must surface the computed non-convergence read")
	}
}
