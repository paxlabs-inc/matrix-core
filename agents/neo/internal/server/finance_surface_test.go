// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"testing"

	"centra/agents/neo/internal/agent"
)

func TestFinanceToolPublishesDurableScreenEvent(t *testing.T) {
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	if !e.trace.Enabled() {
		t.Fatal("trace store must be enabled")
	}

	const intentID = "neo_finance_run"
	r := &run{id: intentID, convID: "conv_finance"}
	e.registerRun(r)
	e.broker.ensure(intentID)
	_, ch, cancel := e.broker.subscribe(intentID, 0)
	defer cancel()

	e.surfaceTool(r, agent.ToolEvent{
		ID:    "market-1",
		Name:  "finance__market_quote",
		Args:  map[string]interface{}{"symbol": "AAPL"},
		Phase: agent.ToolStart,
	})
	e.surfaceTool(r, agent.ToolEvent{
		ID:     "market-1",
		Name:   "finance__market_quote",
		Args:   map[string]interface{}{"symbol": "AAPL"},
		Result: `{"tool":"market_quote","quote":{"symbol":"AAPL","price":213.55,"source":"fmp","as_of":"2026-07-27T14:00:00Z"}}`,
		Phase:  agent.ToolEnd,
	})
	e.trace.Flush()

	var live []Event
	for len(live) < 3 {
		select {
		case ev := <-ch:
			live = append(live, ev)
		default:
			t.Fatalf("expected start, end and finance events; got %d", len(live))
		}
	}
	if live[0].Type != "tool.step" || live[0].Fields["title"] != "Checking market prices" {
		t.Fatalf("finance start step = %#v", live[0])
	}
	finance := live[2]
	if finance.Type != "tool.finance" {
		t.Fatalf("third event = %q, want tool.finance", finance.Type)
	}
	if finance.Fields["id"] != "market-1" || finance.Fields["tool"] != "market_quote" {
		t.Fatalf("finance identity fields = %#v", finance.Fields)
	}
	payload, ok := finance.Fields["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("finance payload type = %T", finance.Fields["payload"])
	}
	quote, ok := payload["quote"].(map[string]interface{})
	if !ok || quote["symbol"] != "AAPL" || quote["price"] != 213.55 {
		t.Fatalf("finance quote payload = %#v", payload["quote"])
	}

	persisted := e.trace.Load(intentID)
	financeEvents := 0
	for _, ev := range persisted {
		if ev.Type == "tool.finance" {
			financeEvents++
			if ev.Fields["tool"] != "market_quote" {
				t.Fatalf("persisted finance tool = %#v", ev.Fields["tool"])
			}
		}
	}
	if financeEvents != 1 {
		t.Fatalf("durable finance events = %d, want 1", financeEvents)
	}
}

func TestFinanceToolDropsUnstructuredResult(t *testing.T) {
	ev := agent.ToolEvent{
		Name:   "finance__market_quote",
		Result: "market data could not be loaded",
		Phase:  agent.ToolEnd,
	}
	if payload, ok := financeResult(ev); ok || payload != nil {
		t.Fatalf("unstructured finance result must not become a screen: %#v", payload)
	}
}

func TestFinanceResearchResultPublishesOnTheFinanceSurface(t *testing.T) {
	ev := agent.ToolEvent{
		Name:   "finance__market_research_get",
		Result: `{"tool":"market_research_get","research":{"id":"agent_run_1","status":"completed","output":{"grounding":[{"field":"ticker","citations":[{"url":"https://www.sec.gov/aapl"}]}]}}}`,
		Phase:  agent.ToolEnd,
	}
	payload, ok := financeResult(ev)
	if !ok || payload["tool"] != "market_research_get" {
		t.Fatalf("grounded finance result = %#v, %v", payload, ok)
	}
	if got := financeTitle("market_research_get"); got != "Reading grounded company research" {
		t.Fatalf("research title = %q", got)
	}
}
