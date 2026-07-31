// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"testing"

	"matrix/neo/internal/agent"
)

func TestExaResultPublishesDurableSearchCards(t *testing.T) {
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)

	const intentID = "neo_exa_run"
	r := &run{id: intentID, convID: "conv_exa"}
	e.registerRun(r)
	e.broker.ensure(intentID)
	_, ch, cancel := e.broker.subscribe(intentID, 0)
	defer cancel()

	e.surfaceTool(r, agent.ToolEvent{
		ID:     "exa-1",
		Name:   "exa__exa_search",
		Args:   map[string]interface{}{"query": "grounded topic"},
		Result: `{"tool":"exa_search","provider":"exa","query":"grounded topic","answer":"Generated synthesis","results":[{"title":"Primary source","url":"https://example.com/source","snippet":"Extractive evidence"}]}`,
		Phase:  agent.ToolEnd,
	})
	e.trace.Flush()

	step := <-ch
	search := <-ch
	if step.Type != "tool.step" || step.Fields["title"] != "Searching grounded sources" {
		t.Fatalf("Exa step = %#v", step)
	}
	if search.Type != "tool.search" || search.Fields["provider"] != "exa" || search.Fields["answer"] != "Generated synthesis" {
		t.Fatalf("Exa search event = %#v", search)
	}
	results, ok := search.Fields["results"].([]map[string]interface{})
	if !ok || len(results) != 1 || results[0]["url"] != "https://example.com/source" {
		t.Fatalf("Exa cards = %#v", search.Fields["results"])
	}

	persisted := e.trace.Load(intentID)
	searchEvents := 0
	for _, event := range persisted {
		if event.Type == "tool.search" {
			searchEvents++
		}
	}
	if searchEvents != 1 {
		t.Fatalf("durable Exa search events = %d, want 1", searchEvents)
	}
}

func TestExaStepTitlesDescribeLifecycle(t *testing.T) {
	cases := map[string]string{
		"exa__exa_contents":          "Reading source evidence",
		"exa__exa_research_start":    "Starting grounded research",
		"exa__exa_research_get":      "Reading grounded research",
		"exa__exa_research_continue": "Continuing grounded research",
		"exa__exa_research_cancel":   "Stopping grounded research",
		"exa__exa_outbound_brief":    "Researching an outbound brief",
		"exa__exa_social_draft":      "Researching a social draft",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			step := describeStep(agent.ToolEvent{Name: name, Phase: agent.ToolStart})
			if step["surface"] != "search" || step["title"] != want {
				t.Fatalf("step = %#v, want title %q", step, want)
			}
		})
	}
}
