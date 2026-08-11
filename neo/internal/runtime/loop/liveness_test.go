// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	executortool "matrix/executor/tool"
	"matrix/neo/internal/runtime/liveness"
	"matrix/neo/internal/runtime/protocol"
	neotools "matrix/neo/internal/tools"
)

// The tool-call budget is unexceedable by construction: a real gateway that
// never stops asking for another tool runs into the derived budget and the turn
// ends as a typed incomplete over a durable checkpoint, not an unbounded burn.
// Every call carries a distinct argument, so the same-strategy bound and the
// repetition breaker cannot be what stopped it.
func TestToolCallBudgetCannotBeExceededOnTheRealLoop(t *testing.T) {
	manager := realExecManager(t)
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
			writeSSETool(
				writer, fmt.Sprintf("budget-%d", current), "exec__shell",
				map[string]interface{}{
					"command": fmt.Sprintf("printf budget-step-%d", current),
					"cwd":     t.TempDir(),
					"expect":  fmt.Sprintf("prints budget-step-%d", current),
				},
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "liveness-tool-budget"
	userContent := "Print each readiness marker in order."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)

	if turnErr != nil || !response.HonestPartial || response.Checkpoint == nil ||
		len(response.Checkpoint.Messages) == 0 {
		t.Fatalf("unbounded tool loop was not contained: response=%+v err=%v", response, turnErr)
	}
	budget := response.Liveness.Policy.ToolCallBudget
	if budget != 22 {
		t.Fatalf("derived tool call budget = %d want the healthy 22", budget)
	}
	if len(response.ToolEvents) != budget {
		t.Fatalf("executed %d tools under a budget of %d",
			len(response.ToolEvents), budget)
	}
	if dispatched := countRole(
		response.Checkpoint.Messages, protocol.RoleTool,
	); dispatched != budget {
		t.Fatalf("durable tool results = %d want %d", dispatched, budget)
	}
	if len(response.Liveness.Enforcements) != 1 ||
		response.Liveness.Enforcements[0].Bound != boundToolBudget ||
		response.Liveness.Enforcements[0].Limit != budget {
		t.Fatalf("enforcement trace = %+v", response.Liveness.Enforcements)
	}
	for _, event := range response.ToolEvents {
		if event.Error != "" {
			t.Fatalf("a budgeted dispatch failed: %+v", event)
		}
	}
}

