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
	"time"

	mcllm "centra/core/mcl/llm"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/tools"
)

func revisionTestClient(t *testing.T, url string) *llm.Client {
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

// TestRevisionForcedAtMismatchLimit proves req.6.3 on the real loop: three
// consecutive mismatches on ONE strategy (differently-argued probes of one
// host, each with a stated expectation, each failing) force a tools-stripped
// reasoning-only revision step before any further dispatch.
func TestRevisionForcedAtMismatchLimit(t *testing.T) {
	probe := func(path string) string {
		return fmt.Sprintf(`{"index":0,"id":"c%s","type":"function","function":{"name":"fetch_url","arguments":"{\"url\":\"http://api.example.com%s\",\"expect\":\"200 with a JSON array of candles\"}"}}`, strings.Trim(path, "/"), path)
	}
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
			emit(probe("/ohlc"))
		case 2:
			emit(probe("/candles"))
		case 3:
			emit(probe("/klines"))
		case 4:
			// The forced revision step (tools-stripped).
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"The endpoint convention itself is wrong — I will read the API docs instead of guessing paths."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.EpistemicPredictions = true
	a := New(Options{Config: cfg, Main: revisionTestClient(t, srv.URL), Tools: &tools.Manager{}})

	if err := a.Chat(context.Background(), "get the OHLC data"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if a.turn.revisionsThisTurn != 1 {
		t.Fatalf("the mismatch limit (3) must force exactly one revision; got %d", a.turn.revisionsThisTurn)
	}
	if len(bodies) < 4 {
		t.Fatalf("expected the revision as the 4th model call, got %d calls", len(bodies))
	}
	if !strings.Contains(bodies[3], "consecutive prediction mismatches") {
		t.Fatal("the revision directive must name the mismatch bound")
	}
	if strings.Contains(bodies[3], `"tools"`) {
		t.Fatal("the forced revision step must be tools-stripped")
	}
	if len(a.turn.mismatchMeter) != 0 {
		t.Fatal("the revision must reset the per-strategy mismatch meters")
	}
}

// TestRevisionForcedAtConvergenceWindow proves req.7.2 on the real loop: four
// consecutive actions with an UNCHANGED evidence set — distinct operations
// that the text-similarity stall guard cannot see — are measured
// non-convergence and force the same revision step; the turn completes
// cleanly (no stall failsafe death), proving the epistemic layer fired first.
func TestRevisionForcedAtConvergenceWindow(t *testing.T) {
	recall := func(q string) string {
		return fmt.Sprintf(`{"index":0,"id":"c%d","type":"function","function":{"name":"memory_recall","arguments":"{\"query\":\"%s\"}"}}`, len(q), q)
	}
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
		// Distinct queries every step (no textual repeat for the stall guard),
		// but the store answers every one identically: no evidence growth.
		switch call {
		case 1:
			emit(recall("alpha"))
		case 2:
			emit(recall("bravo kilo"))
		case 3:
			emit(recall("charlie zulu xray"))
		case 4:
			emit(recall("delta november golf lima"))
		case 5:
			emit(recall("echo sierra tango uniform whiskey"))
		case 6:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"I keep re-reading the same fact — revised plan: act on it."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(srv.Close)

	mgr := &tools.Manager{}
	mgr.SetRecall(func(_ context.Context, query string, types []string, k int, asOf *time.Time) (string, error) {
		return "the one durable fact", nil
	})
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = true
	a := New(Options{Config: cfg, Main: revisionTestClient(t, srv.URL), Tools: mgr})

	if err := a.Chat(context.Background(), "figure it out"); err != nil {
		t.Fatalf("the epistemic layer must fire before any failsafe death: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if a.turn.revisionsThisTurn != 1 {
		t.Fatalf("measured non-convergence (window 4) must force exactly one revision; got %d", a.turn.revisionsThisTurn)
	}
	var revision string
	for _, b := range bodies {
		if strings.Contains(b, "Measured non-convergence") && !strings.Contains(b, `"tools"`) {
			revision = b
			break
		}
	}
	if revision == "" {
		t.Fatal("the convergence revision must run tools-stripped with the measured directive")
	}
}

// TestExpectationFreeProbeDispatches proves optional prediction metadata can
// never block a real tool attempt.
func TestExpectationFreeProbeDispatches(t *testing.T) {
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
		if call == 1 {
			tc := `{"index":0,"id":"c1","type":"function","function":{"name":"fetch_url","arguments":"{\"url\":\"http://api.example.com/guess\"}"}}`
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	var dispatched int
	a := New(Options{Config: cfg, Main: revisionTestClient(t, srv.URL), Tools: &tools.Manager{},
		Observer: func(ev ToolEvent) {
			if ev.Phase == ToolStart {
				dispatched++
			}
		}})
	if err := a.Chat(context.Background(), "find the endpoint"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("an expectation-free probe must dispatch once; %d dispatches observed", dispatched)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(strings.Join(bodies, ""), "not dispatched (no expectation stated)") {
		t.Fatal("optional prediction metadata produced a dispatch refusal")
	}
}

// TestStallFailsafeRetainedWhenEpistemicDisabled proves req.7.2's failsafe
// posture (do_2): with the epistemic layer disabled, the outer NoProgressStall
// guard still owns a genuinely repeating loop — the guards were kept, not
// retired.
func TestStallFailsafeRetainedWhenEpistemicDisabled(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls++
		idx := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		tc := fmt.Sprintf(`{"index":0,"id":"call_%d","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"the same query\"}"}}`, idx)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	a := New(Options{Config: cfg, Main: revisionTestClient(t, srv.URL), Tools: &tools.Manager{}})
	err := a.Chat(context.Background(), "search for it")
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("the no-progress stall failsafe must still fire with the epistemic layer disabled; got %v", err)
	}
}
