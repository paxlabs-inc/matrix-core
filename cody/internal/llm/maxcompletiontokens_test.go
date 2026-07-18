// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcllm "matrix/mcl/llm"
)

// TestMaxCompletionTokensWire pins FIX 2 on the real Chat path: an xAI/grok
// client serializes `max_completion_tokens` and OMITS the deprecated
// `max_tokens`, while every other provider keeps `max_tokens` and never emits
// `max_completion_tokens`. It drives Chat against a mock chat-completions
// endpoint, captures the raw request body, and asserts presence/absence of
// each field on the actual bytes that leave the client.
func TestMaxCompletionTokensWire(t *testing.T) {
	tests := []struct {
		name           string
		model          string
		wantCompletion bool // true: expect max_completion_tokens (grok); false: expect max_tokens
	}{
		{"grok uses max_completion_tokens", "xiaomimimo/mimo-v2.5-pro", true},
		{"non-grok uses max_tokens", "accounts/fireworks/models/deepseek-v4-flash", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				// Minimal valid OpenAI SSE stream folding to one assistant turn.
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n"))
				w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n"))
				w.Write([]byte("data: [DONE]\n"))
			}))
			defer server.Close()

			client, err := New(mcllm.Config{
				Model:     tt.model,
				APIKey:    "test-key",
				Endpoint:  server.URL + "/v1/chat/completions",
				MaxTokens: 2048,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := client.Chat(context.Background(), ChatRequest{
				Messages: []Message{UserMessage("hi")},
			}); err != nil {
				t.Fatalf("Chat: %v", err)
			}

			_, hasMax := raw["max_tokens"]
			maxCompletion, hasCompletion := raw["max_completion_tokens"]
			if tt.wantCompletion {
				if !hasCompletion {
					t.Errorf("grok request missing max_completion_tokens; got %v", raw)
				}
				if string(maxCompletion) != "2048" {
					t.Errorf("max_completion_tokens = %s, want 2048", maxCompletion)
				}
				if hasMax {
					t.Errorf("grok request must OMIT max_tokens; got %v", raw)
				}
			} else {
				if !hasMax {
					t.Errorf("non-grok request missing max_tokens; got %v", raw)
				}
				if hasCompletion {
					t.Errorf("non-grok request must OMIT max_completion_tokens; got %v", raw)
				}
			}
		})
	}
}
