// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"matrix/neo/internal/runtime/protocol"
)

const (
	mimoToolOpen       = "<tool_call>"
	mimoToolClose      = "</tool_call>"
	mimoFunctionOpen   = "<function="
	mimoFunctionClose  = "</function>"
	mimoParameterOpen  = "<parameter="
	mimoParameterClose = "</parameter>"
)

type MiMoAdapter struct {
	textualRecoveries atomic.Uint64
}

func (*MiMoAdapter) Name() string {
	return "mimo"
}

func (*MiMoAdapter) TranslateRequest(request protocol.GenerationRequest) (json.RawMessage, error) {
	raw, err := (OpenAIAdapter{}).TranslateRequest(request)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("resurrection provider mimo: decode compatible request: %w", err)
	}
	payload["temperature"] = 0.3
	payload["top_p"] = 0.95
	if messages, ok := payload["messages"].([]any); ok {
		for index, message := range request.Messages {
			if index >= len(messages) || message.Role != protocol.RoleAssistant {
				continue
			}
			if wire, ok := messages[index].(map[string]any); ok {
				wire["reasoning_content"] = message.Reasoning
			}
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("resurrection provider mimo: encode request: %w", err)
	}
	return encoded, nil
}

func (adapter *MiMoAdapter) TranslateResponse(raw json.RawMessage) (protocol.NormalizedGeneration, error) {
	generation, err := (OpenAIAdapter{}).TranslateResponse(raw)
	if err != nil {
		return protocol.NormalizedGeneration{}, err
	}
	return adapter.FinalizeGeneration(generation)
}

func (*MiMoAdapter) TranslateStreamEvent(event []byte) (protocol.StreamChunk, error) {
	return (OpenAIAdapter{}).TranslateStreamEvent(event)
}

func (adapter *MiMoAdapter) FinalizeGeneration(
	generation protocol.NormalizedGeneration,
) (protocol.NormalizedGeneration, error) {
	content, contentCalls, contentMarkup, err := ParseMiMoToolCalls(generation.Content)
	if err != nil {
		return protocol.NormalizedGeneration{}, err
	}
	reasoning, reasoningCalls, reasoningMarkup, err := ParseMiMoToolCalls(generation.Reasoning)
	if err != nil {
		return protocol.NormalizedGeneration{}, err
	}
	recovered := append(contentCalls, reasoningCalls...)
	if contentMarkup || reasoningMarkup {
		adapter.textualRecoveries.Add(1)
		generation.Content = content
		generation.Reasoning = reasoning
	}
	if len(recovered) == 0 {
		return generation, nil
	}
	if len(generation.ToolCalls) == 0 {
		generation.ToolCalls = recovered
	} else if !sameToolCalls(generation.ToolCalls, recovered) {
		return protocol.NormalizedGeneration{}, fmt.Errorf(
			"%w: native and textual MiMo calls disagree", ErrToolProtocol,
		)
	}
	generation.FinishReason = protocol.FinishToolCalls
	return generation, nil
}

func (adapter *MiMoAdapter) TextualRecoveries() uint64 {
	return adapter.textualRecoveries.Load()
}

