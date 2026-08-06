// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/records"
)

// The recorded non-convergence shape: a turn that keeps generating and never
// converges burns its way toward a million tokens. It terminates AT the
// cumulative budget with an honest partial that delivers the work already
// done — not an error, not a dead turn, and not a claim of completion.
func TestTokenBurnTerminatesAtTheBudgetWithAnHonestPartial(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	const perCall = 120_000
	const budget = 600_000
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
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
			writeSSEToolUsage(
				writer, fmt.Sprintf("burn-%d", current), "exec__shell",
				map[string]interface{}{
					"command": fmt.Sprintf("printf burn-%d", current),
					"cwd":     workdir,
					"expect":  fmt.Sprintf("prints burn-%d", current),
				},
				perCall,
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "token-burn"
	userContent := "Keep refining the check until it stops being flaky."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second, MaxTurnTokens: budget,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)

	// The owner's core complaint: a bound must never end a turn in an error.
	if turnErr != nil {
		t.Fatalf("the token budget killed the turn: %v", turnErr)
	}
	if !response.HonestPartial {
		t.Fatalf("the budget exit was not flagged as an honest partial: %+v",
			response)
	}
	if response.Liveness.TokenBudget != budget {
		t.Fatalf("trace budget = %d want %d",
			response.Liveness.TokenBudget, budget)
	}
	if response.Liveness.TokensSpent < budget {
		t.Fatalf("the turn stopped at %d tokens, under its %d budget",
			response.Liveness.TokensSpent, budget)
	}
	trace := boundEnforcements(response.Liveness.Enforcements, boundTokenBudget)
	if len(trace) != 1 || trace[0].Limit != budget ||
		trace[0].Observed < budget {
		t.Fatalf("token budget trace = %+v", response.Liveness.Enforcements)
	}
	// It stopped as soon as it could, not after another full burn.
	if response.Liveness.TokensSpent > budget+perCall {
		t.Fatalf("the turn overshot its budget by more than one call: %d",
			response.Liveness.TokensSpent)
	}
	// The work already done is DELIVERED, not discarded.
	completed := len(response.ToolEvents)
	if completed < 4 {
		t.Fatalf("the burn produced only %d tool events", completed)
	}
	if !strings.Contains(
		response.Content, fmt.Sprintf("%d completed action", completed),
	) {
		t.Fatalf("the honest partial did not account for the %d completed "+
			"actions: %q", completed, response.Content)
	}
	if !strings.Contains(response.Content, "work limit") ||
		!strings.Contains(response.Content, "will not claim") {
		t.Fatalf("the honest partial did not state its boundary plainly: %q",
			response.Content)
	}
	// Every completed dispatch survived into the durable record.
	if response.Checkpoint == nil {
		t.Fatal("no durable checkpoint")
	}
	if results := countRole(
		response.Checkpoint.Messages, protocol.RoleTool,
	); results != completed {
		t.Fatalf("durable tool results = %d want %d", results, completed)
	}
	for _, event := range response.ToolEvents {
		if event.Error != "" {
			t.Fatalf("a completed dispatch was lost to the budget: %+v", event)
		}
	}
}

// The o1-budget-kill regression, pinned on the new runtime. A FINISHED answer
// must survive the budget: the generation that crosses the ceiling still
// carries a real answer, and that answer is delivered as itself — not replaced
// by an honest partial, not turned into "model call failed". The bound may only
// ever prevent the NEXT generation.
func TestFinishedAnswerSurvivesTheTokenBudget(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	const budget = 100_000
	final := "The readiness check printed its marker, so the environment is ready."
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
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
				writeSSEToolUsage(
					writer, "answer-evidence", "exec__shell",
					map[string]interface{}{
						"command": "printf ready-marker",
						"cwd":     workdir,
						"expect":  "prints ready-marker",
					},
					budget/2,
				)
				return
			}
			// This generation crosses the ceiling AND carries the answer.
			writeSSETextUsage(writer, final, budget)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "token-budget-finished-answer"
	userContent := "Check the readiness marker."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second, MaxTurnTokens: budget,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil {
		t.Fatalf("a finished answer was killed by the budget: %v", turnErr)
	}
	if response.Content != final {
		t.Fatalf("delivered content = %q want the model's finished answer %q",
			response.Content, final)
	}
	if response.HonestPartial {
		t.Fatal("a finished answer was downgraded to an honest partial")
	}
	if response.Liveness.TokensSpent < budget {
		t.Fatalf("the turn did not actually cross its budget: %d of %d",
			response.Liveness.TokensSpent, budget)
	}
	if len(response.Liveness.Enforcements) != 0 {
		t.Fatalf("the budget fired on a delivered answer: %+v",
			response.Liveness.Enforcements)
	}
}

