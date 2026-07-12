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
	"matrix/neo/internal/tools"
)

// premiseLoopServer scripts the REAL loop: the main model's first committing
// turn narrates an arena-shaped plan (an ungrounded self-claim + a user-stated
// fact) and calls a tool; the cheap-model premise-extraction request (detected
// by its distinctive system prompt riding the request bytes) answers with a
// premise JSON; the next main call closes. One endpoint, decisions keyed on
// the actual request content — no fakes.
func premiseLoopServer(t *testing.T, bodies *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	const plan = "Plan: I can wire the Neo side up directly through my own API. The user said the target environment is staging, so I'll scaffold against it."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")

		// The premise-extraction lane (cheap model): reply with premise rows.
		if strings.Contains(body, "load-bearing FACTUAL PREMISES") {
			rows := `[{\"statement\":\"the target environment is staging\",\"basis\":\"user\",\"citation\":\"the user said the target is staging\"},{\"statement\":\"Neo exposes an API I can call directly\",\"basis\":\"self\",\"citation\":\"\"}]`
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"%s\"},\"finish_reason\":\"stop\"}]}\n", rows)
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}

		mu.Lock()
		*bodies = append(*bodies, body)
		call := len(*bodies)
		mu.Unlock()

		if call == 1 {
			tc := `{"index":0,"id":"call_0","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"scaffold\"}"}}`
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q,\"tool_calls\":[%s]}}]}\n", plan, tc)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPremiseLedgerFromRealPlan proves req.4.1/4.2/4.4 on the real loop: a
// real plan produces a ledger whose premises carry correct provenance — the
// deterministic self-claim detector catches the arena-shaped "my own API"
// claim as an unchecked self-assumption, the extraction lane's user-stated
// fact arrives cited from the user, a "self"-basis row is never trusted as
// cited — and the ledger renders resident in the NEXT step's actual request
// bytes, in the fixed tail slot.
func TestPremiseLedgerFromRealPlan(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := premiseLoopServer(t, &bodies, &mu)
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
	cfg.CassandraEnabled = false
	a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: &tools.Manager{}})

	if err := a.Chat(context.Background(), "bridge Neo and the other agent"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if a.ledger == nil || len(a.ledger.items) == 0 {
		t.Fatal("a committing plan must seed the premise ledger")
	}
	var selfClaim, userFact *Premise
	for _, p := range a.ledger.items {
		if p.SelfRef {
			selfClaim = p
		}
		if p.Source == "user" {
			userFact = p
		}
	}
	if selfClaim == nil {
		t.Fatalf("the arena-shaped self-claim must be on the ledger; got %+v", a.ledger.items)
	}
	if selfClaim.Status != premiseAssumption {
		t.Fatalf("a self-referential claim must start as an ASSUMPTION (fail toward checking), got %q", selfClaim.Status)
	}
	if userFact == nil || userFact.Status != premiseCited || userFact.Citation == "" {
		t.Fatalf("the user-stated premise must be cited from the user with a citation; got %+v", userFact)
	}

	// Resident rendering (req.4.2): the ledger rides the step-2 request bytes.
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("expected a multi-step turn, got %d main-model calls", len(bodies))
	}
	step2 := bodies[len(bodies)-1]
	if !strings.Contains(step2, "Premise ledger") {
		t.Fatal("the premise ledger must render resident in the next step's window")
	}
	if !strings.Contains(step2, "ASSUMPTION about yourself") {
		t.Fatal("the unchecked self-claim must render as a self-assumption in the resident ledger")
	}
	if !strings.Contains(step2, "cited: user") {
		t.Fatal("the user-stated premise must render with its user provenance")
	}
}

// TestCassandraGainsPremiseProvenance proves req.4.3: the controller's signal
// bundle carries premise provenance, and classification fires the doubt side
// on a standing refuted premise and on closing over an unchecked self-
// assumption — judged from the real ledger state, ahead of any behavioral
// drift signal.
func TestCassandraGainsPremiseProvenance(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.ledger = &premiseLedger{}
	self := a.ledger.add(Premise{Statement: "I expose an OpenAI-compatible API", Status: premiseAssumption, SelfRef: true})

	sig := a.buildCassandraSignals(3, llm.Message{Role: llm.RoleAssistant, Content: "all done"}, 0, nil, "", nil, map[string]struct{}{"web_search": {}}, true)
	if sig.ungroundedSelfPremises != 1 {
		t.Fatalf("signals must carry the unchecked self-premise count, got %d", sig.ungroundedSelfPremises)
	}
	if trig, side := a.cassandraClassify(sig); trig != trigUngroundedClose || side != sideDoubt {
		t.Fatalf("closing over an unchecked self-assumption must classify as (%s, doubt); got (%s, %s)", trigUngroundedClose, trig, side)
	}

	a.premiseRefute(self, "capability surface: no OpenAI-compatible endpoint")
	sig = a.buildCassandraSignals(4, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{}}}, 0, nil, "", nil, map[string]struct{}{"web_search": {}}, false)
	if sig.refutedPremises != 1 {
		t.Fatalf("signals must carry the refuted-premise count, got %d", sig.refutedPremises)
	}
	if trig, side := a.cassandraClassify(sig); trig != trigRefutedPremise || side != sideDoubt {
		t.Fatalf("acting past a refuted premise must classify as (%s, doubt); got (%s, %s)", trigRefutedPremise, trig, side)
	}

	// The transition re-rendered the resident slot the step it occurred.
	if !strings.Contains(a.premiseTail, "REFUTED") {
		t.Fatal("a refute transition must re-render the resident ledger slot")
	}
}
