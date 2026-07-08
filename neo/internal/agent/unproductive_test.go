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

// emptyAnswerServer scripts a model that ALWAYS returns an empty assistant
// message with no content and no tool calls — the "returns nothing, makes no
// progress" pathology. Each such turn takes the empty-answer branch, which
// steers via the guidance channel and folds the nudge into the ONE unified
// unproductive-attempt counter (req.1.3: the loop-discipline backstop that the
// Cassandra 2.0 retirement leaves UNCHANGED). It counts model calls so the test
// can prove the loop escalates to an honest stop-and-ask within the unified
// bound rather than running to the step budget.
func emptyAnswerServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		frame := map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": ""},
			}},
		}
		fb, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n", fb)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmptyAnswerLoopBoundedByUnifiedCounter proves the unified unproductive-
// attempt bound (req.1.3) survives the completion-gate retirement: a model that
// keeps returning nothing must terminate with an honest ErrIncomplete via the
// unified escalation — well before the step budget — rather than being nudged
// forever or running to the budget.
func TestEmptyAnswerLoopBoundedByUnifiedCounter(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := emptyAnswerServer(t, &calls, &mu)

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
	cfg.CortexActor = "neo-empty-answer"

	// Sanity: the bound must be materially below the step budget, else the test
	// would not distinguish the unified escalation from the budget-only bound.
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
		ConvID: "conv-empty-answer",
	})
	if !a.continuousMemory() {
		t.Fatal("precondition: continuous-memory path must be active")
	}

	err = a.Chat(context.Background(), "do the thing")

	// The turn must NOT succeed (nothing was ever produced), and must terminate
	// as an honest incomplete — never fabricated success.
	if err == nil {
		t.Fatal("an all-empty run must NOT report success")
	}
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("must terminate with ErrIncomplete (honest partial), got: %v", err)
	}
	// It must stop via the unified unproductive-attempt bound, NOT the step budget.
	if strings.Contains(err.Error(), "step budget") {
		t.Fatalf("loop ran to the step budget instead of the unified bound: %v", err)
	}
	if !strings.Contains(err.Error(), "unproductive attempts") {
		t.Fatalf("expected the unified unproductive-attempt escalation, got: %v", err)
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	// The bound is reached in ~MaxGuidanceNudges+1 model calls; assert a tight
	// ceiling well below the step budget.
	ceiling := cfg.MaxGuidanceNudges + 2
	if got > ceiling {
		t.Fatalf("loop made %d model calls; the unified bound must terminate it within %d (step budget is %d)", got, ceiling, cfg.StepBudget)
	}
}
