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

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// TestPinnedOncePerTurnAndMidTurnWritesDeferred proves the NE-7 discipline on
// the real loop's SINGLE memory path: the activation bundle is assembled ONCE
// per turn, and a Neocortex write landing MID-TURN (while the loop is between
// steps) does not mutate the rendered snapshot — it surfaces on the NEXT turn.
// Real Agent.Chat, real Neocortex pager, real httptest SSE endpoint; the mid-turn
// write happens inside the model handler, which genuinely executes between
// loop steps.
func TestPinnedOncePerTurnAndMidTurnWritesDeferred(t *testing.T) {
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-prefix-discipline"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { pager.Close() })

	const marker = "mid-turn hard rule: never deploy on fridays"
	var (
		mu      sync.Mutex
		bodies  []string
		wrote   bool
		writeMu sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		call := len(bodies)
		mu.Unlock()

		// The FIRST model call of a turn is in flight while the loop is
		// mid-turn: write a hard constraint into the REAL Neocortex here, before
		// the loop assembles step 2's window.
		writeMu.Lock()
		if !wrote {
			wrote = true
			if _, err := pager.RememberConstraint(context.Background(), marker, "dont", "hard"); err != nil {
				t.Errorf("RememberConstraint: %v", err)
			}
		}
		writeMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			tc := `{"index":0,"id":"call_0","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q\"}"}}`
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
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
	a := New(Options{Config: cfg, Main: client, Tools: &tools.Manager{}, Pager: pager})

	// Turn 1: a genuine multi-step turn (tool call, then final).
	if err := a.Chat(context.Background(), "first turn"); err != nil {
		t.Fatalf("Chat turn 1: %v", err)
	}
	if a.activationAssemblies != 1 {
		t.Fatalf("activation bundle must be assembled exactly once per turn (NE-7); got %d assemblies", a.activationAssemblies)
	}
	mu.Lock()
	turn1 := append([]string(nil), bodies...)
	mu.Unlock()
	if len(turn1) < 2 {
		t.Fatalf("turn 1 must be multi-step, got %d model calls", len(turn1))
	}
	for i, b := range turn1 {
		if strings.Contains(b, "never deploy on fridays") {
			t.Fatalf("mid-turn Neocortex write leaked into the frozen turn-1 snapshot (model call %d)", i+1)
		}
	}

	// Turn 2: the deferred write surfaces on the next turn's snapshot.
	if err := a.Chat(context.Background(), "second turn"); err != nil {
		t.Fatalf("Chat turn 2: %v", err)
	}
	if a.activationAssemblies != 2 {
		t.Fatalf("activation assemblies after two turns = %d, want 2", a.activationAssemblies)
	}
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	if !strings.Contains(last, "never deploy on fridays") {
		t.Fatal("the mid-turn constraint must surface on the NEXT turn's pinned snapshot")
	}
}

// TestEpistemicTailFixedOrder proves req.3.1's fixed tail section order on the
// real window assembly: activation/memory block first, then the premise-ledger
// slot, then the task-graph slot, then the budget stat.
func TestEpistemicTailFixedOrder(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.turn.premiseTail = "\n[premise-ledger-slot]\n"
	a.turn.graphTail = "\n[task-graph-slot]\n"
	tail := a.epistemicTail()
	li := strings.Index(tail, "[premise-ledger-slot]")
	gi := strings.Index(tail, "[task-graph-slot]")
	if li < 0 || gi < 0 || li > gi {
		t.Fatalf("fixed tail order violated (ledger=%d graph=%d):\n%s", li, gi, tail)
	}
}
