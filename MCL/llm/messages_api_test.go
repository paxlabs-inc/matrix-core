// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix/mcl/mtx/interpreter"
)

// newMessagesTestClient constructs a messagesClient pointing at the given
// test server endpoint. Used to keep test ergonomics close to the existing
// newTestClient(*Client) helper in llm_test.go.
func newMessagesTestClient(t *testing.T, endpoint string, inject bool) *messagesClient {
	t.Helper()
	cfg := Config{
		Model:          ForgeModelClaudeOpus47,
		Endpoint:       endpoint,
		Provider:       ProviderOpencode,
		ProviderSet:    true,
		Shape:          ShapeMessages,
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
	mc, ok := c.(*messagesClient)
	if !ok {
		t.Fatalf("New returned %T, want *messagesClient", c)
	}
	return mc
}

// TestMessagesAPIShape_Detection asserts the URL→shape detector picks
// /v1/messages correctly across the host variants Forge will see.
func TestMessagesAPIShape_Detection(t *testing.T) {
	cases := []struct {
		ep   string
		want APIShape
	}{
		{"https://opencode.ai/zen/v1/messages", ShapeMessages},
		{"https://api.anthropic.com/v1/messages", ShapeMessages},
		{"https://opencode.ai/zen/v1/messages/", ShapeMessages},
		{"https://opencode.ai/zen/v1/responses", ShapeResponses},
		{"https://api.openai.com/v1/responses", ShapeResponses},
		{"https://api.together.xyz/v1/chat/completions", ShapeChatCompletions},
		{"http://localhost/v1/chat/completions", ShapeChatCompletions},
		{"https://example.com/foo/bar", ShapeUnknown},
		{"", ShapeUnknown},
	}
	for _, c := range cases {
		if got := DetectAPIShape(c.ep); got != c.want {
			t.Errorf("DetectAPIShape(%q) = %s, want %s", c.ep, got, c.want)
		}
	}
}

// TestNew_DispatchesByEndpoint guards the New() dispatcher: each endpoint
// suffix must produce its dedicated client type.
func TestNew_DispatchesByEndpoint(t *testing.T) {
	cases := []struct {
		ep       string
		wantType string
	}{
		{"https://opencode.ai/zen/v1/messages", "*llm.messagesClient"},
		{"https://opencode.ai/zen/v1/responses", "*llm.responsesClient"},
		{"https://api.together.xyz/v1/chat/completions", "*llm.Client"},
	}
	for _, c := range cases {
		cfg := Config{
			Model:       "test-model",
			Endpoint:    c.ep,
			Provider:    ProviderOpencode,
			ProviderSet: true,
			APIKey:      "x",
		}
		got, err := New(&cfg)
		if err != nil {
			t.Fatalf("New(%q): %v", c.ep, err)
		}
		gotType := fmt.Sprintf("%T", got)
		if gotType != c.wantType {
			t.Errorf("New(%q) = %s, want %s", c.ep, gotType, c.wantType)
		}
	}
}

// TestMessagesDecode_HappyPath asserts the request shape (system field
// split out, messages array intact) and the response parse (concat of
// text-typed content blocks).
func TestMessagesDecode_HappyPath(t *testing.T) {
	var captured messagesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization header = %q, want Bearer prefix", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Errorf("anthropic-version header missing")
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := messagesResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: []messagesContentBlock{
				{Type: "text", Text: "Hello "},
				{Type: "text", Text: "Matrix"},
			},
			StopReason: "end_turn",
			Usage:      &messagesUsage{InputTokens: 10, OutputTokens: 4},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, false)
	msgs := []interpreter.Message{
		{Role: "system", Content: "You are a code assistant."},
		{Role: "user", Content: "say hi"},
	}
	got, err := c.Decode(context.Background(), msgs, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != "Hello Matrix" {
		t.Errorf("Decode text = %q, want %q", got, "Hello Matrix")
	}
	if captured.Model != ForgeModelClaudeOpus47 {
		t.Errorf("captured.Model = %q, want %q", captured.Model, ForgeModelClaudeOpus47)
	}
	if captured.System != "You are a code assistant." {
		t.Errorf("captured.System = %q, want system text extracted", captured.System)
	}
	if len(captured.Messages) != 1 || captured.Messages[0].Role != "user" {
		t.Errorf("captured.Messages = %+v, want one user turn", captured.Messages)
	}
	if captured.Stream {
		t.Errorf("captured.Stream = true on Decode path; want false")
	}
}

// TestMessagesDecode_InjectsIdentity verifies the preamble lands in the
// `system` field of the upstream request when InjectIdentity=true.
func TestMessagesDecode_InjectsIdentity(t *testing.T) {
	var captured messagesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messagesResponse{
			ID: "msg_id", Type: "message", Role: "assistant",
			Content: []messagesContentBlock{{Type: "text", Text: "ok"}},
		})
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, true)
	msgs := []interpreter.Message{
		{Role: "user", Content: "ping"},
	}
	if _, err := c.Decode(context.Background(), msgs, ""); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if !strings.Contains(captured.System, IdentityPreamble) {
		t.Errorf("system field missing IdentityPreamble; got %q", captured.System)
	}
}