func ParseMiMoToolCalls(
	text string,
) (string, []protocol.NormalizedToolCall, bool, error) {
	hasMarkup := strings.Contains(text, mimoToolOpen) ||
		strings.Contains(text, mimoToolClose) ||
		strings.Contains(text, mimoFunctionOpen)
	if !hasMarkup {
		return text, nil, false, nil
	}
	if !strings.Contains(text, mimoFunctionOpen) {
		return "", nil, true, fmt.Errorf("%w: MiMo function tag is missing", ErrToolProtocol)
	}
	var cleaned strings.Builder
	var calls []protocol.NormalizedToolCall
	remainder := text
	for {
		functionAt := strings.Index(remainder, mimoFunctionOpen)
		if functionAt < 0 {
			if strings.Contains(remainder, mimoToolOpen) || strings.Contains(remainder, mimoToolClose) {
				return "", nil, true, fmt.Errorf("%w: unmatched MiMo tool tag", ErrToolProtocol)
			}
			cleaned.WriteString(remainder)
			break
		}
		start := functionAt
		if toolAt := strings.LastIndex(remainder[:functionAt], mimoToolOpen); toolAt >= 0 &&
			strings.TrimSpace(remainder[toolAt+len(mimoToolOpen):functionAt]) == "" {
			start = toolAt
		}
		cleaned.WriteString(remainder[:start])
		afterFunction := remainder[functionAt+len(mimoFunctionOpen):]
		headerEnd := strings.IndexByte(afterFunction, '>')
		if headerEnd < 0 {
			return "", nil, true, fmt.Errorf("%w: truncated MiMo function tag", ErrToolProtocol)
		}
		name := strings.TrimSpace(strings.TrimSuffix(afterFunction[:headerEnd], "/"))
		if !validMiMoName(name) {
			return "", nil, true, fmt.Errorf("%w: invalid MiMo function name", ErrToolProtocol)
		}
		body := afterFunction[headerEnd+1:]
		bodyEnd := len(body)
		functionEnd := strings.Index(body, mimoFunctionClose)
		toolEnd := strings.Index(body, mimoToolClose)
		if functionEnd >= 0 {
			bodyEnd = functionEnd
		}
		if toolEnd >= 0 && toolEnd < bodyEnd {
			bodyEnd = toolEnd
		}
		arguments, err := parseMiMoParameters(body[:bodyEnd])
		if err != nil {
			return "", nil, true, err
		}
		consumed := len(body)
		switch {
		case toolEnd >= 0:
			consumed = toolEnd + len(mimoToolClose)
		case functionEnd >= 0:
			consumed = functionEnd + len(mimoFunctionClose)
		}
		rawEnd := functionAt + len(mimoFunctionOpen) + headerEnd + 1 + consumed
		if rawEnd > len(remainder) {
			rawEnd = len(remainder)
		}
		raw := remainder[start:rawEnd]
		digest := sha256.Sum256([]byte(raw))
		calls = append(calls, protocol.NormalizedToolCall{
			ID:        fmt.Sprintf("mimo-%x", digest[:12]),
			Name:      name,
			Arguments: arguments,
		})
		remainder = body[consumed:]
		if remainder == "" {
			break
		}
	}
	return strings.TrimSpace(cleaned.String()), calls, true, nil
}

func parseMiMoParameters(body string) (json.RawMessage, error) {
	parameters := map[string]any{}
	remainder := body
	for {
		parameterAt := strings.Index(remainder, mimoParameterOpen)
		if parameterAt < 0 {
			break
		}
		afterParameter := remainder[parameterAt+len(mimoParameterOpen):]
		headerEnd := strings.IndexByte(afterParameter, '>')
		if headerEnd < 0 {
			return nil, fmt.Errorf("%w: truncated MiMo parameter tag", ErrToolProtocol)
		}
		name := strings.TrimSpace(strings.TrimSuffix(afterParameter[:headerEnd], "/"))
		if !validMiMoName(name) {
			return nil, fmt.Errorf("%w: invalid MiMo parameter name", ErrToolProtocol)
		}
		if _, duplicate := parameters[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate MiMo parameter", ErrToolProtocol)
		}
		valueAndRest := afterParameter[headerEnd+1:]
		valueEnd := len(valueAndRest)
		for _, marker := range []string{
			mimoParameterClose,
			mimoParameterOpen,
			mimoFunctionClose,
			mimoToolClose,
		} {
			if markerAt := strings.Index(valueAndRest, marker); markerAt >= 0 && markerAt < valueEnd {
				valueEnd = markerAt
			}
		}
		parameters[name] = mimoParameterValue(strings.TrimSpace(valueAndRest[:valueEnd]))
		remainder = valueAndRest[valueEnd:]
	}
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return nil, fmt.Errorf("%w: encode MiMo parameters", ErrToolProtocol)
	}
	return encoded, nil
}

func mimoParameterValue(raw string) any {
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		if _, isString := value.(string); !isString {
			return value
		}
	}
	return raw
}

func validMiMoName(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, current := range value {
		if current >= 'a' && current <= 'z' ||
			current >= 'A' && current <= 'Z' ||
			current >= '0' && current <= '9' ||
			current == '_' || current == '-' || current == '.' {
			continue
		}
		return false
	}
	return true
}

func sameToolCalls(left, right []protocol.NormalizedToolCall) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name != right[index].Name ||
			!sameJSONValue(left[index].Arguments, right[index].Arguments) {
			return false
		}
	}
	return true
}

