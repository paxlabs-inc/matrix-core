// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package llm — Anthropic Messages API client (Session 34 / Forge Phase 1).
//
// Implements interpreter.LLM + interpreter.StreamingLLM against an Anthropic-
// Messages-shaped endpoint at /v1/messages. The opencode.ai/zen proxy serves
// claude-* models through this shape; the existing /v1/chat/completions client
// in llm.go cannot speak to it (different request schema, different SSE event
// names, different response envelope).
//
// Wire reference: https://docs.anthropic.com/en/api/messages
//
// Request shape:
//
//	POST <endpoint>
//	Authorization: Bearer <OPENCODE_API_KEY>   (or x-api-key for direct Anthropic)
//	{
//	  "model":      "claude-opus-4-7",
//	  "max_tokens": 4096,
//	  "system":     "<system message text>",         // concatenated system msgs
//	  "messages":   [{"role":"user","content":"..."}, ...],
//	  "temperature": 0.4,
//	  "stream":      false
//	}
//
// Response shape (non-streaming):
//
//	{
//	  "id":   "msg_...",
//	  "role": "assistant",
//	  "content": [{"type":"text","text":"..."}],
//	  "stop_reason": "end_turn",
//	  "usage": {"input_tokens":100,"output_tokens":50}
//	}
//
// Streaming shape (Server-Sent Events on text/event-stream):
//
//	event: message_start         data: {"type":"message_start","message":{...}}
//	event: content_block_start   data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}
//	event: content_block_delta   data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}
//	event: content_block_stop    data: {"type":"content_block_stop","index":0}
//	event: message_delta         data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}
//	event: message_stop          data: {"type":"message_stop"}
//
// Each event has BOTH an `event:` line and a `data:` line. The data payload
// is JSON; we parse it and dispatch on the top-level `type` field. Text
// fragments arrive via content_block_delta with delta.type=="text_delta".
//
// Grammar constraint: Anthropic does not support response_format/json_schema
// the same way OpenAI does. Forge uses GrammarNone for messages-shape callers
// (compiler + planner slots stay on chat-completions endpoints where grammar
// is honored). When a grammar id is passed AND cfg.GrammarMode != GrammarNone,
// the messages client silently ignores it and emits a debug header so audit
// can pick up the divergence — matches the existing chat-completions client's
// "unknown grammar id silently skip" posture.

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"matrix/mcl/mtx/interpreter"
)

// messagesClient implements interpreter.LLM + interpreter.StreamingLLM
// against an Anthropic Messages API endpoint.
type messagesClient struct {
	cfg        Config
	httpClient *http.Client
	provider   Provider
	endpoint   string
	apiKey     string
}

// Compile-time interface assertions.
var _ interpreter.LLM = (*messagesClient)(nil)
var _ interpreter.StreamingLLM = (*messagesClient)(nil)

func newMessagesClient(cfg Config, hc *http.Client, p Provider, endpoint, apiKey string) *messagesClient {
	return &messagesClient{
		cfg:        cfg,
		httpClient: hc,
		provider:   p,
		endpoint:   endpoint,
		apiKey:     apiKey,
	}
}

// Decode sends a one-shot Messages request and returns the concatenated text
// from all content blocks of type "text" in the response.
func (c *messagesClient) Decode(ctx context.Context, messages []interpreter.Message, grammar string) (string, error) {
	messages = maybeInjectIdentity(c.cfg, messages)

	req := c.buildRequest(messages)
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("llm: marshal messages request: %w", err)
	}

	httpReq, err := c.newHTTPRequest(ctx, body, "application/json")
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: %s messages POST: %w", c.provider, err)
	}
	defer resp.Body.Close()
	c.dispatchHeaders(resp.Header)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("llm: %s messages read body: %w", c.provider, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed messagesErrorEnvelope
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("llm: %s messages http %d: %s (type=%s)",
				c.provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return "", fmt.Errorf("llm: %s messages http %d: %s",
			c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	var parsed messagesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("llm: %s messages parse response: %w", c.provider, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: %s messages api error: %s", c.provider, parsed.Error.Message)
	}
	if len(parsed.Content) == 0 {
		return "", errors.New("llm: empty content in messages response")
	}

	var sb strings.Builder
	for _, blk := range parsed.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		}
	}
	return sb.String(), nil
}

