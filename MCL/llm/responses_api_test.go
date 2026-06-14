// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix/mcl/mtx/interpreter"
)

func newResponsesTestClient(t *testing.T, endpoint string, inject bool) *responsesClient {
	t.Helper()
	cfg := Config{
		Model:          ForgeModelGPT55,
		Endpoint:       endpoint,
		Provider:       ProviderOpencode,
		ProviderSet:    true,
		Shape:          ShapeResponses,
		APIKey:         "test-opencode-key",
		Temperature:    0.2,
		MaxTokens:      256,
		GrammarMode:    GrammarNone,
		InjectIdentity: inject,
	}
	c, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rc, ok := c.(*responsesClient)
	if !ok {
		t.Fatalf("New returned %T, want *responsesClient", c)
	}
	return rc
}

// TestResponsesDecode_HappyPath asserts the request shape (system →
// instructions field; messages → input array) and the response parse
// (walk output[].content[] for output_text parts).
func TestResponsesDecode_HappyPath(t *testing.T) {
	var captured responsesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization header = %q, want Bearer prefix", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := responsesResponse{
			ID: "resp_test", Object: "response", Status: "completed",
			Output: []responsesOutputItem{
				{
					Type: "message", Role: "assistant",
					Content: []responsesContentPart{
						{Type: "output_text", Text: "Hello "},
						{Type: "output_text", Text: "GPT"},
					},
				},
			},
			Usage: &responsesUsage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	msgs := []interpreter.Message{
		{Role: "system", Content: "Be terse."},
		{Role: "user", Content: "say hi"},
	}
	got, err := c.Decode(context.Background(), msgs, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != "Hello GPT" {
		t.Errorf("Decode text = %q, want %q", got, "Hello GPT")
	}
	if captured.Model != ForgeModelGPT55 {
		t.Errorf("captured.Model = %q, want %q", captured.Model, ForgeModelGPT55)
	}
	if captured.Instructions != "Be terse." {
		t.Errorf("captured.Instructions = %q, want %q", captured.Instructions, "Be terse.")
	}
	if len(captured.Input) != 1 || captured.Input[0].Role != "user" {
		t.Errorf("captured.Input = %+v, want one user turn", captured.Input)
	}
}

// TestResponsesDecode_PrefersTopLevelOutputText covers the convenience
// field some proxies populate so callers can skip the output[].content[]
// walk.
func TestResponsesDecode_PrefersTopLevelOutputText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := responsesResponse{
			ID: "resp_id", Object: "response", Status: "completed",
			OutputText: "shortcut payload",
			// output[] intentionally non-empty but ignored when OutputText set.
			Output: []responsesOutputItem{
				{Type: "message", Role: "assistant", Content: []responsesContentPart{
					{Type: "output_text", Text: "different text"},
				}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	got, err := c.Decode(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != "shortcut payload" {
		t.Errorf("got %q, want %q (top-level output_text must win)", got, "shortcut payload")
	}
}

// TestResponsesDecode_InjectsIdentity verifies the preamble lands in the
// instructions field of the upstream request when InjectIdentity=true.
func TestResponsesDecode_InjectsIdentity(t *testing.T) {
	var captured responsesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID: "x", Object: "response", Status: "completed",
			OutputText: "ok",
		})
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, true)
	if _, err := c.Decode(context.Background(), []interpreter.Message{
		{Role: "user", Content: "ping"},
	}, ""); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !strings.Contains(captured.Instructions, IdentityPreamble) {
		t.Errorf("instructions missing IdentityPreamble; got %q", captured.Instructions)
	}
}

// TestResponsesDecode_HTTPError surfaces the upstream error envelope.
func TestResponsesDecode_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(responsesErrorEnvelope{
			Error: &responsesErrorBody{Type: "invalid_request", Message: "bad input shape"},
		})
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	_, err := c.Decode(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "")
	if err == nil {
		t.Fatal("Decode: expected error")
	}
	if !strings.Contains(err.Error(), "bad input shape") || !strings.Contains(err.Error(), "invalid_request") {
		t.Errorf("error missing type+message details: %v", err)
	}
}

// TestResponsesDecode_Refusal surfaces a refusal content part as an
// explicit error. Sess#36: we distinguish refusals from empty responses
// so the caller can decide whether to retry (refusals never recover).
func TestResponsesDecode_Refusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID: "resp_id", Object: "response", Status: "completed",
			Output: []responsesOutputItem{
				{Type: "message", Role: "assistant", Content: []responsesContentPart{
					{Type: "refusal", Text: "I can't help with that."},
				}},
			},
		})
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	_, err := c.Decode(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "")
	if err == nil {
		t.Fatal("Decode: expected refusal error")
	}
	if !strings.Contains(err.Error(), "refusal") {
		t.Errorf("error must mention refusal; got: %v", err)
	}
	if !strings.Contains(err.Error(), "I can't help") {
		t.Errorf("error must include refusal text; got: %v", err)
	}
}

