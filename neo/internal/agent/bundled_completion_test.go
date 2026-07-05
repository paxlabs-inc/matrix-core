// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// bundledCompletionServer scripts an SSE endpoint that ALWAYS answers with the
// same assistant turn: a bookkeeping `todo` call bundled WITH `task_complete`
// in one batch — the exact shape grok emitted in the 2026-07-05 spiral. It
// counts model calls so the test can prove the turn terminates instead of
// re-driving the model forever. Serving an unconditionally-bundled completion
// is the worst case: if the loop ever bounces it back, it will bundle again,
// and the call count grows without bound (up to the step budget).
func bundledCompletionServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		frame := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"index": 0, "id": "call_todo", "type": "function",
							"function": map[string]any{
								"name":      tools.TodoTool,
								"arguments": `{"items":[{"text":"survey papers","status":"done"},{"text":"summarize","status":"done"}]}`,
							},
						},
						map[string]any{
							"index": 1, "id": "call_done", "type": "function",
							"function": map[string]any{
								"name":      tools.TaskCompleteTool,
								"arguments": `{"summary":"Research complete: decentralized-training papers (INTELLECT-1, Gensyn, Bittensor) with links.","coverage":"full"}`,
							},
						},
					},
				},
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

// TestBundledTaskCompleteDoesNotSpiral is the regression guard for the
// 2026-07-05 "Hey Andrew. What's up?" spiral. grok bundled `task_complete` with
// a final `todo` bookkeeping call; the loop bounced it ("call task_complete on
// its own"), reset the guidance budget, and never reached the no-progress stall
// — so bundled-complete <-> bare-message alternated forever. The fix runs the
// sibling and adjudicates the completion in the SAME step. With GateAllWork on
// and no adjudicator wired, the strict path fails open once the object is
// coherent, so the turn must finish on the FIRST model call.
func TestBundledTaskCompleteDoesNotSpiral(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := bundledCompletionServer(t, &calls, &mu)

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
	cfg.ContinuousMemory = true
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-bundled-complete"
	cfg.GateAllWork = true // mirror prod: substantive work rides the grounded gate

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
		ConvID: "conv-bundled-complete",
	})
	if !a.continuousMemory() {
		t.Fatal("precondition: continuous-memory path must be active")
	}

	if err := a.Chat(context.Background(), "research decentralized AI training and summarize the key papers"); err != nil {
		t.Fatalf("Chat returned error (spiral / step-budget exhaustion?): %v", err)
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	// The bundled completion must be accepted on the FIRST model call. Before the
	// fix this bounced and re-drove the model until the step budget, so the count
	// would be many; the tight bound proves the spiral is gone.
	if got != 1 {
		t.Fatalf("model was called %d times; a bundled todo+task_complete must finish on the first call (no bounce/spiral)", got)
	}
}
