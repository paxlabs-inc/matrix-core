// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestEvidenceBackedResearchAnswerCommitsWithoutLiteralURL(t *testing.T) {
	manager := explorationExecManager(t)
	workdir := t.TempDir()
	const answer = "The AI agent landscape is consolidating around tool-using model runtimes, durable context, governance, observability, and enterprise workflow integration, with reliability now mattering as much as raw model capability."
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded gatewayRequest
		_ = json.Unmarshal(body, &decoded)
		if handleCapabilityCanary(writer, decoded) {
			return
		}
		mu.Lock()
		step++
		current := step
		mu.Unlock()
		if current == 1 {
			writeSSETool(writer, "research-evidence", "fetch__shell", map[string]interface{}{
				"command": "printf 'agent runtimes now emphasize durable context, governance, observability, and workflow integration'",
				"cwd":     workdir,
				"expect":  "returns current AI agent landscape evidence",
			})
			return
		}
		writeSSEText(writer, answer)
	}))
	t.Cleanup(gateway.Close)

	turnID := "research-answer-survival"
	request := "Research the latest information on the AI agent landscape."
	store := realTurnStore(t, turnID, request)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{TurnID: turnID, Model: "mimo-v2", IdleTimeout: 30 * time.Second},
		Dependencies{Observer: NewReporterObserver(reporter, 0)},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), request)
	if turnErr != nil {
		t.Fatalf("research answer was not delivered: %v", turnErr)
	}
	if response.Content != answer || response.HonestPartial {
		t.Fatalf("research answer was replaced: content=%q honest_partial=%v", response.Content, response.HonestPartial)
	}
	if len(response.ToolEvents) != 1 || response.ToolEvents[0].Error != "" {
		t.Fatalf("research evidence was not preserved: %+v", response.ToolEvents)
	}
	deltas := reporter.snapshot()
	answerTurn := -1
	for _, delta := range deltas {
		if delta.Channel == "content" && delta.Text != "" {
			answerTurn = delta.Turn
		}
	}
	for _, delta := range deltas {
		if delta.Channel == "retraction" && delta.Turn == answerTurn {
			t.Fatalf("accepted research answer was retracted: %+v", deltas)
		}
	}
	mu.Lock()
	providerCalls := step
	mu.Unlock()
	if providerCalls != 2 {
		t.Fatalf("provider calls = %d, want tool selection plus one synthesis", providerCalls)
	}
}
