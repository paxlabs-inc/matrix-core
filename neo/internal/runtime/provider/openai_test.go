// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package provider

import (
	"encoding/json"
	"testing"

	"matrix/neo/internal/runtime/protocol"
)

func TestOpenAIAdapterRoundTrip(t *testing.T) {
	t.Parallel()
	adapter := OpenAIAdapter{}
	request := providerRequest(true)
	request.Messages = append(request.Messages, protocol.Message{
		Role:      protocol.RoleAssistant,
		Content:   "Calling.",
		Reasoning: "Need a tool.",
		ToolCalls: []protocol.NormalizedToolCall{{
			ID:        "call-1",
			Name:      "weather",
			Arguments: json.RawMessage(`{"city":"Berlin"}`),
		}},
	})
	rawRequest, err := adapter.TranslateRequest(request)
	if err != nil {
		t.Fatalf("TranslateRequest() error = %v", err)
	}
	var encoded struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []struct {
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rawRequest, &encoded); err != nil {
		t.Fatalf("Unmarshal(request) error = %v", err)
	}
	if encoded.Model != "mimo-v2.5-pro" || !encoded.Stream ||
		!encoded.StreamOptions.IncludeUsage ||
		encoded.Messages[1].Reasoning != "Need a tool." ||
		len(encoded.Messages[1].ToolCalls) != 1 ||
		encoded.Messages[1].ToolCalls[0].Function.Name != "weather" ||
		encoded.Messages[1].ToolCalls[0].Function.Arguments != `{"city":"Berlin"}` {
		t.Fatalf("translated request = %+v", encoded)
	}

	rawResponse := json.RawMessage(`{
		"model":"mimo-v2.5-pro",
		"choices":[{
			"message":{
				"content":"Checking.",
				"reasoning_content":"Need a tool.",
				"tool_calls":[{
					"id":"call-1",
					"type":"function",
					"function":{"name":"weather","arguments":"{\"city\":\"Berlin\"}"}
				}]
			},
			"finish_reason":"tool_calls"
		}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`)
	generation, err := adapter.TranslateResponse(rawResponse)
	if err != nil {
		t.Fatalf("TranslateResponse() error = %v", err)
	}
	if generation.Content != "Checking." || generation.Reasoning != "Need a tool." ||
		generation.FinishReason != protocol.FinishToolCalls ||
		len(generation.ToolCalls) != 1 ||
		string(generation.ToolCalls[0].Arguments) != `{"city":"Berlin"}` ||
		generation.Usage.TotalTokens != 15 {
		t.Fatalf("generation = %+v", generation)
	}
}

func TestOpenAIAdapterRejectsInvalidWireData(t *testing.T) {
	t.Parallel()
	adapter := OpenAIAdapter{}
	request := providerRequest(false)
	request.Model = ""
	if _, err := adapter.TranslateRequest(request); err == nil {
		t.Fatal("invalid request accepted")
	}
	for _, raw := range []string{
		`{`,
		`{"choices":[]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"call","function":{"name":"x","arguments":"[]"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{},"finish_reason":"stop"}]} {}`,
	} {
		if _, err := adapter.TranslateResponse(json.RawMessage(raw)); err == nil {
			t.Fatalf("TranslateResponse(%q) succeeded", raw)
		}
	}
	if _, err := adapter.TranslateStreamEvent([]byte(`data: {`)); err == nil {
		t.Fatal("invalid stream event accepted")
	}
}

func providerRequest(stream bool) protocol.GenerationRequest {
	return protocol.GenerationRequest{
		Model: "mimo-v2.5-pro",
		Messages: []protocol.Message{{
			Role:    protocol.RoleUser,
			Content: "What is the weather?",
		}},
		Tools: []protocol.ToolDefinition{{
			Name:        "weather",
			Description: "Get weather.",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		MaxOutputTokens: 256,
		Stream:          stream,
	}
}