// Over-budget exploration returns an INJECTED ERROR RESULT rather than killing
// the turn: the model sees a real tool result saying the budget is spent, keeps
// working, and the turn still delivers. Nothing is dropped silently and the
// refused calls never execute.
func TestExplorationBudgetInjectsErrorResultsAndStillDelivers(t *testing.T) {
	manager := explorationExecManager(t)
	workdir := t.TempDir()
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
			if current > 6 {
				writeSSEText(
					writer,
					"Four readiness markers were gathered before the "+
						"exploration budget was spent, and the remaining "+
						"lookups were refused rather than guessed.",
				)
				return
			}
			writeSSETool(
				writer, fmt.Sprintf("explore-%d", current), "fetch__shell",
				map[string]interface{}{
					"command": fmt.Sprintf("printf marker-%d", current),
					"cwd":     workdir,
					"expect":  fmt.Sprintf("prints marker-%d", current),
				},
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "liveness-exploration-budget"
	userContent := "Print the readiness markers."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil {
		t.Fatalf("exploration budget killed the turn: %v", turnErr)
	}
	if response.HonestPartial ||
		!strings.Contains(response.Content, "exploration budget") {
		t.Fatalf("delivery = %+v", response)
	}
	budget := response.Liveness.Policy.ExplorationBudget
	if budget != 4 {
		t.Fatalf("derived exploration budget = %d want the direct-shape 4",
			budget)
	}
	executed, refused := 0, 0
	ran := make(map[string]bool)
	for _, event := range response.ToolEvents {
		if strings.Contains(string(event.Result), "exploration budget") {
			refused++
			if event.Error == "" {
				t.Fatalf("a refused call was recorded as clean: %+v", event)
			}
			if event.Citation != nil {
				t.Fatalf("a call that never ran was journalled: %+v", event)
			}
			continue
		}
		executed++
		for index := 1; index <= 6; index++ {
			marker := fmt.Sprintf("marker-%d", index)
			if strings.Contains(string(event.Result), marker) {
				ran[marker] = true
			}
		}
	}
	if executed != budget || refused != 2 {
		t.Fatalf("executed=%d refused=%d under a budget of %d",
			executed, refused, budget)
	}
	// The refused work never reached the real bridge: only the first four
	// markers ever came back from a real dispatch.
	for index := 1; index <= 6; index++ {
		marker := fmt.Sprintf("marker-%d", index)
		if ran[marker] != (index <= budget) {
			t.Fatalf("%s executed=%t under a budget of %d",
				marker, ran[marker], budget)
		}
	}
	// The refusals are TOOL RESULTS, so they belong in the durable window: the
	// model emitted those call IDs and has to see a result for each.
	if response.Checkpoint == nil {
		t.Fatal("no durable checkpoint")
	}
	durable := 0
	for _, message := range response.Checkpoint.Messages {
		if message.Role == protocol.RoleTool &&
			strings.Contains(message.Content, "exploration budget") {
			durable++
		}
	}
	if durable != refused {
		t.Fatalf("durable refusals = %d want %d", durable, refused)
	}
	enforcements := boundEnforcements(
		response.Liveness.Enforcements, boundExplorationBudget,
	)
	if len(enforcements) != 2 {
		t.Fatalf("exploration enforcement trace = %+v",
			response.Liveness.Enforcements)
	}
}

// Excess parallel calls are DEFERRED with explicit tool-result markers, never
// dropped: every call ID the model emitted gets a result back, the surplus ones
// carry the reason, and only the allowed prefix actually dispatches.
func TestParallelismDefersExcessCallsWithExplicitMarkers(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	var (
		mu   sync.Mutex
		step int
	)
	batch := make([]toolFrame, 0, 6)
	for index := 1; index <= 6; index++ {
		command := fmt.Sprintf("printf fan-%d", index)
		if index == 2 {
			command = "exit 7"
		}
		batch = append(batch, toolFrame{
			ID: fmt.Sprintf("fan-%d", index), Name: "exec__shell",
			Arguments: map[string]interface{}{
				"command": command,
				"cwd":     workdir,
				"expect":  fmt.Sprintf("prints fan-%d", index),
			},
		})
	}
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
				writeSSEToolBatch(writer, batch)
				return
			}
			writeSSEText(
				writer,
				"The allowed fan-out ran and the surplus calls were deferred "+
					"rather than dropped.",
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "liveness-parallelism"
	userContent := "Run the fan-out steps."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil {
		t.Fatalf("parallelism bound killed the turn: %v", turnErr)
	}
	parallelism := response.Liveness.Policy.Parallelism
	if parallelism != 4 {
		t.Fatalf("derived parallelism = %d want the healthy 4", parallelism)
	}
	executed, deferredCount, failed := 0, 0, 0
	for _, event := range response.ToolEvents {
		if strings.Contains(string(event.Result), "dispatch deferred") {
			deferredCount++
			continue
		}
		executed++
		var outcome map[string]interface{}
		if err := json.Unmarshal(event.Result, &outcome); err != nil {
			t.Fatalf("unstructured executed result: %s: %v", event.Result, err)
		}
		if outcome["outcome"] == "error" {
			failed++
		} else if outcome["outcome"] != "success" {
			t.Fatalf("unexpected structured outcome: %+v", outcome)
		}
	}
	if executed != parallelism || failed != 1 || deferredCount != len(batch)-parallelism {
		t.Fatalf("executed=%d deferred=%d for a batch of %d under %d",
			executed, deferredCount, len(batch), parallelism)
	}
	// Nothing was dropped: every emitted call ID has exactly one result.
	if response.Checkpoint == nil {
		t.Fatal("no durable checkpoint")
	}
	results := make(map[string]string)
	for _, message := range response.Checkpoint.Messages {
		if message.Role != protocol.RoleTool {
			continue
		}
		if _, exists := results[message.ToolCallID]; exists {
			t.Fatalf("call %s got two results", message.ToolCallID)
		}
		results[message.ToolCallID] = message.Content
	}
	for index, call := range batch {
		content, exists := results[call.ID]
		if !exists {
			t.Fatalf("call %s was dropped silently", call.ID)
		}
		if index < parallelism {
			if strings.Contains(content, "dispatch deferred") {
				t.Fatalf("call %s inside the bound was deferred", call.ID)
			}
			continue
		}
		if content != parallelismDeferredResult {
			t.Fatalf("deferred marker for %s = %q", call.ID, content)
		}
	}
	if len(boundEnforcements(
		response.Liveness.Enforcements, boundParallelism,
	)) != len(batch)-parallelism {
		t.Fatalf("parallelism enforcement trace = %+v",
			response.Liveness.Enforcements)
	}
}

// Past the same-strategy bound the runtime returns structured observations to
// the same cognitive run and only the overall turn budget may stop it.
func TestSameStrategyRepetitionRequiresStrategyChangeInBand(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
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
			// Identical canonical signature every time, only the call ID moves.
			writeSSETool(
				writer, fmt.Sprintf("repeat-%d", current), "exec__shell",
				map[string]interface{}{
					"command": "printf same-strategy",
					"cwd":     workdir,
					"expect":  "prints same-strategy",
				},
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "liveness-same-strategy"
	userContent := "Confirm the marker."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil || !response.HonestPartial || response.Checkpoint == nil ||
		len(response.Checkpoint.Messages) == 0 {
		t.Fatalf("same-strategy containment response=%+v err=%v", response, turnErr)
	}
	retries := response.Liveness.Policy.SameStrategyRetries
	if retries != 3 {
		t.Fatalf("derived same-strategy retries = %d want 3", retries)
	}
	dispatched := 0
	contained := 0
	for _, event := range response.ToolEvents {
		if event.IdempotencyKey != "" {
			dispatched++
		}
		if strings.Contains(string(event.Result), `"failure_layer":"convergence"`) {
			contained++
		}
	}
	if dispatched != retries+1 || contained == 0 {
		t.Fatalf("same-strategy dispatches=%d contained=%d bound=%d",
			dispatched, contained, retries)
	}
	trace := boundEnforcements(
		response.Liveness.Enforcements, boundSameStrategy,
	)
	if len(trace) == 0 || trace[0].Limit != retries {
		t.Fatalf("same-strategy trace = %+v", response.Liveness.Enforcements)
	}
}

// The repetition circuit breaker runs over canonical tool signatures, so it
// sees the interleaved churn a consecutive-repeat bound and the legacy
// similarity heuristic both miss. It trips at a phase boundary and the turn
// ends as a typed incomplete.
func TestCircuitBreakerStopsInterleavedSignatureChurn(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
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
			marker := "alpha"
			if current%2 == 0 {
				marker = "beta"
			}
			writeSSETool(
				writer, fmt.Sprintf("churn-%d", current), "exec__shell",
				map[string]interface{}{
					"command": "printf " + marker,
					"cwd":     workdir,
					"expect":  "prints " + marker,
				},
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "liveness-circuit-provider-phase"
	userContent := "Alternate the two checks until they agree."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil || !response.HonestPartial {
		t.Fatalf("interleaved churn was not contained in-band: response=%+v err=%v",
			response, turnErr)
	}
	// The consecutive-repeat bound never fired: no signature ever repeated
	// back to back, which is exactly the gap this breaker closes.
	if len(boundEnforcements(
		response.Liveness.Enforcements, boundSameStrategy,
	)) != 0 {
		t.Fatalf("same-strategy bound fired on interleaved churn: %+v",
			response.Liveness.Enforcements)
	}
	dispatched := len(response.ToolEvents)
	if dispatched != 2*liveness.DefaultRepeatThreshold+1 {
		t.Fatalf("breaker tripped after %d dispatches", dispatched)
	}
	if dispatched >= response.Liveness.Policy.ToolCallBudget {
		t.Fatalf("the tool budget, not the breaker, ended the turn: %d",
			dispatched)
	}
	// The provider phase catches the churn, then the canonical runtime spends
	// its reserved synthesis generation. MiMo performs one bounded conformance
	// retry when the scripted provider ignores the tools-disabled request.
	mu.Lock()
	generations := step
	mu.Unlock()
	if generations != dispatched+2 {
		t.Fatalf("provider generations = %d dispatches = %d: want the breaker"+
			" generation plus the bounded synthesis attempt", generations, dispatched)
	}
}

// The tool phase and the turn phase are load-bearing too. Inside a single
// batch the breaker refuses the remaining calls before they dispatch, and the
// open ledger rides the durable checkpoint, so a respawn that resumes the
// checkpoint refuses at the turn phase without spending a single provider call.
func TestCircuitBreakerRefusesInsideABatchAndSurvivesResume(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	var (
		mu    sync.Mutex
		calls int
	)
	batch := make([]toolFrame, 0, 4)
	for index := 1; index <= 4; index++ {
		batch = append(batch, toolFrame{
			ID: fmt.Sprintf("batch-%d", index), Name: "exec__shell",
			Arguments: map[string]interface{}{
				"command": "printf batched",
				"cwd":     workdir,
				"expect":  "prints batched",
			},
		})
	}
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			calls++
			mu.Unlock()
			writeSSEToolBatch(writer, batch)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "liveness-circuit-batch"
	userContent := "Confirm the batched marker."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		TurnID: turnID, Model: "mimo-v2",
		IdleTimeout: 30 * time.Second,
		Breaker:     liveness.BreakerConfig{RepeatThreshold: 2},
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store, config,
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil || !response.HonestPartial {
		t.Fatalf("in-batch churn = %+v err=%v", response, turnErr)
	}
	// The fourth call in the batch never dispatched.
	if len(response.ToolEvents) != 3 {
		t.Fatalf("dispatched %d calls of a 4-call batch under a threshold of 2",
			len(response.ToolEvents))
	}
	mu.Lock()
	afterTurn := calls
	mu.Unlock()
	if afterTurn != 3 {
		t.Fatalf("provider generations = %d want tool batch plus bounded synthesis", afterTurn)
	}

	if response.Checkpoint == nil {
		t.Fatal("contained circuit result lacks its durable logical-turn checkpoint")
	}
}

// Silence when healthy. A run that never approaches a bound produces exactly
// the transcript it would have produced with no policy at all: no injected
// results, no deferred markers, no early exit, and not one word of policy
// guidance in any request the provider saw. The enforcement trace is empty.
func TestHealthyRunCarriesNoEnforcementResidue(t *testing.T) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	final := "Both readiness markers printed, so the environment is ready."
	var (
		mu       sync.Mutex
		step     int
		requests []gatewayRequest
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
			requests = append(requests, decoded)
			mu.Unlock()
			if current > 2 {
				writeSSEText(writer, final)
				return
			}
			writeSSETool(
				writer, fmt.Sprintf("healthy-%d", current), "exec__shell",
				map[string]interface{}{
					"command": fmt.Sprintf("printf healthy-%d", current),
					"cwd":     workdir,
					"expect":  fmt.Sprintf("prints healthy-%d", current),
				},
			)
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "liveness-healthy"
	userContent := "Check the readiness markers."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
	if turnErr != nil {
		t.Fatalf("healthy turn failed: %v", turnErr)
	}
	if response.Content != final {
		t.Fatalf("delivered content = %q want %q", response.Content, final)
	}
	if len(response.Liveness.Enforcements) != 0 {
		t.Fatalf("a healthy run enforced something: %+v",
			response.Liveness.Enforcements)
	}
	policy := response.Liveness.Policy
	if policy.ToolCallBudget != 22 || policy.Parallelism != 4 ||
		policy.ExplorationBudget != 4 || policy.SameStrategyRetries != 3 ||
		len(policy.Causes) != 0 {
		t.Fatalf("healthy policy = %+v", policy)
	}
	if response.Checkpoint == nil {
		t.Fatal("no durable checkpoint")
	}
	if sequence := roleSequence(
		response.Checkpoint.Messages,
	); sequence != "user,assistant,tool,assistant,tool,assistant" {
		t.Fatalf("durable role sequence = %s", sequence)
	}
	for _, message := range response.Checkpoint.Messages {
		assertNoEnforcementResidue(t, "durable transcript", message.Content)
	}
	for _, event := range response.ToolEvents {
		assertNoEnforcementResidue(t, "evidence", string(event.Result))
		if event.Error != "" {
			t.Fatalf("healthy run recorded a failed dispatch: %+v", event)
		}
	}
	if len(response.ToolEvents) != 2 {
		t.Fatalf("healthy tool events = %d want 2", len(response.ToolEvents))
	}
	// Enforced, not advisory: the policy never speaks to the model.
	mu.Lock()
	captured := append([]gatewayRequest(nil), requests...)
	mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("provider calls = %d want 3", len(captured))
	}
	for _, request := range captured {
		for _, message := range request.Messages {
			assertNoEnforcementResidue(t, "provider request", message.Content)
			if strings.Contains(
				strings.ToLower(message.Content), "liveness",
			) {
				t.Fatalf("policy guidance reached the model: %q",
					message.Content)
			}
		}
	}
}

func assertNoEnforcementResidue(t *testing.T, where string, content string) {
	t.Helper()
	for _, marker := range []string{
		"exploration budget", "dispatch deferred",
		"enforced liveness", "change_strategy",
	} {
		if strings.Contains(strings.ToLower(content), marker) {
			t.Fatalf("healthy run left %q in the %s: %q",
				marker, where, content)
		}
	}
}

func boundEnforcements(
	enforcements []LivenessEnforcement,
	bound string,
) []LivenessEnforcement {
	result := make([]LivenessEnforcement, 0, len(enforcements))
	for _, enforcement := range enforcements {
		if enforcement.Bound == bound {
			result = append(result, enforcement)
		}
	}
	return result
}

func countRole(
	messages []protocol.Message,
	role protocol.MessageRole,
) int {
	total := 0
	for _, message := range messages {
		if message.Role == role {
			total++
		}
	}
	return total
}

type toolFrame struct {
	ID        string
	Name      string
	Arguments map[string]interface{}
}

func writeSSEToolBatch(writer http.ResponseWriter, calls []toolFrame) {
	writer.Header().Set("Content-Type", "text/event-stream")
	encoded := make([]interface{}, 0, len(calls))
	for index, call := range calls {
		rawArguments, _ := json.Marshal(call.Arguments)
		encoded = append(encoded, map[string]interface{}{
			"index": index, "id": call.ID, "type": "function",
			"function": map[string]interface{}{
				"name": call.Name, "arguments": string(rawArguments),
			},
		})
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]interface{}{"tool_calls": encoded},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7,
		},
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

// explorationExecManager spawns the REAL exec bridge twice, once under an alias
// the exploration classifier recognises. Tool names on the surface are
// "<alias>__<tool>", so "fetch__shell" is a genuine, really-dispatched tool
// that the runtime classifies as open-web exploration — no stand-in, no double.
func explorationExecManager(t *testing.T) *neotools.Manager {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for real exec dispatch: %v", err)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve runtime liveness test source")
	}
	execBridge := filepath.Clean(filepath.Join(
		filepath.Dir(thisFile), "..", "..", "..", "..",
		"tools", "exec", "exec.mjs",
	))
	entry := func(alias string) executortool.ServerEntry {
		return executortool.ServerEntry{
			Alias: alias, Transport: "stdio",
			Command: "node", Args: []string{execBridge},
			PackageDigest: "sha256:" + strings.Repeat("c", 64),
			Version:       "0.1.0",
			Tools: []executortool.ToolEntry{
				{
					Name:            "shell",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_start",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_list",
					SideEffectClass: executortool.SideEffectRead,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_logs",
					SideEffectClass: executortool.SideEffectRead,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_stop",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_restart",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
			},
		}
	}
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	raw, err := json.Marshal(executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/resurrection-liveness-test",
		Servers: []executortool.ServerEntry{
			entry("exec"), entry("fetch"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(
		t.Context(),
		neotools.Options{
			ManifestPath: manifestPath,
			SpawnTimeout: 20 * time.Second,
			StderrSink:   io.Discard,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if warnings := manager.Warnings(); len(warnings) != 0 {
		_ = manager.Close()
		t.Fatalf("real exec bridge warnings: %v", warnings)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}
