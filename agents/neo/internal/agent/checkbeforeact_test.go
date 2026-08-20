// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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

	mcllm "centra/core/mcl/llm"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/tools"
)

// TestSelfClaimDischargeAllowsDispatch proves the discharge path (req.5.1):
// a self-referential premise that MATCHES a resident surface fact is cited
// from the self-model at the gate and the dispatch proceeds unrefused.
func TestSelfClaimDischargeAllowsDispatch(t *testing.T) {
	cfg := config.Default()
	cfg.EpistemicPremises = true
	a := New(Options{Config: cfg, Capability: trueSurface()})
	a.turn.ledger = &premiseLedger{}
	p := a.turn.ledger.add(Premise{Statement: "clients reach me via POST /chat", Status: premiseAssumption, SelfRef: true})

	calls := []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"x"}`}}}
	allowed, refused := a.checkBeforeAct(calls)
	if refused || len(allowed) != 1 {
		t.Fatalf("a citable self-claim must not refuse dispatch (refused=%v allowed=%d)", refused, len(allowed))
	}
	if p.Status != premiseCited || p.Source != "self-model" {
		t.Fatalf("the gate's check must discharge the premise to cited(self-model); got %+v", p)
	}
}

// TestSelfClaimToolInventoryDischarge pins the false-refutation regression
// (2026-07-13 layerx transcript): a TRUE premise naming an advertised tool
// must discharge to cited against the a.schemas-derived inventory — never be
// refuted against the HTTP API surface — and the dependent dispatch proceeds.
func TestSelfClaimToolInventoryDischarge(t *testing.T) {
	cfg := config.Default()
	cfg.EpistemicPremises = true
	a := New(Options{Config: cfg, Capability: trueSurface()})
	a.schemas = append(a.schemas, llm.NewFunctionTool("layerx__layerx_pay", "Pay USDX over LayerX", nil))
	a.turn.ledger = &premiseLedger{}
	p := a.turn.ledger.add(Premise{Statement: "layerx__layerx_pay is in my tool inventory", Status: premiseAssumption, SelfRef: true})

	calls := []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "layerx__layerx_pay", Arguments: `{}`}}}
	allowed, refused := a.checkBeforeAct(calls)
	if refused || len(allowed) != 1 {
		t.Fatalf("a true tool-inventory claim must not refuse dispatch (refused=%v allowed=%d)", refused, len(allowed))
	}
	if p.Status != premiseCited || p.Source != "self-model" || !strings.Contains(p.Citation, "layerx__layerx_pay") {
		t.Fatalf("the claim must discharge to cited(self-model) with the tool as citation; got %+v", p)
	}
}

// TestSelfClaimUnmatchedStaysAssumption pins the verdict polarity: a self-
// claim the coarse matcher can neither cite nor positively contradict stays a
// visible ASSUMPTION (claimUnknown) — absence of a substring match is not
// falsity, and dispatch is never refused over it.
func TestSelfClaimUnmatchedStaysAssumption(t *testing.T) {
	cfg := config.Default()
	cfg.EpistemicPremises = true
	a := New(Options{Config: cfg, Capability: trueSurface()})
	a.turn.ledger = &premiseLedger{}
	p := a.turn.ledger.add(Premise{Statement: "I keep a durable memory of past sessions", Status: premiseAssumption, SelfRef: true})

	calls := []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"x"}`}}}
	allowed, refused := a.checkBeforeAct(calls)
	if refused || len(allowed) != 1 {
		t.Fatalf("an unmatched self-claim must never refuse dispatch (refused=%v allowed=%d)", refused, len(allowed))
	}
	if p.Status != premiseAssumption {
		t.Fatalf("an unmatched self-claim must stay an assumption, got %q (citation %q)", p.Status, p.Citation)
	}
}