func TestCommittedEvidenceReceivesReservedSynthesisAfterBudgetIsSpent(
	t *testing.T,
) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	const budget = 100_000
	final := "The readiness evidence was committed successfully, so the checked environment is ready for the requested work."
	var (
		mu   sync.Mutex
		step int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
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
				writeSSEToolUsage(
					writer, "reserved-synthesis-evidence", "exec__shell",
					map[string]interface{}{
						"command": "printf ready-marker",
						"cwd":     workdir,
						"expect":  "prints ready-marker",
					},
					budget,
				)
				return
			}
			if len(decoded.Tools) != 0 {
				http.Error(writer, "synthesis request still advertised tools", http.StatusConflict)
				return
			}
			writeSSEText(writer, final)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "reserved-synthesis-after-budget"
	userContent := "Check the readiness marker."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(
		manager, &DurableEffectJournal{Store: store},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second, MaxTurnTokens: budget,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil {
		t.Fatalf("reserved synthesis failed: %v", turnErr)
	}
	mu.Lock()
	generations := step
	mu.Unlock()
	if generations != 2 || response.Content != final || response.HonestPartial {
		t.Fatalf("reserved synthesis generations=%d response=%+v", generations, response)
	}
	if len(response.Liveness.Enforcements) != 0 {
		t.Fatalf("budget displaced committed synthesis: %+v", response.Liveness.Enforcements)
	}
	turn, err := store.LoadTurnRecord(t.Context(), turnID)
	if err != nil || turn.CurrentState != records.StateDelivered || turn.SynthesisDebt.Owed {
		t.Fatalf("reserved synthesis durable turn=%+v err=%v", turn, err)
	}
}

// Deterministic repetition stays inside one logical turn. It cannot manufacture
// a fresh retry budget by forcing a supervisor respawn.
func TestTokenBudgetContainsRepetitionWithoutRespawn(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	const perCall = 40_000
	const budget = 300_000
	var (
		mu       sync.Mutex
		step     int
		resuming bool
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			step++
			current := step
			// Before the resume every call repeats one signature so the
			// same-strategy bound ends the first run under the ceiling; after
			// it, every call is a fresh strategy so only the token budget can
			// stop the turn.
			command := "printf settled"
			if resuming {
				command = fmt.Sprintf("printf settled-%d", current)
			}
			mu.Unlock()
			writeSSEToolUsage(
				writer, fmt.Sprintf("resume-%d", current), "exec__shell",
				map[string]interface{}{
					"command": command,
					"cwd":     workdir,
					"expect":  "prints settled",
				},
				perCall,
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "token-budget-resume"
	userContent := "Work the check until it settles."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		TurnID: turnID, Model: "mimo-v2",
		IdleTimeout: 30 * time.Second, MaxTurnTokens: budget,
	}
	first, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store, config,
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := first.Turn(t.Context(), userContent)
	if turnErr != nil || !response.HonestPartial {
		t.Fatalf("first run = %+v err=%v", response, turnErr)
	}
	spent := response.Liveness.TokensSpent
	if spent < budget {
		t.Fatalf("single logical run spent %d of %d without reaching its ceiling",
			spent, budget)
	}
	if response.ProviderCalls != (budget+perCall-1)/perCall+1 {
		t.Fatalf("provider calls=%d exceeded one bounded turn", response.ProviderCalls)
	}
}

func writeSSEToolUsage(
	writer http.ResponseWriter,
	id string,
	name string,
	arguments map[string]interface{},
	totalTokens int,
) {
	writer.Header().Set("Content-Type", "text/event-stream")
	rawArguments, _ := json.Marshal(arguments)
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{
					"index": 0, "id": id, "type": "function",
					"function": map[string]interface{}{
						"name": name, "arguments": string(rawArguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": usageFrame(totalTokens),
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func writeSSETextUsage(
	writer http.ResponseWriter,
	content string,
	totalTokens int,
) {
	writer.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]interface{}{"content": content},
			"finish_reason": "stop",
		}},
		"usage": usageFrame(totalTokens),
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func usageFrame(totalTokens int) map[string]interface{} {
	completion := totalTokens / 4
	return map[string]interface{}{
		"prompt_tokens":     totalTokens - completion,
		"completion_tokens": completion,
		"total_tokens":      totalTokens,
	}
}
