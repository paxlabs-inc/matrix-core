// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// interleavedRejectServer scripts the exact N2 shape: a work->premature-
// complete->reject loop. Even model calls emit a lone `todo` bookkeeping call
// (genuine, DISTINCT work — the item text carries the call index so no two
// batches share a signature, so the pure no-progress stall never trips). Odd
// model calls emit a lone `task_complete` claiming coverage="full" WITH a
// non-empty open_gaps list — an incoherent claim the g1 coherence guard rejects
// deterministically with NO adjudicator wired (real code path, no fakes). Before
// the fix, the interleaved `todo` reset the guidance counter and the completion
// branch bypassed stall detection, so the loop ran to the step budget (50). With
// the unified unproductive-attempt counter, the rejections accumulate across the
// interleaved work and the loop stops-and-asks well before the budget.
func interleavedRejectServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")

		var toolCall map[string]any
		if idx%2 == 0 {
			// Genuine, DISTINCT work: a todo whose text varies per call so the
			// batch signature is unique every time (no pure-repeat stall).
			toolCall = map[string]any{
				"index": 0, "id": fmt.Sprintf("call_todo_%d", idx), "type": "function",
				"function": map[string]any{
					"name":      tools.TodoTool,
					"arguments": fmt.Sprintf(`{"items":[{"text":"work step %d","status":"done"}]}`, idx),
				},
			}
		} else {
			// Premature, incoherent completion: coverage=full with open gaps →
			// g1 rejects (no adjudicator needed).
			toolCall = map[string]any{
				"index": 0, "id": fmt.Sprintf("call_done_%d", idx), "type": "function",
				"function": map[string]any{
					"name":      tools.TaskCompleteTool,
					"arguments": `{"summary":"All finished.","coverage":"full","open_gaps":["the deliverable is still unresolved"]}`,
				},
			}
		}

		frame := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role":       "assistant",
					"tool_calls": []any{toolCall},
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

// TestInterleavedCompleteRejectLoopBoundedByUnifiedCounter is the N2 regression
// guard (req.8.1/8.2/8.3): an interleaved work->premature-complete->reject
// pattern must terminate with an honest partial (ErrIncomplete) within the
// unified unproductive-attempt bound — well before the step budget — rather than
// being reset to zero by the interleaved genuine work and running to the budget.
func TestInterleavedCompleteRejectLoopBoundedByUnifiedCounter(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := interleavedRejectServer(t, &calls, &mu)

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
	cfg.CortexActor = "neo-interleaved-reject"
	cfg.GateAllWork = true // mirror prod: substantive work rides the grounded gate

	// Sanity: the bound must be materially below the step budget, else the test
	// would not distinguish the fix from the pre-fix budget-only bound.
	if cfg.MaxGuidanceNudges <= 0 || cfg.MaxGuidanceNudges >= cfg.StepBudget/2 {
		t.Fatalf("precondition: MaxGuidanceNudges (%d) must be a small positive bound below StepBudget/2 (%d)", cfg.MaxGuidanceNudges, cfg.StepBudget/2)
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
		ConvID: "conv-interleaved-reject",
	})
	if !a.continuousMemory() {
		t.Fatal("precondition: continuous-memory path must be active")
	}

	err = a.Chat(context.Background(), "build the thing and finish")

	// The turn must NOT succeed (the completion was never genuinely acceptable),
	// and must terminate as an honest incomplete — never fabricated success.
	if err == nil {
		t.Fatal("interleaved premature-complete/reject loop must NOT report success")
	}
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("must terminate with ErrIncomplete (honest partial), got: %v", err)
	}
	// It must stop via the unified unproductive-attempt bound, NOT by exhausting
	// the step budget (the pre-fix behavior).
	if strings.Contains(err.Error(), "step budget") {
		t.Fatalf("loop ran to the step budget instead of the unified bound — the reset-on-tool-dispatch bug is back: %v", err)
	}
	if !strings.Contains(err.Error(), "unproductive attempts") {
		t.Fatalf("expected the unified unproductive-attempt escalation, got: %v", err)
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	// With MaxGuidanceNudges rejections interleaved 1:1 with genuine work, the
	// bound is reached in ~2*(MaxGuidanceNudges+1) model calls. Assert a tight
	// ceiling well below the step budget: before the fix this ran to ~StepBudget.
	ceiling := 2*(cfg.MaxGuidanceNudges+1) + 2
	if got > ceiling {
		t.Fatalf("loop made %d model calls; the unified bound must terminate it within %d (step budget is %d) — interleaved work must not reset the completion-reject bound", got, ceiling, cfg.StepBudget)
	}
}