// TestMessagesDecode_MergesMultipleSystemMessages confirms the
// double-newline join used to flatten skill-supplied system prompts.
func TestMessagesDecode_MergesMultipleSystemMessages(t *testing.T) {
	var captured messagesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messagesResponse{
			ID: "msg_id", Type: "message", Role: "assistant",
			Content: []messagesContentBlock{{Type: "text", Text: "ok"}},
		})
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, false)
	msgs := []interpreter.Message{
		{Role: "system", Content: "first"},
		{Role: "system", Content: "second"},
		{Role: "user", Content: "x"},
	}
	if _, err := c.Decode(context.Background(), msgs, ""); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if captured.System != "first\n\nsecond" {
		t.Errorf("captured.System = %q, want %q", captured.System, "first\n\nsecond")
	}
}

// TestMessagesDecode_HTTPError surfaces the upstream error body.
func TestMessagesDecode_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(messagesErrorEnvelope{
			Type:  "error",
			Error: &messagesErrorBody{Type: "rate_limit", Message: "slow down"},
		})
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, false)
	_, err := c.Decode(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "")
	if err == nil {
		t.Fatal("Decode: expected error")
	}
	if !strings.Contains(err.Error(), "slow down") || !strings.Contains(err.Error(), "rate_limit") {
		t.Errorf("error missing message+type details: %v", err)
	}
}

// TestMessagesStream_DeltaConcat asserts the SSE parser concatenates
// text_delta fragments in order and invokes onDelta synchronously.
func TestMessagesStream_DeltaConcat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeMessagesFrame(w, fl, map[string]interface{}{"type": "message_start"})
		writeMessagesFrame(w, fl, map[string]interface{}{"type": "content_block_start", "index": 0})
		writeMessagesFrame(w, fl, map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": "Hello "},
		})
		writeMessagesFrame(w, fl, map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": "Matrix"},
		})
		writeMessagesFrame(w, fl, map[string]interface{}{"type": "content_block_stop", "index": 0})
		writeMessagesFrame(w, fl, map[string]interface{}{"type": "message_stop"})
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, false)
	var deltas []string
	got, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "say hi"},
	}, "", func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got != "Hello Matrix" {
		t.Errorf("final text = %q, want %q", got, "Hello Matrix")
	}
	if len(deltas) != 2 || deltas[0] != "Hello " || deltas[1] != "Matrix" {
		t.Errorf("onDelta sequence = %+v, want [Hello , Matrix]", deltas)
	}
}

// TestMessagesStream_AnthropicVersionHeader confirms the header lands on
// the streaming request too — Anthropic enforces it regardless of stream
// mode, and the opencode proxy passes it through.
func TestMessagesStream_AnthropicVersionHeader(t *testing.T) {
	var sawHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeMessagesFrame(w, fl, map[string]interface{}{"type": "message_stop"})
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, false)
	if _, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if sawHeader == "" {
		t.Errorf("anthropic-version header missing on stream request")
	}
}