// TestResponsesDecode_NoOutputText surfaces a clear, diagnostic error
// when no output_text content is present in the response (no refusal,
// no text — e.g. truncation or function-call-only output).
func TestResponsesDecode_NoOutputText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID: "resp_id", Object: "response", Status: "incomplete",
			IncompleteDetails: &responsesIncomplete{Reason: "max_output_tokens"},
			Output: []responsesOutputItem{
				{Type: "reasoning", Summary: nil},
			},
		})
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	_, err := c.Decode(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "")
	if err == nil {
		t.Fatal("Decode: expected error on output without text parts")
	}
	if !strings.Contains(err.Error(), "no output_text") {
		t.Errorf("error must mention 'no output_text'; got: %v", err)
	}
	if !strings.Contains(err.Error(), "max_output_tokens") {
		t.Errorf("error must include incomplete reason; got: %v", err)
	}
	if !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("error must include observed item types; got: %v", err)
	}
}

// TestResponsesDecode_FlatOutputText accepts the proxy variant where
// output_text lands directly on the output item rather than nested
// under content[]. opencode-zen sometimes emits this shape.
func TestResponsesDecode_FlatOutputText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID: "resp_id", Object: "response", Status: "completed",
			Output: []responsesOutputItem{
				{Type: "output_text", Text: "hello flat shape"},
			},
		})
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	got, err := c.Decode(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != "hello flat shape" {
		t.Errorf("got %q, want %q", got, "hello flat shape")
	}
}

// TestResponsesDecode_ReasoningSummary accepts text from a reasoning
// item's summary[] when no message item carried output_text. Edge
// case some opencode reasoning models hit on short prompts.
func TestResponsesDecode_ReasoningSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID: "resp_id", Object: "response", Status: "completed",
			Output: []responsesOutputItem{
				{Type: "reasoning", Summary: []responsesContentPart{
					{Type: "summary_text", Text: "fallback reasoning"},
				}},
			},
		})
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	got, err := c.Decode(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != "fallback reasoning" {
		t.Errorf("got %q, want %q", got, "fallback reasoning")
	}
}

// TestResponsesStream_DeltaConcat asserts the SSE parser concatenates
// response.output_text.delta events in order and invokes onDelta.
func TestResponsesStream_DeltaConcat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeResponsesFrame(w, fl, map[string]interface{}{"type": "response.created"})
		writeResponsesFrame(w, fl, map[string]interface{}{"type": "response.output_item.added"})
		writeResponsesFrame(w, fl, map[string]interface{}{"type": "response.content_part.added"})
		writeResponsesFrame(w, fl, map[string]interface{}{
			"type": "response.output_text.delta", "delta": "Hello ",
		})
		writeResponsesFrame(w, fl, map[string]interface{}{
			"type": "response.output_text.delta", "delta": "world",
		})
		writeResponsesFrame(w, fl, map[string]interface{}{
			"type": "response.output_text.done", "text": "Hello world",
		})
		writeResponsesFrame(w, fl, map[string]interface{}{"type": "response.completed"})
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	var deltas []string
	got, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "say hi"},
	}, "", func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got != "Hello world" {
		t.Errorf("final text = %q, want %q", got, "Hello world")
	}
	if len(deltas) != 2 || deltas[0] != "Hello " || deltas[1] != "world" {
		t.Errorf("onDelta sequence = %+v, want [Hello , world]", deltas)
	}
}

