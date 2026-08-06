// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/neo/internal/runtime/protocol"
)

func TestMiMoAdapterNormalizesBothChannelsAndRejectsDisagreement(t *testing.T) {
	adapter := &MiMoAdapter{}
	raw, err := adapter.TranslateRequest(protocol.GenerationRequest{
		Model: "mimo-v2.5-pro",
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: "write"},
			{Role: protocol.RoleAssistant, Content: "working", Reasoning: ""},
		},
		Tools: []protocol.ToolDefinition{{
			Name: "filesystem_write", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
		Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Temperature float64 `json:"temperature"`
		TopP        float64 `json:"top_p"`
		Messages    []map[string]any
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Temperature != 0.3 || request.TopP != 0.95 {
		t.Fatalf("sampling = %+v", request)
	}
	if reasoning, present := request.Messages[1]["reasoning_content"]; !present || reasoning != "" {
		t.Fatalf("empty assistant reasoning_content = %#v, present=%t", reasoning, present)
	}

	generation, err := adapter.FinalizeGeneration(protocol.NormalizedGeneration{
		Content: "I will write it.\n<tool_call><function=filesystem_write>" +
			"<parameter=path>src/app.go</parameter></function></tool_call>",
		Reasoning:    "<tool_call><function=filesystem_list><parameter=path>.</parameter></function></tool_call>",
		FinishReason: protocol.FinishStop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if generation.Content != "I will write it." || generation.Reasoning != "" ||
		len(generation.ToolCalls) != 2 ||
		generation.FinishReason != protocol.FinishToolCalls {
		t.Fatalf("generation = %+v", generation)
	}

	matching, err := adapter.FinalizeGeneration(protocol.NormalizedGeneration{
		Content: `<tool_call><function=echo><parameter=a>1</parameter><parameter=b>true</parameter></function></tool_call>`,
		ToolCalls: []protocol.NormalizedToolCall{{
			ID: "native", Name: "echo", Arguments: json.RawMessage(`{"b":true,"a":1}`),
		}},
		FinishReason: protocol.FinishToolCalls,
	})
	if err != nil || len(matching.ToolCalls) != 1 || matching.ToolCalls[0].ID != "native" {
		t.Fatalf("matching generation = %+v, error = %v", matching, err)
	}
	_, err = adapter.FinalizeGeneration(protocol.NormalizedGeneration{
		Content: `<tool_call><function=echo><parameter=a>2</parameter></function></tool_call>`,
		ToolCalls: []protocol.NormalizedToolCall{{
			ID: "native", Name: "echo", Arguments: json.RawMessage(`{"a":1}`),
		}},
		FinishReason: protocol.FinishToolCalls,
	})
	if !errors.Is(err, ErrToolProtocol) {
		t.Fatalf("disagreement error = %v", err)
	}
}

func TestParseMiMoToolCallsHandlesRecordedTruncationWithoutMarkupLeak(t *testing.T) {
	content := "Working. <tool_call><function=exec__shell><parameter=command>echo hi"
	cleaned, calls, markup, err := ParseMiMoToolCalls(content)
	if err != nil {
		t.Fatal(err)
	}
	if !markup || cleaned != "Working." || len(calls) != 1 ||
		calls[0].Name != "exec__shell" ||
		string(calls[0].Arguments) != `{"command":"echo hi"}` {
		t.Fatalf("cleaned=%q calls=%+v markup=%t", cleaned, calls, markup)
	}
}

func TestMiMoGeneratorPreflightsAndForwardsActualSSE(t *testing.T) {
	simulator := newMiMoSimulator()
	server := httptest.NewServer(simulator)
	defer server.Close()
	generator := newTestMiMoGenerator(t, server)

	var delivered []protocol.StreamChunk
	result, err := generator.GenerateStream(
		context.Background(),
		mimoTask("tag-content", true),
		nil,
		func(chunk protocol.StreamChunk) error {
			delivered = append(delivered, chunk)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "Checking." || len(result.ToolCalls) != 1 ||
		result.ToolCalls[0].Name != "web_search" {
		t.Fatalf("result = %+v", result)
	}
	if len(delivered) != 1 || delivered[0].ContentDelta != "Checking.<tool_call><function=web_search><parameter=query>Matrix</parameter></function></tool_call>" {
		t.Fatalf("delivered = %+v", delivered)
	}
	status := generator.CapabilityStatus()
	if !status.Checked || status.Mode != "textual-buffered" ||
		status.Native || !status.MultiTurn {
		t.Fatalf("capability = %+v", status)
	}
	if simulator.capabilityCalls() != 2 {
		t.Fatalf("capability calls = %d", simulator.capabilityCalls())
	}
	truncated, err := generator.Generate(context.Background(), mimoTask("truncated", true), nil)
	if err != nil || truncated.Content != "Working." ||
		len(truncated.ToolCalls) != 1 ||
		truncated.ToolCalls[0].Name != "exec__shell" {
		t.Fatalf("truncated = %+v, error = %v", truncated, err)
	}
}

func TestMiMoGeneratorCrossValidatesDualEmissionThroughBoundary(t *testing.T) {
	simulator := newMiMoSimulator()
	server := httptest.NewServer(simulator)
	defer server.Close()
	generator := newTestMiMoGenerator(t, server)

	matching, err := generator.Generate(context.Background(), mimoTask("dual-match", true), nil)
	if err != nil || len(matching.ToolCalls) != 1 || matching.ToolCalls[0].Name != "echo" {
		t.Fatalf("matching = %+v, error = %v", matching, err)
	}
	_, err = generator.Generate(context.Background(), mimoTask("dual-disagree", true), nil)
	if !errors.Is(err, ErrToolProtocol) || !IsFailureKind(err, FailureProtocol) {
		t.Fatalf("disagreement error = %T %v", err, err)
	}
}

func TestMiMoNoToolsCompatibilityRetryIsRequestLocal(t *testing.T) {
	simulator := newMiMoSimulator()
	server := httptest.NewServer(simulator)
	defer server.Close()
	generator := newTestMiMoGenerator(t, server)

	first, err := generator.Generate(context.Background(), mimoTask("no-tools", false), nil)
	if err != nil || first.Content != "Revised without tools." ||
		len(first.ToolCalls) != 0 {
		t.Fatalf("first = %+v, error = %v", first, err)
	}
	second, err := generator.Generate(context.Background(), mimoTask("plain-no-tools", false), nil)
	if err != nil || second.Content != "Plain answer." {
		t.Fatalf("second = %+v, error = %v", second, err)
	}
	if simulator.compatibilityPrompts() != 1 {
		t.Fatalf("compatibility prompts = %d", simulator.compatibilityPrompts())
	}
}

func TestMiMoStreamingNoToolsBuffersInvalidAttemptAndDeliversCompatibilityRetry(t *testing.T) {
	simulator := newMiMoSimulator()
	server := httptest.NewServer(simulator)
	defer server.Close()
	generator := newTestMiMoGenerator(t, server)

	var delivered []protocol.StreamChunk
	result, err := generator.GenerateStream(
		context.Background(), mimoTask("no-tools", false), nil,
		func(chunk protocol.StreamChunk) error {
			delivered = append(delivered, chunk)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "Revised without tools." || len(result.ToolCalls) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(delivered) != 1 || delivered[0].ContentDelta != "Revised without tools." {
		t.Fatalf("invalid first attempt escaped or retry was not delivered: %+v", delivered)
	}
	if simulator.compatibilityPrompts() != 1 {
		t.Fatalf("compatibility prompts = %d", simulator.compatibilityPrompts())
	}
}

func TestMiMoStrategyLadderChangesRequestAndExhausts(t *testing.T) {
	simulator := newMiMoSimulator()
	server := httptest.NewServer(simulator)
	defer server.Close()
	generator := newTestMiMoGenerator(t, server)
	if _, err := generator.Generate(context.Background(), mimoTask("plain-tool", true), nil); err != nil {
		t.Fatal(err)
	}
	base := generator.applyStrategy(mimoTask("plain-tool", true))
	if !generator.AdvanceGenerationStrategy("first") {
		t.Fatal("first strategy advance failed")
	}
	first := generator.applyStrategy(mimoTask("plain-tool", true))
	if !generator.AdvanceGenerationStrategy("second") {
		t.Fatal("second strategy advance failed")
	}
	second := generator.applyStrategy(mimoTask("plain-tool", true))
	if generator.AdvanceGenerationStrategy("third") {
		t.Fatal("strategy ladder did not exhaust after three distinct request shapes")
	}
	if len(base.Messages) != 1 || len(first.Messages) != 2 ||
		len(second.Messages) != 2 ||
		!strings.Contains(first.Messages[0].Content, "strategy 1") ||
		!strings.Contains(second.Messages[0].Content, "strategy 2") ||
		first.Messages[0].Content == second.Messages[0].Content {
		t.Fatalf("strategy requests base=%+v first=%+v second=%+v",
			base.Messages, first.Messages, second.Messages)
	}
}

func TestMiMoTransientPathologiesUnderConcurrentLoad(t *testing.T) {
	simulator := newMiMoSimulator()
	server := httptest.NewServer(simulator)
	defer server.Close()
	generator := newTestMiMoGenerator(t, server)

	const workers = 12
	var usage TurnUsage
	var group sync.WaitGroup
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := mimoTask(fmt.Sprintf("transient-%d", index), true)
			generation, err := generator.Generate(context.Background(), request, &usage)
			if err == nil && generation.Content != "Recovered." {
				err = fmt.Errorf("content = %q", generation.Content)
			}
			errs <- err
		}(index)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot := usage.Snapshot()
	if snapshot.Calls != workers || snapshot.Usage.TotalTokens != workers*3 {
		t.Fatalf("usage = %+v", snapshot)
	}
	if simulator.capabilityCalls() != 2 {
		t.Fatalf("capability canary ran %d calls under concurrency", simulator.capabilityCalls())
	}
}

func newTestMiMoGenerator(t *testing.T, server *httptest.Server) *MiMoGenerator {
	t.Helper()
	t.Setenv("TEST_GATEWAY_TOKEN", "gateway-secret")
	adapter := &MiMoAdapter{}
	client, err := New(adapter, Config{
		GatewayURL:     server.URL,
		BearerEnv:      "TEST_GATEWAY_TOKEN",
		ActorDID:       "did:matrix:test",
		SlotLabel:      "neo",
		MaxAttempts:    3,
		BackoffInitial: time.Millisecond,
		BackoffMax:     time.Millisecond,
		IdleTimeout:    time.Second,
		HTTPClient:     server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewMiMoGenerator(client, adapter)
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

func mimoTask(name string, tools bool) protocol.GenerationRequest {
	request := protocol.GenerationRequest{
		Model: "mimo-v2.5-pro",
		Messages: []protocol.Message{{
			Role: protocol.RoleUser, Content: name,
		}},
		Stream: true,
	}
	if tools {
		request.Tools = []protocol.ToolDefinition{{
			Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`),
		}}
	}
	return request
}

type miMoSimulator struct {
	mu            sync.Mutex
	capability    int
	compatibility int
	transient     map[string]int
}

func newMiMoSimulator() *miMoSimulator {
	return &miMoSimulator{transient: map[string]int{}}
}

func (simulator *miMoSimulator) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var body struct {
		Messages []protocol.Message `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if !body.Stream {
		http.Error(writer, "MiMo boundary must use buffered SSE", http.StatusBadRequest)
		return
	}
	lastUser := ""
	hasCompatibilityPrompt := false
	for _, message := range body.Messages {
		if message.Role == protocol.RoleUser {
			lastUser = message.Content
		}
		if message.Content == noToolCompatibilityPrompt {
			hasCompatibilityPrompt = true
		}
	}
	for _, tool := range body.Tools {
		if tool.Function.Name == "matrix_runtime_capability_echo" {
			simulator.mu.Lock()
			simulator.capability++
			simulator.mu.Unlock()
			if strings.Contains(lastUser, "Reply with READY") {
				writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"READY"},"finish_reason":"stop"}]}`, 1, 1)
				return
			}
			writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"<tool_call><function=matrix_runtime_capability_echo><parameter=value>READY</parameter><parameter=expect>echo succeeds</parameter></function></tool_call>"},"finish_reason":"stop"}]}`, 1, 1)
			return
		}
	}

	switch {
	case strings.HasPrefix(lastUser, "transient-"):
		simulator.mu.Lock()
		simulator.transient[lastUser]++
		attempt := simulator.transient[lastUser]
		simulator.mu.Unlock()
		if attempt == 1 {
			http.Error(writer, "rate limited", http.StatusTooManyRequests)
			return
		}
		if attempt == 2 {
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"Recovered."},"finish_reason":"stop"}]}`, 2, 1)
	case lastUser == "tag-content":
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"Checking.<tool_call><function=web_search><parameter=query>Matrix</parameter></function></tool_call>"},"finish_reason":"stop"}]}`, 2, 1)
	case lastUser == "dual-match":
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"<tool_call><function=echo><parameter=value>ok</parameter></function></tool_call>","tool_calls":[{"index":0,"id":"native","function":{"name":"echo","arguments":"{\"value\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]}`, 2, 1)
	case lastUser == "dual-disagree":
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"<tool_call><function=echo><parameter=value>textual</parameter></function></tool_call>","tool_calls":[{"index":0,"id":"native","function":{"name":"echo","arguments":"{\"value\":\"native\"}"}}]},"finish_reason":"tool_calls"}]}`, 2, 1)
	case lastUser == "truncated":
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"Working. <tool_call><function=exec__shell><parameter=command>echo hi"},"finish_reason":"stop"}]}`, 2, 1)
	case lastUser == "no-tools" && !hasCompatibilityPrompt:
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"<tool_call><function=echo><parameter=value>bad</parameter></function></tool_call>"},"finish_reason":"stop"}]}`, 1, 1)
	case lastUser == "no-tools" && hasCompatibilityPrompt:
		simulator.mu.Lock()
		simulator.compatibility++
		simulator.mu.Unlock()
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"Revised without tools."},"finish_reason":"stop"}]}`, 1, 1)
	case lastUser == "plain-no-tools":
		if hasCompatibilityPrompt {
			http.Error(writer, "compatibility prompt leaked across requests", http.StatusBadRequest)
			return
		}
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"Plain answer."},"finish_reason":"stop"}]}`, 1, 1)
	default:
		writeMiMoSSE(writer, `{"choices":[{"delta":{"content":"Plain."},"finish_reason":"stop"}]}`, 1, 1)
	}
}

func (simulator *miMoSimulator) capabilityCalls() int {
	simulator.mu.Lock()
	defer simulator.mu.Unlock()
	return simulator.capability
}

func (simulator *miMoSimulator) compatibilityPrompts() int {
	simulator.mu.Lock()
	defer simulator.mu.Unlock()
	return simulator.compatibility
}

func writeMiMoSSE(writer http.ResponseWriter, choice string, prompt, completion int) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", choice)
	_, _ = fmt.Fprintf(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", prompt, completion, prompt+completion)
	_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
}
