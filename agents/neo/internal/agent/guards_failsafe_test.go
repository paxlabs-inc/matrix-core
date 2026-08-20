// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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

	mcllm "centra/core/mcl/llm"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/memory"
	"centra/agents/neo/internal/tools"
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
// canned outcome independent of the window. loopAlways forces the looping arm
// regardless of the window, isolating the stall failsafe on the single path
// (where the reframe is always present).
func reframeKeyedServer(t *testing.T, calls *int, mu *sync.Mutex, loopAlways bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		selfAware := strings.Contains(string(body), selfAwareReframeMarker)
		if !loopAlways && !selfAware {
			t.Errorf("the single-path window must always carry the self-aware reframe; it is missing from the request body")
		}

		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if selfAware && !loopAlways {
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

// TestSelfAwarenessPreventsLoopEarlierGuardsAreFailsafe proves the failsafe
// posture on real code paths, on the SINGLE memory path (the reframe is always
// present in the window). Cassandra is off in both runs so the ONLY loop
// backstop is the no-progress stall guard, isolating the attribution:
//
//   - looping model: even with the reframe present, a model that spins on the
//     same call is caught by the no-progress stall FAILSAFE with ErrIncomplete
//     (the guard holds regardless of self-awareness);
//   - window-keyed model: the reframe is present in the real window (asserted
//     inside the server), and the model converges on the FIRST step — the loop
//     is prevented earlier, before any guard needs to fire.
func TestSelfAwarenessPreventsLoopEarlierGuardsAreFailsafe(t *testing.T) {
	newAgent := func(t *testing.T, loopAlways bool, calls *int, mu *sync.Mutex) (*Agent, func()) {
		srv := reframeKeyedServer(t, calls, mu, loopAlways)
		cfg := config.Default()
		cfg.CassandraEnabled = false // isolate the no-progress stall failsafe
		cfg.DataRoot = t.TempDir()
		cfg.NeocortexActor = "neo-self-aware-failsafe"
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

	// --- looping model: the guard must catch the loop ---
	var loopCalls int
	var bmu sync.Mutex
	looping, closeB := newAgent(t, true, &loopCalls, &bmu)
	defer closeB()
	errLooping := looping.Chat(context.Background(), "keep searching for the same query")
	if errLooping == nil || !errors.Is(errLooping, ErrIncomplete) {
		t.Fatalf("a looping model must hit the failsafe (ErrIncomplete); got: %v", errLooping)
	}
	if !strings.Contains(errLooping.Error(), "repeating the same step") {
		t.Fatalf("the no-progress stall guard must be the failsafe that fired; got: %v", errLooping)
	}
	bmu.Lock()
	nLooping := loopCalls
	bmu.Unlock()
	if nLooping < config.Default().NoProgressStall {
		t.Fatalf("the looping run must actually loop into the stall bound; only %d calls", nLooping)
	}

	// --- window-keyed model: the reframe prevents the loop earlier ---
	var keyedCalls int
	var emu sync.Mutex
	keyed, closeE := newAgent(t, false, &keyedCalls, &emu)
	defer closeE()
	errKeyed := keyed.Chat(context.Background(), "keep searching for the same query")
	if errKeyed != nil {
		t.Fatalf("with the reframe present the loop must be prevented (no failsafe needed); got: %v", errKeyed)
	}
	emu.Lock()
	nKeyed := keyedCalls
	emu.Unlock()
	if nKeyed != 1 {
		t.Fatalf("self-awareness must converge on the first step; took %d calls", nKeyed)
	}
	if nKeyed >= nLooping {
		t.Fatalf("self-awareness must prevent the loop EARLIER than the guard: keyed=%d, looping=%d", nKeyed, nLooping)
	}
}
