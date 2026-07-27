// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/neo/internal/runtime/protocol"
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

// The cumulative budget is durable: tokens merged by an earlier run of the same
// turn ride the checkpoint, so a respawn continues ONE budget instead of buying
// a fresh one on every resume.
//
// The first run stops UNDER the ceiling (on the same-strategy bound), so the
// resumed run has real budget left. It gets exactly the remainder: three more
// generations, not the eight a fresh budget would have paid for.
func TestTokenBudgetIsCumulativeAcrossResume(t *testing.T) {
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
	var incomplete *Incomplete
	if !errors.As(turnErr, &incomplete) || incomplete.Phase != "tool_loop" {
		t.Fatalf("first run = %+v err=%v", response, turnErr)
	}
	spent := response.Liveness.TokensSpent
	if spent == 0 || spent >= budget {
		t.Fatalf("first run spent %d of %d: it must stop UNDER the ceiling",
			spent, budget)
	}
	dispatched := len(response.ToolEvents)

	mu.Lock()
	beforeResume := step
	resuming = true
	mu.Unlock()
	resumed, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store, config,
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resumeResponse, resumeErr := resumed.Resume(
		t.Context(), userContent, incomplete.Checkpoint,
	)
	if resumeErr != nil {
		t.Fatalf("resume = %+v err=%v", resumeResponse, resumeErr)
	}
	if !resumeResponse.HonestPartial {
		t.Fatalf("the resumed turn did not stop at the budget: %+v",
			resumeResponse)
	}
	if resumeResponse.Liveness.TokensSpent < budget {
		t.Fatalf("resumed spend = %d never reached the %d budget",
			resumeResponse.Liveness.TokensSpent, budget)
	}
	mu.Lock()
	afterResume := step
	mu.Unlock()
	remainder := (budget - spent + perCall - 1) / perCall
	if generations := afterResume - beforeResume; generations != remainder {
		t.Fatalf("the resumed turn made %d generations; carrying %d of the %d"+
			" budget leaves exactly %d", generations, spent, budget, remainder)
	}
	// The evidence from before the interruption came back with the checkpoint.
	if len(resumeResponse.ToolEvents) <= dispatched {
		t.Fatalf("resumed evidence = %d lost the carried %d",
			len(resumeResponse.ToolEvents), dispatched)
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