// TestResponsesStream_DoneSentinel accepts [DONE] as an alternative
// stream terminator (some proxies emit it instead of response.completed
// when they wrap the upstream stream).
func TestResponsesStream_DoneSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeResponsesFrame(w, fl, map[string]interface{}{
			"type": "response.output_text.delta", "delta": "ok",
		})
		// Raw [DONE] without a wrapping event line.
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer server.Close()

	c := newResponsesTestClient(t, server.URL, false)
	got, err := c.Stream(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

// TestResponsesClient_ImplementsStreamingLLM compile/runtime guard.
func TestResponsesClient_ImplementsStreamingLLM(t *testing.T) {
	cfg := Config{
		Model:       ForgeModelGPT55,
		Endpoint:    OpencodeResponsesEndpoint,
		Provider:    ProviderOpencode,
		ProviderSet: true,
		APIKey:      "x",
	}
	c, _ := New(&cfg)
	if _, ok := c.(interpreter.StreamingLLM); !ok {
		t.Fatal("*responsesClient does not implement interpreter.StreamingLLM")
	}
}

// TestForgeRegistry_RoutingShape asserts ForgeRegistry maps every slot
// to the expected (model, endpoint, shape) tuple. Locks the canvas
// Section 2+3 routing table for the Forge spin-off.
func TestForgeRegistry_RoutingShape(t *testing.T) {
	reg := ForgeRegistry()

	cases := []struct {
		key        RouteKey
		wantModel  string
		wantEndpt  string
		wantShape  APIShape
		wantInject bool
	}{
		{RouteKey{Slot: SlotCompiler}, ForgeModelGPT55, OpencodeResponsesEndpoint, ShapeResponses, true},
		{RouteKey{Slot: SlotPlanner}, ForgeModelClaudeOpus47, OpencodeMessagesEndpoint, ShapeMessages, true},
		{RouteKey{Slot: SlotExecutor, Kind: KindReason}, ForgeModelClaudeOpus47, OpencodeMessagesEndpoint, ShapeMessages, true},
		{RouteKey{Slot: SlotExecutor, Kind: KindCode}, ForgeModelClaudeOpus47, OpencodeMessagesEndpoint, ShapeMessages, true},
		{RouteKey{Slot: SlotExecutor, Kind: KindSummarize}, ForgeModelGPT55, OpencodeResponsesEndpoint, ShapeResponses, true},
		{RouteKey{Slot: SlotExecutor, Kind: KindWrite}, ForgeModelClaudeOpus47, OpencodeMessagesEndpoint, ShapeMessages, true},
		{RouteKey{Slot: SlotExecutor, Kind: KindTransform}, ForgeModelGPT55, OpencodeResponsesEndpoint, ShapeResponses, true},
		{RouteKey{Slot: SlotExecutor, Kind: KindClassify}, ForgeModelGPT55, OpencodeResponsesEndpoint, ShapeResponses, true},
		{RouteKey{Slot: SlotExecutor, Kind: KindHardReason}, ForgeModelClaudeOpus47, OpencodeMessagesEndpoint, ShapeMessages, true},
	}

	for _, c := range cases {
		cfg := reg.Resolve(c.key)
		if cfg.Model != c.wantModel {
			t.Errorf("%+v: Model = %q, want %q", c.key, cfg.Model, c.wantModel)
		}
		if cfg.Endpoint != c.wantEndpt {
			t.Errorf("%+v: Endpoint = %q, want %q", c.key, cfg.Endpoint, c.wantEndpt)
		}
		if cfg.Shape != c.wantShape {
			t.Errorf("%+v: Shape = %s, want %s", c.key, cfg.Shape, c.wantShape)
		}
		if cfg.InjectIdentity != c.wantInject {
			t.Errorf("%+v: InjectIdentity = %v, want %v", c.key, cfg.InjectIdentity, c.wantInject)
		}
		if !cfg.ProviderSet || cfg.Provider != ProviderOpencode {
			t.Errorf("%+v: must set ProviderOpencode explicitly (bare ids don't survive DetectProvider)", c.key)
		}
	}
}

// TestIsOpencodeModelID covers the bare-model-id detection rules used
// when ProviderSet=false. Exercises the catalog prefixes Forge accepts.
func TestIsOpencodeModelID(t *testing.T) {
	yes := []string{
		"claude-opus-4-7", "claude-sonnet-4-6", "claude-haiku-4-5", "claude-3-5-haiku",
		"gpt-5.5", "gpt-5.5-pro", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5-nano",
		"gemini-3.5-flash", "gemini-3-flash",
		"qwen3.6-plus", "qwen3.5-plus",
		"kimi-k2p7-code", "kimi-k2.5",
		"glm-5.1", "glm-5",
		"minimax-m2.7",
		"grok-build-0.1",
		"big-pickle",
		"deepseek-v4-flash-free",
		"mimo-v2.5-free",
		"nemotron-3-super-free",
	}
	no := []string{
		"accounts/fireworks/models/deepseek-v4-flash", // provider-scoped, not bare
		"openai/gpt-oss-120b",                         // together-shaped, not bare
		"deepseek-ai/DeepSeek-V4-Flash",               // together-shaped
		"",                                            // empty
		"some-random-model",                           // unmatched bare id
	}

	for _, m := range yes {
		if !isOpencodeModelID(m) {
			t.Errorf("isOpencodeModelID(%q) = false, want true", m)
		}
	}
	for _, m := range no {
		if isOpencodeModelID(m) {
			t.Errorf("isOpencodeModelID(%q) = true, want false", m)
		}
	}
}

// writeResponsesFrame writes one SSE frame in the OpenAI Responses shape.
func writeResponsesFrame(w http.ResponseWriter, fl http.Flusher, payload map[string]interface{}) {
	body, _ := json.Marshal(payload)
	t, _ := payload["type"].(string)
	if t != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", t)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
	if fl != nil {
		fl.Flush()
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
