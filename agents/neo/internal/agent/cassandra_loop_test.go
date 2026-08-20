// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	mcllm "centra/core/mcl/llm"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/memory"
	"centra/agents/neo/internal/tools"
)

// spiralServer scripts a model that emits the SAME tool call every step — a
// no-progress spiral. Driven through the REAL agent.Chat loop, the Cassandra 2.0
// controller must fold a doubt mod into the agent's own prior content before the
// hard no-progress stall trips, proving the Chat-loop wiring (task 2.3) end to
// end on real code paths (no controller stub).
func spiralServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		tcJSON := map[string]any{
			"index": 0, "id": fmt.Sprintf("call_%d", idx), "type": "function",
			"function": map[string]any{
				"name":      "web_search",
				"arguments": `{"query":"the same query"}`,
			},
		}
		frame := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "tool_calls": []any{tcJSON}},
			}},
		}
		fb, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n", fb)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCassandraFiresOnRealLoopSpiral(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := spiralServer(t, &calls, &mu)

	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-cassandra-spiral"
	if !cfg.CassandraEnabled {
		t.Fatal("precondition: the controller must be enabled by default")
	}

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	a := New(Options{
		Config: cfg,
		Main:   client,
		Tools:  &tools.Manager{},
		Pager:  pager,
		ConvID: "conv-cassandra-spiral",
	})

	err = a.Chat(context.Background(), "keep searching for the same query")

	// The spiral must terminate as an honest incomplete (the no-progress stall
	// backstop), never fabricated success.
	if err == nil || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a spiral must terminate with ErrIncomplete, got: %v", err)
	}
	// The controller must have fired at least one doubt mod during the run — the
	// Chat-loop wiring (buildCassandraSignals -> cassandraStep after the assistant
	// append) is exercised on the real loop.
	if len(a.turn.casRecord) == 0 {
		t.Fatal("the controller must inject at least one mod during a real spiral")
	}
	sawLoopDoubt := false
	for _, m := range a.turn.casRecord {
		if m.Trigger == trigLoop && m.Side == sideDoubt {
			sawLoopDoubt = true
		}
		// Every mod is a dual-record with the original preserved and a non-empty
		// injected line (guardrail 3 + metacognition-only).
		if m.Mod == "" {
			t.Error("every mod must carry a non-empty injected line")
		}
	}
	if !sawLoopDoubt {
		t.Errorf("a no-progress spiral must produce a (loop, doubt) mod; got %+v", a.turn.casRecord)
	}
	// It must fire BEFORE the step budget (the stall bound is much tighter).
	mu.Lock()
	got := calls
	mu.Unlock()
	if got >= cfg.StepBudget {
		t.Errorf("the spiral ran to the step budget (%d calls); the stall bound should end it far sooner", got)
	}
}
