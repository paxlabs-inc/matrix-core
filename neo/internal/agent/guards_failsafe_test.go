// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// selfAwareReframeMarker is the load-bearing sentence the continuous-memory
// window assembly (renderActivationBundle, continuous.go) folds in front of the
// trailing memory tail — the structural self-knowledge that stops the agent
// from treating the tail as a fresh request and re-answering it. Its presence
// in the assembled window IS "self-awareness enabled" for the window-assembly
// facet; its absence is "self-awareness bypassed".
const selfAwareReframeMarker = "do NOT restart it or re-answer an earlier request"

// TestGuardsRetainedAsFailsafes proves req.10.1: the three convergence guards —
// the no-progress stall detector, the step budget, and the Cassandra
// silent-voice controller — are RETAINED (wired, none removed) under this
// feature. Self-awareness is primary, but the failsafes stay armed by default.
func TestGuardsRetainedAsFailsafes(t *testing.T) {
	cfg := config.Default()
	if cfg.NoProgressStall <= 0 {
		t.Errorf("no-progress stall guard removed: NoProgressStall=%d", cfg.NoProgressStall)
	}
	if cfg.StepBudget <= 0 || cfg.StepBudgetMax <= 0 {
		t.Errorf("step-budget guard removed: StepBudget=%d StepBudgetMax=%d", cfg.StepBudget, cfg.StepBudgetMax)
	}
	if !cfg.CassandraEnabled {
		t.Error("Cassandra silent-voice controller must remain enabled as a failsafe by default")
	}
}

// reframeKeyedServer scripts a model whose behavior depends on the REAL
// assembled window: if the window carries the self-aware reframe (the agent
// knows the trailing tail is reference notes, not a fresh ask) the model
// converges — one final answer, no tool calls. Otherwise it loops, emitting the
// SAME tool call every step (the diagnosed re-answer spiral). It is a genuine
// decision function over the actual request bytes — no fake controller, no
// canned outcome independent of the window.
func reframeKeyedServer(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		selfAware := strings.Contains(string(body), selfAwareReframeMarker)

		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if selfAware {
			// The reframe is in the window: continue from the live exchange and
			// finish, instead of re-answering the trailing objective.
			frame := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Done — continuing from the live exchange, nothing to redo."},"finish_reason":"stop"}]}`
			fmt.Fprint(w, frame+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		// No reframe: treat the tail as a fresh request and spin on the same call.
		tc := fmt.Sprintf(`{"index":0,"id":"call_%d","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"the same query\"}"}}`, idx)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reframeKeyedClient(t *testing.T, url string) *llm.Client {
	t.Helper()
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  url,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	return client
}

// TestSelfAwarenessPreventsLoopEarlierGuardsAreFailsafe proves req.10.2 + 10.3
// on real code paths. The SAME window-keyed model is driven through the REAL
// agent.Chat loop twice, changing exactly ONE variable — whether the self-aware
// window-assembly reframe is enabled (ContinuousMemory). Cassandra is off in
// both runs so the ONLY loop backstop is the no-progress stall guard, isolating
// the attribution:
//
//   - bypassed: the reframe is absent, the model loops, and the no-progress
//     stall FAILSAFE fires with ErrIncomplete (the guard holds without
//     self-awareness);
//   - enabled: the reframe is present, the model converges on the FIRST step —
//     the loop is prevented earlier, before any guard needs to fire.
func TestSelfAwarenessPreventsLoopEarlierGuardsAreFailsafe(t *testing.T) {
	newAgent := func(t *testing.T, continuousMemory bool, calls *int, mu *sync.Mutex) (*Agent, func()) {
		srv := reframeKeyedServer(t, calls, mu)
		cfg := config.Default()
		cfg.ContinuousMemory = continuousMemory
		cfg.CassandraEnabled = false // isolate the no-progress stall failsafe
		cfg.CortexRoot = t.TempDir()
		cfg.CortexActor = "neo-self-aware-failsafe"
		pager, err := memory.Open(cfg)
		if err != nil {
			t.Fatalf("memory.Open: %v", err)
		}
		a := New(Options{
			Config: cfg,
			Main:   reframeKeyedClient(t, srv.URL),
			Tools:  &tools.Manager{},
			Pager:  pager,
			ConvID: "conv-self-aware-failsafe",
		})
		return a, func() { _ = pager.Close() }
	}

	// --- self-awareness BYPASSED: the guard must catch the loop ---
	var bypassedCalls int
	var bmu sync.Mutex
	bypassed, closeB := newAgent(t, false, &bypassedCalls, &bmu)
	defer closeB()
	errBypassed := bypassed.Chat(context.Background(), "keep searching for the same query")
	if errBypassed == nil || !errors.Is(errBypassed, ErrIncomplete) {
		t.Fatalf("without self-awareness the loop must hit the failsafe (ErrIncomplete); got: %v", errBypassed)
	}
	if !strings.Contains(errBypassed.Error(), "repeating the same step") {
		t.Fatalf("the no-progress stall guard must be the failsafe that fired; got: %v", errBypassed)
	}
	bmu.Lock()
	nBypassed := bypassedCalls
	bmu.Unlock()
	if nBypassed < config.Default().NoProgressStall {
		t.Fatalf("the bypassed run must actually loop into the stall bound; only %d calls", nBypassed)
	}

	// --- self-awareness ENABLED: the loop is prevented earlier ---
	var enabledCalls int
	var emu sync.Mutex
	enabled, closeE := newAgent(t, true, &enabledCalls, &emu)
	defer closeE()
	errEnabled := enabled.Chat(context.Background(), "keep searching for the same query")
	if errEnabled != nil {
		t.Fatalf("with self-awareness the loop must be prevented (no failsafe needed); got: %v", errEnabled)
	}
	emu.Lock()
	nEnabled := enabledCalls
	emu.Unlock()
	if nEnabled != 1 {
		t.Fatalf("self-awareness must converge on the first step; took %d calls", nEnabled)
	}
	if nEnabled >= nBypassed {
		t.Fatalf("self-awareness must prevent the loop EARLIER than the guard: enabled=%d, bypassed=%d", nEnabled, nBypassed)
	}
}
