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
		{"deepseek-ai/DeepSeek-V4-Flash", ProviderTogether, false},
		{"openai/gpt-oss-120b", ProviderTogether, false},
		{"Qwen/Qwen3.5-9B-FP8", ProviderTogether, false},
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