// TestSelfClaimAbsentToolRefuted proves the sound half of the complete-
// inventory posture survives: a claim naming a concrete tool-shaped token
// (alias__name) that matches NO advertised schema is positively false — the
// premise is refuted and the dependent dispatch is refused.
func TestSelfClaimAbsentToolRefuted(t *testing.T) {
	cfg := config.Default()
	cfg.EpistemicPremises = true
	a := New(Options{Config: cfg, Capability: trueSurface()})
	a.turn.ledger = &premiseLedger{}
	p := a.turn.ledger.add(Premise{Statement: "I can call ghost__teleport to move the funds", Status: premiseAssumption, SelfRef: true})

	calls := []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"x"}`}}}
	allowed, refused := a.checkBeforeAct(calls)
	if !refused || len(allowed) != 0 {
		t.Fatalf("a named-but-absent tool claim must refuse dispatch (refused=%v allowed=%d)", refused, len(allowed))
	}
	if p.Status != premiseRefuted || !strings.Contains(p.Citation, "ghost__teleport") {
		t.Fatalf("the claim must be refuted naming the absent tool; got %+v", p)
	}
	if a.turn.revisionPending == "" {
		t.Fatal("a genuine refutation must force the plan-revision step")
	}
}

// TestExtractorSelfMislabelStaysPlain pins the SelfRef gate on the model
// half of the extractor: a "self"-basis row the deterministic detector does
// NOT recognize (arithmetic, world facts) stays a plain assumption — it can
// never enter the gate's refutation path — while a genuinely self-shaped row
// still becomes gate-checkable.
func TestExtractorSelfMislabelStaysPlain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rows := `[{\"statement\":\"25 x 10 = 250 USDX\",\"basis\":\"self\",\"citation\":\"\"},{\"statement\":\"callers integrate through my own api\",\"basis\":\"self\",\"citation\":\"\"}]`
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"%s\"},\"finish_reason\":\"stop\"}]}\n", rows)
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
	a := New(Options{Config: config.Default(), Cheap: client})

	got := a.extractPremisesModel(context.Background(), "Plan: pay 25 vendors 10 USDX each.")
	if len(got) != 2 {
		t.Fatalf("expected 2 extracted premises, got %d (%+v)", len(got), got)
	}
	for _, p := range got {
		switch {
		case strings.Contains(p.Statement, "250 USDX"):
			if p.SelfRef || p.Status != premiseAssumption {
				t.Fatalf("a mislabeled arithmetic row must stay a plain assumption; got %+v", p)
			}
		case strings.Contains(p.Statement, "my own api"):
			if !p.SelfRef || p.Status != premiseAssumption {
				t.Fatalf("a genuinely self-shaped row must be a gate-checkable self-assumption; got %+v", p)
			}
		default:
			t.Fatalf("unexpected premise %+v", p)
		}
	}
}

// TestCheckBeforeActArenaShape proves refusal, refute→forced-revision, and the
// self-claim contradiction mismatch event (req.5.1/5.2/5.3/5.4) on the REAL
// loop and dispatch seams. The model opens with the arena-shaped plan — an
// OpenAI-compatible-API self-claim — and tries to scaffold; the resident
// capability surface refutes the claim at the gate, the scaffold call is
// refused (never dispatched), a tools-stripped revision step is forced, and
// only the revised, grounded plan's action dispatches.
func TestCheckBeforeActArenaShape(t *testing.T) {
	const scaffold = `{"index":0,"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"bridge.py\",\"content\":\"...\"}"}}`
	var (
		mu         sync.Mutex
		bodies     []string
		mainCall   int
		dispatched []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")

		if strings.Contains(body, "load-bearing FACTUAL PREMISES") {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"[]"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}

		mu.Lock()
		bodies = append(bodies, body)
		mainCall++
		call := mainCall
		mu.Unlock()

		switch {
		case call == 1:
			// The arena plan: an ungrounded self-claim, then a scaffold action.
			plan := "Plan: the Neo side I can wire up directly through my own OpenAI-compatible API. Scaffolding the bridge now."
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q,\"tool_calls\":[%s]}}]}\n", plan, scaffold)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		case call == 2:
			// The forced revision step: answer with the grounded revised plan.
			if !strings.Contains(body, "NO tools on purpose") {
				t.Errorf("call 2 must be the forced revision step; body lacks the revision directive")
			}
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Revised plan: there is no OpenAI-compatible endpoint on the Neo side; the bridge integrates via the POST /chat + SSE /events surface instead."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		case call == 3:
			// Post-revision: the grounded scaffold action dispatches.
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", scaffold)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
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
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = true
	a := New(Options{
		Config:     cfg,
		Main:       client,
		Cheap:      client,
		Tools:      &tools.Manager{},
		Capability: trueSurface(),
		Observer: func(ev ToolEvent) {
			if ev.Phase == ToolStart {
				mu.Lock()
				dispatched = append(dispatched, ev.Name)
				mu.Unlock()
			}
		},
	})

	if err := a.Chat(context.Background(), "bridge Neo and the hermes instance"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// The first scaffold was REFUSED (the arena action never dispatched); the
	// post-revision scaffold DID dispatch — exactly one write_file dispatch.
	writes := 0
	for _, n := range dispatched {
		if n == "write_file" {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("exactly the post-revision scaffold must dispatch; write_file dispatched %d times (%v)", writes, dispatched)
	}

	all := strings.Join(bodies, "\n====\n")
	if !strings.Contains(all, "not dispatched (check-before-act)") {
		t.Fatal("the refusal directive must ride the transcript, naming the gate")
	}
	if !strings.Contains(all, "Self-claim contradiction") {
		t.Fatal("the self-claim contradiction must surface as a mismatch event (req.5.3)")
	}

	// The revision step ran tools-stripped (req.5.2): its request (call 2)
	// carried no tool schemas, while the normal steps did.
	if len(bodies) < 3 {
		t.Fatalf("a forced revision step + a post-revision step must run; got %d model calls", len(bodies))
	}
	if strings.Contains(bodies[1], `"tools"`) {
		t.Fatal("the forced revision step must be tools-stripped")
	}
	if !strings.Contains(bodies[0], `"tools"`) {
		t.Fatal("sanity: normal steps advertise tools")
	}

	// The refuted premise was answered by the revision, and the revised plan
	// carries no standing refutation.
	if n := len(a.turn.ledger.unrevisedRefuted()); n != 0 {
		t.Fatalf("the revision must answer the refuted premise; %d still standing", n)
	}
}