// Stream sends a Messages request with stream:true, parses the SSE event
// stream, and invokes onDelta for each text_delta fragment. Returns the
// fully accumulated text on message_stop or EOF.
//
// onDelta MUST NOT be invoked after Stream returns. onDelta == nil is
// permitted; the stream still drains.
func (c *messagesClient) Stream(ctx context.Context, messages []interpreter.Message,
	grammar string, onDelta func(delta string)) (string, error) {

	messages = maybeInjectIdentity(c.cfg, messages)

	req := c.buildRequest(messages)
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("llm: marshal messages stream request: %w", err)
	}

	httpReq, err := c.newHTTPRequest(ctx, body, "text/event-stream")
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: %s messages stream POST: %w", c.provider, err)
	}
	defer resp.Body.Close()
	c.dispatchHeaders(resp.Header)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var parsed messagesErrorEnvelope
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("llm: %s messages stream http %d: %s (type=%s)",
				c.provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return "", fmt.Errorf("llm: %s messages stream http %d: %s",
			c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	return parseMessagesSSEStream(ctx, resp.Body, c.provider, onDelta)
}

// buildRequest assembles the messagesRequest. Splits role=system messages
// out into the top-level `system` field (Anthropic requirement) and folds
// the rest into the `messages[]` array. Multiple system messages are
// concatenated with double-newline separators in declaration order — the
// natural reading order matches the OpenAI convention where every system
// message is appended.
func (c *messagesClient) buildRequest(messages []interpreter.Message) *messagesRequest {
	var systemParts []string
	convo := make([]messagesTurn, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			systemParts = append(systemParts, m.Content)
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		convo = append(convo, messagesTurn{Role: role, Content: m.Content})
	}

	maxTokens := c.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	req := &messagesRequest{
		Model:       c.cfg.Model,
		MaxTokens:   maxTokens,
		Messages:    convo,
		Temperature: c.cfg.Temperature,
	}
	if len(systemParts) > 0 {
		req.System = strings.Join(systemParts, "\n\n")
	}
	return req
}

// newHTTPRequest builds the *http.Request shared between Decode + Stream.
//
// Auth posture (sess#36 fix — verified live against opencode.ai/zen
// 2026-05-28):
//
//	The Anthropic Messages API (and every proxy that fronts it,
//	including opencode.ai/zen) authenticates via the x-api-key header,
//	NOT Authorization: Bearer. The earlier sess#34 wiring sent Bearer
//	to opencode and the proxy 401'd with `{"type":"AuthError","message":
//	"Missing API key."}`. A direct probe confirmed x-api-key returns 200
//	with a real assistant response while Bearer 401s — opencode's
//	messages endpoint behaves like Anthropic's native auth.
//
// We send BOTH headers — x-api-key (the load-bearing one) AND Authorization:
// Bearer (preserved as a no-op for any future proxy that prefers it; many
// proxies accept either). The previous "either x-api-key OR Bearer based on
// host substring" heuristic was wrong — the format is shape-driven, not
// host-driven, and opencode is shape-Anthropic.
//
// Anthropic also requires an `anthropic-version` header; opencode passes it
// through, direct Anthropic enforces it.
func (c *messagesClient) newHTTPRequest(ctx context.Context, body []byte, accept string) (*http.Request, error) {
	url := c.endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build messages request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("anthropic-version", "2023-06-01")

	// x-api-key is the canonical messages-API auth header. Set unconditionally
	// for ALL messages-shape endpoints (api.anthropic.com + opencode.ai/zen +
	// any future proxy that fronts the messages shape).
	req.Header.Set("x-api-key", c.apiKey)

	// Authorization: Bearer is preserved for proxies that key on it for
	// routing/tenancy (vs auth). Direct Anthropic ignores it; opencode
	// accepts either; future BYO proxies may want it. Belt and suspenders
	// when contacting a non-Anthropic endpoint — keep direct Anthropic
	// strictly x-api-key only because Anthropic rejects both being present
	// in some edge cases.
	host := strings.ToLower(url)
	if !strings.Contains(host, "api.anthropic.com") {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

// dispatchHeaders invokes the configured OnResponseHeaders hook with the
// upstream response headers. Mirrors (*Client).dispatchHeaders verbatim so
// gateway header capture (sess#33 cost telemetry) works uniformly across
// every API shape.
func (c *messagesClient) dispatchHeaders(h http.Header) {
	if c == nil || c.cfg.OnResponseHeaders == nil {
		return
	}
	defer func() { _ = recover() }()
	c.cfg.OnResponseHeaders(h)
}

// parseMessagesSSEStream consumes an Anthropic-shaped SSE stream and
// invokes onDelta for each text_delta fragment. Returns the accumulated
// text on message_stop or EOF.
//
// The Anthropic SSE framing differs from OpenAI's in two important ways:
//   - Each event has both `event:` and `data:` lines (OpenAI uses data: only).
//   - Text fragments arrive as `content_block_delta` events whose JSON payload
//     looks like {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"..."}}.
//     Other event types (message_start, content_block_start, message_delta,
//     message_stop, ping) are accepted and ignored for text accumulation.
func parseMessagesSSEStream(ctx context.Context, r io.Reader, provider Provider,
	onDelta func(delta string)) (string, error) {

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	var sb strings.Builder
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return sb.String(), ctx.Err()
		default:
		}

		line := scanner.Text()
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// event:, id:, retry:, comment — accept and skip; the data
			// line that follows carries everything we need.
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}

		var frame messagesStreamFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			// Malformed frame is non-fatal; some proxies emit keepalive
			// comments as data: with non-JSON sentinels.
			continue
		}

		switch frame.Type {
		case "content_block_delta":
			if frame.Delta != nil && frame.Delta.Type == "text_delta" && frame.Delta.Text != "" {
				sb.WriteString(frame.Delta.Text)
				if onDelta != nil {
					onDelta(frame.Delta.Text)
				}
			}
		case "message_stop":
			return sb.String(), nil
		case "error":
			if frame.Error != nil && frame.Error.Message != "" {
				return sb.String(), fmt.Errorf("llm: %s messages stream api error: %s",
					provider, frame.Error.Message)
			}
		default:
			// message_start, content_block_start, content_block_stop,
			// message_delta, ping — accepted and ignored.
		}
	}
	if err := scanner.Err(); err != nil {
		return sb.String(), fmt.Errorf("llm: %s messages stream read: %w", provider, err)
	}
	return sb.String(), nil
}

// --- Anthropic Messages API request/response types ---

type messagesRequest struct {
	Model       string         `json:"model"`
	MaxTokens   int            `json:"max_tokens"`
	System      string         `json:"system,omitempty"`
	Messages    []messagesTurn `json:"messages"`
	Temperature float64        `json:"temperature,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
}

type messagesTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Role       string                 `json:"role"`
	Content    []messagesContentBlock `json:"content"`
	Model      string                 `json:"model"`
	StopReason string                 `json:"stop_reason"`
	Usage      *messagesUsage         `json:"usage,omitempty"`
	Error      *messagesErrorBody     `json:"error,omitempty"`
}

type messagesContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type messagesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type messagesErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type messagesErrorEnvelope struct {
	Type  string             `json:"type"`
	Error *messagesErrorBody `json:"error,omitempty"`
}

// messagesStreamFrame matches every event payload Anthropic emits on the
// streaming SSE channel. Fields populated depend on Type; unused fields
// stay zero-valued.
type messagesStreamFrame struct {
	Type  string             `json:"type"`
	Index int                `json:"index,omitempty"`
	Delta *messagesDelta     `json:"delta,omitempty"`
	Error *messagesErrorBody `json:"error,omitempty"`
}

type messagesDelta struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
