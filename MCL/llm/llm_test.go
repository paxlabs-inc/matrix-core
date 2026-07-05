// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"matrix/mcl/mtx/interpreter"
)

func TestDetectProvider(t *testing.T) {
	tests := []struct {
		model string
		want  Provider
		err   bool
	}{
		{"accounts/fireworks/models/deepseek-v4-flash", ProviderFireworks, false},
		{"accounts/fireworks/models/deepseek-v4-pro", ProviderFireworks, false},
		// "grok-*" ids (the v9 xAI migration off Z.ai GLM) resolve to xAI's
		// OpenAI-compatible API — the primary chat provider. The check precedes
		// the opencode bare-id detection so grok-build-* lands on xAI (NOT
		// opencode). The remaining bare "<vendor>/<model>" shape (including the
		// retired zai-org/GLM ids) resolves to Baseten; Together is only
		// reachable via explicit Config.Provider + ProviderSet.
		{"grok-4.3", ProviderXai, false},
		{"grok-build-0.1", ProviderXai, false},
		{"grok-4.20-0309-non-reasoning", ProviderXai, false},
		{"zai-org/GLM-5.2", ProviderBaseten, false},
		{"deepseek-ai/DeepSeek-V4-Flash", ProviderBaseten, false},
		{"openai/gpt-oss-120b", ProviderBaseten, false},
		{"Qwen/Qwen3.5-9B-FP8", ProviderBaseten, false},
		{"no-slash-model", 0, true},
	}

	for _, tt := range tests {
		got, err := DetectProvider(tt.model)
		if tt.err {
			if err == nil {
				t.Errorf("DetectProvider(%q) = %v, want error", tt.model, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("DetectProvider(%q) error: %v", tt.model, err)
			continue
		}
		if got != tt.want {
			t.Errorf("DetectProvider(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestIsXaiModel(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"grok-4.3", true},
		{"grok-build-0.1", true},
		{"grok-4.20-0309-non-reasoning", true},
		{"GROK-4.3", true}, // case-insensitive prefix
		{"zai-org/GLM-5.2", false},
		{"deepseek-ai/DeepSeek-V4-Flash", false},
		{"accounts/fireworks/models/gpt-oss-120b", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsXaiModel(tt.in); got != tt.want {
			t.Errorf("IsXaiModel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestXaiModelPassesThroughUnchanged is the successor to the retired
// TestZaiNativeModel: the v9 migration off Z.ai GLM means there is NO
// send-time model rewrite — the bare "grok-*" fleet id IS xAI's native
// model code, so it must reach the wire verbatim (no native-id translation,
// no thinking-block rewrite).
func TestXaiModelPassesThroughUnchanged(t *testing.T) {
	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = req.Model
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer server.Close()

	client, err := New(&Config{
		Model:    "grok-4.3",
		APIKey:   "test-key",
		Endpoint: server.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Decode(context.Background(), []interpreter.Message{
		{Role: "user", Content: "hi"},
	}, ""); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if gotModel != "grok-4.3" {
		t.Errorf("upstream model = %q, want %q (grok id must pass through unchanged)", gotModel, "grok-4.3")
	}
}

// TestReasoningEffortWire pins the xAI reasoning_effort request contract that
// replaced the retired Z.ai `thinking` block. Only grok-4.3 carries the field:
// EnableThinking=true maps to "high", EnableThinking=false maps to "none", and
// models that do not support reasoning_effort (grok-build-*, the non-reasoning
// grok, and non-grok models) omit the field entirely (omitempty).
func TestReasoningEffortWire(t *testing.T) {
	tests := []struct {
		name           string
		model          string
		enableThinking bool
		wantEffort     string // "" means the field must be omitted from the wire
	}{
		{"grok43 thinking on -> high", "grok-4.3", true, "high"},
		{"grok43 thinking off -> none", "grok-4.3", false, "none"},
		{"grok-build omits", "grok-build-0.1", true, ""},
		{"grok non-reasoning omits", "grok-4.20-0309-non-reasoning", true, ""},
		{"non-grok omits", "deepseek-ai/DeepSeek-V4-Flash", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				json.NewEncoder(w).Encode(chatResponse{
					Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: "ok"}}},
				})
			}))
			defer server.Close()

			client, err := New(&Config{
				Model:          tt.model,
				APIKey:         "test-key",
				Endpoint:       server.URL,
				EnableThinking: tt.enableThinking,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := client.Decode(context.Background(), []interpreter.Message{
				{Role: "user", Content: "hi"},
			}, ""); err != nil {
				t.Fatalf("Decode: %v", err)
			}

			effort, present := raw["reasoning_effort"]
			if tt.wantEffort == "" {
				if present {
					t.Errorf("reasoning_effort present (%s), want omitted", effort)
				}
				return
			}
			if !present {
				t.Fatalf("reasoning_effort missing, want %q", tt.wantEffort)
			}
			if string(effort) != `"`+tt.wantEffort+`"` {
				t.Errorf("reasoning_effort = %s, want %q", effort, tt.wantEffort)
			}
		})
	}
}

func TestDecodeWithMockServer(t *testing.T) {
	// Mock OpenAI-compat server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request shape
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.Model != "test/model" {
			t.Errorf("model = %q, want %q", req.Model, "test/model")
		}
		if req.Temperature != 0 {
			t.Errorf("temperature = %v, want 0", req.Temperature)
		}
		if req.Seed == nil || *req.Seed != 42 {
			t.Errorf("seed = %v, want 42", req.Seed)
		}
		if len(req.Messages) != 2 {
			t.Errorf("messages len = %d, want 2", len(req.Messages))
		}

		// Return mock response
		resp := chatResponse{
			ID: "test-id",
			Choices: []chatChoice{
				{
					Index:        0,
					Message:      chatMessage{Role: "assistant", Content: `{"verb":"build","objects":[{"kind":"service","ref":"test"}]}`},
					FinishReason: "stop",
				},
			},
			Usage: &chatUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := New(&Config{
		Model:       "test/model",
		APIKey:      "test-key",
		Endpoint:    server.URL,
		Temperature: 0,
		Seed:        42,
		MaxTokens:   4096,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	messages := []interpreter.Message{
		{Role: "system", Content: "You are a compiler."},
		{Role: "user", Content: "Build me a website."},
	}

	output, err := client.Decode(context.Background(), messages, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if output != `{"verb":"build","objects":[{"kind":"service","ref":"test"}]}` {
		t.Errorf("output = %q", output)
	}
}

func TestDecodeWithGrammarConstraint(t *testing.T) {
	var gotResponseFormat *responseFormat

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotResponseFormat = req.ResponseFormat

		resp := chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: `{"verb":"find"}`}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := New(&Config{
		Model:       "test/model",
		APIKey:      "test-key",
		Endpoint:    server.URL,
		GrammarMode: GrammarJSONSchema,
		Grammars:    DefaultGrammars(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Decode(context.Background(), []interpreter.Message{
		{Role: "user", Content: "find my wallet"},
	}, "intent_frame@1")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if gotResponseFormat == nil {
		t.Fatal("response_format not set")
	}
	if gotResponseFormat.Type != "json_schema" {
		t.Errorf("response_format.type = %q, want %q", gotResponseFormat.Type, "json_schema")
	}
	if gotResponseFormat.JSONSchema == nil {
		t.Fatal("response_format.json_schema is nil")
	}
	if gotResponseFormat.JSONSchema.Name != "intent_frame" {
		t.Errorf("json_schema.name = %q, want %q", gotResponseFormat.JSONSchema.Name, "intent_frame")
	}
	if !gotResponseFormat.JSONSchema.Strict {
		t.Error("json_schema.strict = false, want true")
	}
}

func TestDecodeUnknownGrammarNoError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.ResponseFormat != nil {
			t.Error("response_format should be nil for unknown grammar")
		}
		resp := chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: "ok"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := New(&Config{
		Model:       "test/model",
		APIKey:      "test-key",
		Endpoint:    server.URL,
		GrammarMode: GrammarJSONSchema,
		Grammars:    DefaultGrammars(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Unknown grammar ID should not error — silent fallback to unconstrained
	output, err := client.Decode(context.Background(), []interpreter.Message{
		{Role: "user", Content: "hello"},
	}, "nonexistent_grammar@1")
	if err != nil {
		t.Fatalf("Decode with unknown grammar: %v", err)
	}
	if output != "ok" {
		t.Errorf("output = %q, want %q", output, "ok")
	}
}

func TestDecodeHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(chatResponse{
			Error: &chatErrorBody{Message: "rate limited", Type: "rate_limit"},
		})
	}))
	defer server.Close()

	client, err := New(&Config{
		Model:    "test/model",
		APIKey:   "test-key",
		Endpoint: server.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Decode(context.Background(), []interpreter.Message{
		{Role: "user", Content: "hello"},
	}, "")
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if !contains(err.Error(), "rate limited") {
		t.Errorf("error = %q, want to contain 'rate limited'", err.Error())
	}
}

func TestDefaultModels(t *testing.T) {
	compiler := DefaultCompilerModel()
	if compiler.Temperature != 0 {
		t.Errorf("compiler temperature = %v, want 0", compiler.Temperature)
	}
	if compiler.Seed != 42 {
		t.Errorf("compiler seed = %v, want 42", compiler.Seed)
	}
	if compiler.GrammarMode != GrammarJSONSchema {
		t.Errorf("compiler grammar mode = %v, want JSONSchema", compiler.GrammarMode)
	}

	executor := DefaultExecutorModel()
	if executor.Temperature != 0.4 {
		t.Errorf("executor temperature = %v, want 0.4", executor.Temperature)
	}
	if executor.GrammarMode != GrammarNone {
		t.Errorf("executor grammar mode = %v, want None", executor.GrammarMode)
	}
}

func TestDefaultGrammars(t *testing.T) {
	grammars := DefaultGrammars()
	if _, ok := grammars["intent_frame@1"]; !ok {
		t.Error("missing intent_frame@1 grammar")
	}
	if _, ok := grammars["verb_vocab@1"]; !ok {
		t.Error("missing verb_vocab@1 grammar")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || s != "" && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
