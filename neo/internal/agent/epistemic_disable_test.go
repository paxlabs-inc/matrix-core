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

	"matrix/neo/internal/config"
	"matrix/neo/internal/tools"
)

// TestEpistemicDisablementRestoresPriorBehavior proves req.8.2 on the real
// loop: with a mechanism disabled the prior behavior returns cleanly — the
// arena-shaped plan's scaffold dispatches un-gated (premises off), and an
// expectation-free probe dispatches unrefused (predictions off). The
// guards-as-failsafes path stays armed either way (proven separately by
// TestStallFailsafeRetainedWhenEpistemicDisabled).
func TestEpistemicDisablementRestoresPriorBehavior(t *testing.T) {
	t.Run("premises off: no gate, no ledger", func(t *testing.T) {
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
				plan := "Plan: I can wire it up directly through my own OpenAI-compatible API."
				tc := `{"index":0,"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"bridge.py\"}"}}`
				fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q,\"tool_calls\":[%s]}}]}\n", plan, tc)
				fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
				fmt.Fprint(w, "data: [DONE]\n")
				return
			}
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}))
		t.Cleanup(srv.Close)

		cfg := config.Default()
		cfg.CassandraEnabled = false
		cfg.EpistemicPremises = false
		var dispatched int
		a := New(Options{Config: cfg, Main: revisionTestClient(t, srv.URL), Tools: &tools.Manager{},
			Capability: trueSurface(),
			Observer: func(ev ToolEvent) {
				if ev.Phase == ToolStart {
					dispatched++
				}
			}})
		if err := a.Chat(context.Background(), "bridge the agents"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if dispatched != 1 {
			t.Fatalf("with premises off the scaffold must dispatch un-gated; got %d dispatches", dispatched)
		}
		if a.ledger != nil {
			t.Fatal("with premises off no ledger must form")
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(strings.Join(bodies, ""), "check-before-act") {
			t.Fatal("with premises off the gate must not refuse anything")
		}
	})

	t.Run("predictions off: expectation-free probe dispatches", func(t *testing.T) {
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
				tc := `{"index":0,"id":"c1","type":"function","function":{"name":"fetch_url","arguments":"{\"url\":\"http://api.example.com/x\"}"}}`
				fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
				fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
				fmt.Fprint(w, "data: [DONE]\n")
				return
			}
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}))
		t.Cleanup(srv.Close)

		cfg := config.Default()
		cfg.CassandraEnabled = false
		cfg.EpistemicPredictions = false
		var dispatched int
		a := New(Options{Config: cfg, Main: revisionTestClient(t, srv.URL), Tools: &tools.Manager{},
			Observer: func(ev ToolEvent) {
				if ev.Phase == ToolStart {
					dispatched++
				}
			}})
		if err := a.Chat(context.Background(), "fetch it"); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if dispatched != 1 {
			t.Fatalf("with predictions off the probe must dispatch unrefused; got %d dispatches", dispatched)
		}
		if len(a.mismatchMeter) != 0 {
			t.Fatal("with predictions off no meter must accumulate")
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(strings.Join(bodies, ""), "no expectation stated") {
			t.Fatal("with predictions off the guess refusal must not fire")
		}
	})
}