func sameJSONValue(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

type MiMoCapability struct {
	Checked   bool      `json:"checked"`
	Mode      string    `json:"mode"`
	Native    bool      `json:"native_tool_calls"`
	MultiTurn bool      `json:"multi_turn"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Strategy  int       `json:"strategy"`
}

type MiMoGenerator struct {
	inner   *Client
	adapter *MiMoAdapter

	mu         sync.Mutex
	probing    bool
	probeDone  chan struct{}
	capability MiMoCapability
}

const noToolCompatibilityPrompt = "This request exposes no tools. Return only ordinary prose that directly satisfies the latest instruction. Do not call, name, describe, or serialize any tool, function, parameter, XML tag, JSON tool object, or tool-call markup."

func NewMiMoGenerator(inner *Client, adapter *MiMoAdapter) (*MiMoGenerator, error) {
	if inner == nil || adapter == nil {
		return nil, fmt.Errorf("resurrection provider mimo: client and adapter are required")
	}
	return &MiMoGenerator{inner: inner, adapter: adapter}, nil
}

func (generator *MiMoGenerator) Generate(
	ctx context.Context,
	request protocol.GenerationRequest,
	turnUsage *TurnUsage,
) (protocol.NormalizedGeneration, error) {
	hasTools := len(request.Tools) > 0
	request.Stream = true
	if hasTools {
		generator.ensureCapability(ctx, request.Model)
		request = generator.applyStrategy(request)
	}
	generation, err := generator.inner.Generate(ctx, request, turnUsage)
	if err != nil {
		return protocol.NormalizedGeneration{}, err
	}
	if hasTools || len(generation.ToolCalls) == 0 {
		return generation, nil
	}

	retry := request
	retry.Messages = append([]protocol.Message{{
		Role: protocol.RoleSystem, Content: noToolCompatibilityPrompt,
	}}, request.Messages...)
	generation, err = generator.inner.Generate(ctx, retry, turnUsage)
	if err != nil {
		return protocol.NormalizedGeneration{}, err
	}
	if len(generation.ToolCalls) > 0 {
		return protocol.NormalizedGeneration{}, fmt.Errorf(
			"%w: MiMo emitted a tool call while the active request exposed no tools after one compatibility retry",
			ErrToolProtocol,
		)
	}
	return generation, nil
}

func (generator *MiMoGenerator) GenerateStream(
	ctx context.Context,
	request protocol.GenerationRequest,
	turnUsage *TurnUsage,
	deliver func(protocol.StreamChunk) error,
) (protocol.NormalizedGeneration, error) {
	hasTools := len(request.Tools) > 0
	request.Stream = true
	if hasTools {
		generator.ensureCapability(ctx, request.Model)
		request = generator.applyStrategy(request)
		return generator.inner.GenerateStream(ctx, request, turnUsage, deliver)
	}

	// A tools-stripped request is a final-answer boundary. Buffer it until the
	// MiMo adapter proves that the response is ordinary prose; otherwise an
	// invalid textual call would be streamed to the user before the request-local
	// compatibility retry could correct it.
	var buffered []protocol.StreamChunk
	buffer := func(chunk protocol.StreamChunk) error {
		buffered = append(buffered, chunk)
		return nil
	}
	generation, err := generator.inner.GenerateStream(ctx, request, turnUsage, buffer)
	if err != nil {
		return protocol.NormalizedGeneration{}, err
	}
	if len(generation.ToolCalls) > 0 {
		retry := request
		retry.Messages = append([]protocol.Message{{
			Role: protocol.RoleSystem, Content: noToolCompatibilityPrompt,
		}}, request.Messages...)
		buffered = nil
		generation, err = generator.inner.GenerateStream(ctx, retry, turnUsage, buffer)
		if err != nil {
			return protocol.NormalizedGeneration{}, err
		}
		if len(generation.ToolCalls) > 0 {
			return protocol.NormalizedGeneration{}, fmt.Errorf(
				"%w: MiMo emitted a tool call while the active streaming request exposed no tools after one compatibility retry",
				ErrToolProtocol,
			)
		}
	}
	for _, chunk := range buffered {
		if err := deliver(chunk); err != nil {
			return protocol.NormalizedGeneration{}, err
		}
	}
	return generation, nil
}

func (generator *MiMoGenerator) CapabilityStatus() MiMoCapability {
	generator.mu.Lock()
	defer generator.mu.Unlock()
	return generator.capability
}

func (generator *MiMoGenerator) AdvanceGenerationStrategy(reason string) bool {
	generator.mu.Lock()
	defer generator.mu.Unlock()
	if generator.capability.Strategy >= 2 {
		return false
	}
	generator.capability.Strategy++
	generator.capability.Mode = "compatibility"
	generator.capability.Reason = strings.TrimSpace(reason)
	return true
}

func (generator *MiMoGenerator) applyStrategy(
	request protocol.GenerationRequest,
) protocol.GenerationRequest {
	generator.mu.Lock()
	strategy := generator.capability.Strategy
	mode := generator.capability.Mode
	generator.mu.Unlock()
	if strategy == 0 && mode != "compatibility" {
		return request
	}
	instruction := "MiMo tool compatibility strategy 1 is active. Emit exactly one complete function call when an action is needed and emit no prose after the call."
	if strategy >= 2 {
		instruction = "MiMo tool compatibility strategy 2 is active. Emit one minimal function call using only the supplied schema fields. Do not emit commentary or duplicate textual and native calls."
	}
	request.Messages = append([]protocol.Message{{
		Role: protocol.RoleSystem, Content: instruction,
	}}, request.Messages...)
	return request
}

func (generator *MiMoGenerator) ensureCapability(ctx context.Context, model string) {
	generator.mu.Lock()
	if generator.capability.Checked {
		generator.mu.Unlock()
		return
	}
	if generator.probing {
		done := generator.probeDone
		generator.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	generator.probing = true
	generator.probeDone = make(chan struct{})
	done := generator.probeDone
	generator.mu.Unlock()

	capability := generator.runCapabilityProbe(ctx, model)
	generator.mu.Lock()
	generator.capability = capability
	generator.probing = false
	close(done)
	generator.mu.Unlock()
}

func (generator *MiMoGenerator) runCapabilityProbe(
	ctx context.Context,
	model string,
) MiMoCapability {
	result := MiMoCapability{
		Checked: true, Mode: "compatibility", CheckedAt: time.Now().UTC(),
	}
	definition := protocol.ToolDefinition{
		Name:        "matrix_runtime_capability_echo",
		Description: "Harmless provider tool-call capability probe with no external effect.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["value","expect"],"properties":{"value":{"type":"string"},"expect":{"type":"string"}}}`),
	}
	before := generator.adapter.TextualRecoveries()
	first, err := generator.inner.Generate(ctx, protocol.GenerationRequest{
		Model: model,
		Messages: []protocol.Message{{
			Role:    protocol.RoleUser,
			Content: "Call matrix_runtime_capability_echo exactly once with value READY and a short expect prediction.",
		}},
		Tools:  []protocol.ToolDefinition{definition},
		Stream: true,
	}, nil)
	if err != nil || len(first.ToolCalls) != 1 ||
		first.ToolCalls[0].Name != definition.Name {
		result.Reason = "tool canary did not produce one recoverable call"
		return result
	}
	result.Native = generator.adapter.TextualRecoveries() == before
	if result.Native {
		result.Mode = "native-buffered"
	} else {
		result.Mode = "textual-buffered"
	}
	second, err := generator.inner.Generate(ctx, protocol.GenerationRequest{
		Model: model,
		Messages: []protocol.Message{
			{
				Role:    protocol.RoleUser,
				Content: "Call matrix_runtime_capability_echo exactly once with value READY and a short expect prediction.",
			},
			{
				Role: protocol.RoleAssistant, Content: first.Content,
				Reasoning: first.Reasoning, ToolCalls: first.ToolCalls,
			},
			{
				Role: protocol.RoleTool, Name: definition.Name,
				ToolCallID: first.ToolCalls[0].ID, Content: `{"value":"READY"}`,
			},
			{
				Role:    protocol.RoleUser,
				Content: "Reply with READY as ordinary text and do not call a tool.",
			},
		},
		Tools:  []protocol.ToolDefinition{definition},
		Stream: true,
	}, nil)
	if err != nil || len(second.ToolCalls) != 0 ||
		!strings.Contains(strings.ToUpper(second.Content), "READY") {
		result.Mode = "compatibility"
		result.Reason = "multi-turn continuation canary failed"
		result.Strategy = 1
		return result
	}
	result.MultiTurn = true
	return result
}