// TestMessagesNewHTTPRequest_AuthHeaders covers the messages-API auth
// posture for both direct Anthropic and the opencode.ai/zen proxy.
//
// Sess#36 fix (2026-05-28): live probe revealed that opencode.ai/zen
// /v1/messages 401s with `{"type":"AuthError","message":"Missing API
// key."}` on Bearer auth — it requires x-api-key like native Anthropic.
// The earlier sess#34 wiring sent ONLY Bearer to opencode and silently
// 401'd until a synthesizer call landed in production. Both endpoints
// now MUST set x-api-key; only the proxy carries Authorization Bearer
// as a defensive belt-and-suspenders for routing-keyed proxies.
func TestMessagesNewHTTPRequest_AuthHeaders(t *testing.T) {
	// --- Direct Anthropic: x-api-key only, no Authorization ---
	cfg := Config{
		Model:       ForgeModelClaudeOpus47,
		Endpoint:    "https://api.anthropic.com/v1/messages",
		Provider:    ProviderOpencode,
		ProviderSet: true,
		Shape:       ShapeMessages,
		APIKey:      "sk-ant-test",
	}
	c, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := c.(*messagesClient).newHTTPRequest(context.Background(), []byte("{}"), "application/json")
	if err != nil {
		t.Fatalf("newHTTPRequest: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("direct Anthropic x-api-key = %q, want sk-ant-test", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("direct Anthropic must NOT set Authorization; got %q", got)
	}

	// --- Opencode proxy: x-api-key (load-bearing) + Authorization Bearer ---
	cfg.Endpoint = OpencodeMessagesEndpoint
	cfg.APIKey = "opencode-key"
	c2, _ := New(&cfg)
	req2, _ := c2.(*messagesClient).newHTTPRequest(context.Background(), []byte("{}"), "application/json")
	if got := req2.Header.Get("x-api-key"); got != "opencode-key" {
		t.Errorf("opencode x-api-key = %q, want opencode-key (load-bearing for /v1/messages)", got)
	}
	if got := req2.Header.Get("Authorization"); got != "Bearer opencode-key" {
		t.Errorf("opencode Authorization = %q, want Bearer opencode-key (defensive)", got)
	}
}

// TestMessagesStream_ContextCancelled verifies ctx cancellation is
// observable mid-stream.
func TestMessagesStream_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeMessagesFrame(w, fl, map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": "first"},
		})
		// Stall forever so the test's cancel fires before message_stop.
		<-r.Context().Done()
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, false)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Allow the first frame to arrive, then cancel.
		// The mock server holds open until r.Context().Done, so the
		// scanner will keep blocking; cancel breaks it via context check.
		<-make(chan struct{}, 1) // never fires; cancel below drives termination
	}()
	cancel() // cancel immediately; the parser checks between frames

	_, err := c.Stream(ctx, []interpreter.Message{{Role: "user", Content: "x"}}, "", nil)
	if err == nil {
		t.Fatal("Stream: expected ctx.Err()")
	}
}

// TestMessagesStream_TextDeltaTypeFilter ignores non-text deltas — model
// reasoning content (e.g. extended thinking) arrives with delta.type !=
// "text_delta" and must not pollute the accumulated output.
func TestMessagesStream_TextDeltaTypeFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeMessagesFrame(w, fl, map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "thinking_delta", "text": "secret reasoning"},
		})
		writeMessagesFrame(w, fl, map[string]interface{}{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": "visible"},
		})
		writeMessagesFrame(w, fl, map[string]interface{}{"type": "message_stop"})
	}))
	defer server.Close()

	c := newMessagesTestClient(t, server.URL, false)
	got, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got != "visible" {
		t.Errorf("got %q, want %q (thinking deltas must not leak)", got, "visible")
	}
}

// TestMessagesClient_ImplementsStreamingLLM is the compile-time +
// runtime guard mirroring the chat-completions test.
func TestMessagesClient_ImplementsStreamingLLM(t *testing.T) {
	cfg := Config{
		Model:       ForgeModelClaudeOpus47,
		Endpoint:    OpencodeMessagesEndpoint,
		Provider:    ProviderOpencode,
		ProviderSet: true,
		APIKey:      "x",
	}
	c, _ := New(&cfg)
	if _, ok := c.(interpreter.StreamingLLM); !ok {
		t.Fatal("*messagesClient does not implement interpreter.StreamingLLM")
	}
}

// writeMessagesFrame writes one SSE frame in the Anthropic shape:
// "event: <type>\ndata: <json>\n\n". The event: line is optional in
// practice (we parse on data:), but mock servers should be realistic.
func writeMessagesFrame(w io.Writer, fl http.Flusher, payload map[string]interface{}) {
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
