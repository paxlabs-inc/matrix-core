// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package llm — OpenAI Responses API client (Session 34 / Forge Phase 1).
//
// Implements interpreter.LLM + interpreter.StreamingLLM against an OpenAI-
// Responses-shaped endpoint at /v1/responses. The opencode.ai/zen proxy
// serves gpt-5* models through this shape; the existing /v1/chat/completions
// client in llm.go cannot speak to it (different request schema, different
// SSE event names, different response envelope).
//
// Wire reference: https://platform.openai.com/docs/api-reference/responses
//
// Request shape (simplified — Forge only uses the conversational subset):
//
//	POST <endpoint>
//	Authorization: Bearer <OPENCODE_API_KEY>
//	{
//	  "model":        "gpt-5.5",
//	  "instructions": "<concatenated system messages>",
//	  "input":        [{"role":"user","content":"..."}, ...],
//	  "max_output_tokens": 4096,
//	  "temperature":  0.4,
//	  "stream":       false
//	}
//
// Response shape (non-streaming):
//
//	{
//	  "id":     "resp_...",
//	  "object": "response",
//	  "status": "completed",
//	  "output": [
//	    {
//	      "type": "message",
//	      "role": "assistant",
//	      "content": [{"type":"output_text","text":"..."}]
//	    }
//	  ],
//	  "usage": {"input_tokens":100, "output_tokens":50, "total_tokens":150}
//	}
//
// Streaming shape (Server-Sent Events on text/event-stream):
//
//	event: response.created               data: {"type":"response.created", ...}
//	event: response.output_item.added     data: {"type":"response.output_item.added", ...}
//	event: response.content_part.added    data: {"type":"response.content_part.added", ...}
//	event: response.output_text.delta     data: {"type":"response.output_text.delta","delta":"Hello"}
//	event: response.output_text.done      data: {"type":"response.output_text.done","text":"Hello world"}
//	event: response.completed             data: {"type":"response.completed","response":{...}}
//
// Text fragments arrive via response.output_text.delta with a top-level
// `delta` string field (NOT nested under `delta.text` like Anthropic).
//
// Grammar/JSON-schema constraint: OpenAI Responses supports response_format
// via the `text.format` field; Forge uses GrammarNone for responses-shape
// callers today since the compiler+planner slots stay on chat-completions
// where the existing grammar plumbing is exercised by tests. Future
// extension can wire `text.format.json_schema` through the same GrammarDef
// registry.

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"matrix/mcl/mtx/interpreter"
)

// responsesClient implements interpreter.LLM + interpreter.StreamingLLM
// against an OpenAI Responses API endpoint.
type responsesClient struct {
	cfg        Config
	httpClient *http.Client
	provider   Provider
	endpoint   string
	apiKey     string
}

// Compile-time interface assertions.
var _ interpreter.LLM = (*responsesClient)(nil)
var _ interpreter.StreamingLLM = (*responsesClient)(nil)

func newResponsesClient(cfg Config, hc *http.Client, p Provider, endpoint, apiKey string) *responsesClient {
	return &responsesClient{
		cfg:        cfg,
		httpClient: hc,
		provider:   p,
		endpoint:   endpoint,
		apiKey:     apiKey,
	}
}

// Decode sends a one-shot Responses request and returns the concatenated
// text from all output_text content parts in the response.
func (c *responsesClient) Decode(ctx context.Context, messages []interpreter.Message, grammar string) (string, error) {
	messages = maybeInjectIdentity(c.cfg, messages)

	req := c.buildRequest(messages)
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("llm: marshal responses request: %w", err)
	}

	httpReq, err := c.newHTTPRequest(ctx, body, "application/json")
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: %s responses POST: %w", c.provider, err)
	}
	defer resp.Body.Close()
	c.dispatchHeaders(resp.Header)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("llm: %s responses read body: %w", c.provider, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed responsesErrorEnvelope
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("llm: %s responses http %d: %s (type=%s)",
				c.provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return "", fmt.Errorf("llm: %s responses http %d: %s",
			c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	var parsed responsesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("llm: %s responses parse response: %w", c.provider, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("llm: %s responses api error: %s", c.provider, parsed.Error.Message)
	}

	// Prefer the top-level convenience field when present; some
	// proxies populate output_text directly so callers don't have to
	// walk the output[].content[] tree.
	if parsed.OutputText != "" {
		return parsed.OutputText, nil
	}

	if len(parsed.Output) == 0 {
		return "", fmt.Errorf("llm: empty output in responses response (status=%q)", parsed.Status)
	}

	// Surface refusals as explicit errors instead of "no output_text" —
	// the model deliberately declined and a retry won't help. Refusal
	// text is what the model said about WHY it refused.
	if refusal := collectRefusal(parsed.Output); refusal != "" {
		return "", fmt.Errorf("llm: %s responses refusal: %s", c.provider, truncate(refusal, 256))
	}

	// Walk every shape: nested content[].output_text, flat item.Text,
	// reasoning summary[]. Any of these is acceptable as the model's
	// final visible text when no `message` item carried output_text
	// (some proxies / reasoning-model edge cases).
	text := collectResponsesText(parsed.Output)
	if text != "" {
		return text, nil
	}

	// Truly nothing — surface enough context (status, incomplete reason,
	// observed item types, body snippet) to diagnose without relooping.
	return "", fmt.Errorf("llm: no output_text content in responses response (status=%q, %s, types=%s, body=%s)",
		parsed.Status,
		describeIncomplete(parsed.IncompleteDetails),
		joinItemTypes(parsed.Output),
		truncate(string(respBody), 320),
	)
}

// collectResponsesText walks every output item shape and returns the
// concatenated visible text. Empty when nothing readable.
func collectResponsesText(items []responsesOutputItem) string {
	var sb strings.Builder
	for _, item := range items {
		// Flat shape: opencode-zen sometimes emits {type:"output_text", text:"..."}
		// at the top level of an output item with no nested content[].
		if item.Type == "output_text" && item.Text != "" {
			sb.WriteString(item.Text)
			continue
		}
		// Nested content[]: standard message + reasoning paths.
		for _, part := range item.Content {
			switch part.Type {
			case "output_text", "text":
				sb.WriteString(part.Text)
			}
		}
		// Some reasoning items surface a summary[] of text parts.
		for _, part := range item.Summary {
			if part.Type == "summary_text" || part.Type == "text" {
				sb.WriteString(part.Text)
			}
		}
	}
	return sb.String()
}

// collectRefusal returns the first non-empty refusal text from any
// content part across the output items. Empty when no refusal landed.
func collectRefusal(items []responsesOutputItem) string {
	for _, item := range items {
		for _, part := range item.Content {
			if part.Refusal != "" {
				return part.Refusal
			}
			if part.Type == "refusal" && part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}

func describeIncomplete(d *responsesIncomplete) string {
	if d == nil || d.Reason == "" {
		return "incomplete=none"
	}
	return "incomplete=" + d.Reason
}

func joinItemTypes(items []responsesOutputItem) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, it.Type)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Stream sends a Responses request with stream:true, parses the SSE event
// stream, and invokes onDelta for each response.output_text.delta event.
// Returns the fully accumulated text on response.completed or EOF.
//
// onDelta MUST NOT be invoked after Stream returns. onDelta == nil is
// permitted; the stream still drains.
func (c *responsesClient) Stream(ctx context.Context, messages []interpreter.Message,
	grammar string, onDelta func(delta string)) (string, error) {

	messages = maybeInjectIdentity(c.cfg, messages)

	req := c.buildRequest(messages)
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("llm: marshal responses stream request: %w", err)
	}

	httpReq, err := c.newHTTPRequest(ctx, body, "text/event-stream")
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: %s responses stream POST: %w", c.provider, err)
	}
	defer resp.Body.Close()
	c.dispatchHeaders(resp.Header)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var parsed responsesErrorEnvelope
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("llm: %s responses stream http %d: %s (type=%s)",
				c.provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return "", fmt.Errorf("llm: %s responses stream http %d: %s",
			c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	return parseResponsesSSEStream(ctx, resp.Body, c.provider, onDelta)
}

// buildRequest assembles the responsesRequest. Splits role=system messages
// out into the top-level `instructions` field (OpenAI Responses convention)
// and folds the rest into the `input[]` array. Multiple system messages are
// concatenated with double-newline separators in declaration order.
func (c *responsesClient) buildRequest(messages []interpreter.Message) *responsesRequest {
	var systemParts []string
	input := make([]responsesInputMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			systemParts = append(systemParts, m.Content)
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		input = append(input, responsesInputMessage{Role: role, Content: m.Content})
	}

	maxOut := c.cfg.MaxTokens
	if maxOut == 0 {
		maxOut = 4096
	}

	req := &responsesRequest{
		Model:           c.cfg.Model,
		Input:           input,
		MaxOutputTokens: maxOut,
		// NOTE: the Responses API is used only for GPT-5+ reasoning models,
		// which reject a `temperature` parameter ("Unsupported parameter:
		// 'temperature'"). Omit it entirely (zero value + omitempty) so these
		// models do not 400. Determinism for this path comes from the model,
		// not a temperature pin.
	}
	if len(systemParts) > 0 {
		req.Instructions = strings.Join(systemParts, "\n\n")
	}
	return req
}

// newHTTPRequest builds the *http.Request shared between Decode + Stream.
// Bearer authorization works against both the real OpenAI API and the
// opencode.ai/zen proxy; no provider-specific header dance like Anthropic's
// x-api-key.
func (c *responsesClient) newHTTPRequest(ctx context.Context, body []byte, accept string) (*http.Request, error) {
	url := c.endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build responses request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return req, nil
}

// dispatchHeaders invokes the configured OnResponseHeaders hook with the
// upstream response headers. Mirrors (*Client).dispatchHeaders verbatim.
func (c *responsesClient) dispatchHeaders(h http.Header) {
	if c == nil || c.cfg.OnResponseHeaders == nil {
		return
	}
	defer func() { _ = recover() }()
	c.cfg.OnResponseHeaders(h)
}

// parseResponsesSSEStream consumes an OpenAI-Responses-shaped SSE stream
// and invokes onDelta for each response.output_text.delta event. Returns
// the accumulated text on response.completed or EOF.
//
// The Responses SSE framing differs from chat-completions:
//   - Each event has both `event:` and `data:` lines (chat-completions uses
//     data: only). We rely on the data: line's `type` field exclusively.
//   - Text fragments arrive as `response.output_text.delta` events whose
//     JSON payload carries a top-level `delta` STRING field (not nested).
//   - response.completed signals end-of-stream; we also accept EOF and
//     [DONE] sentinels for proxy compatibility.
func parseResponsesSSEStream(ctx context.Context, r io.Reader, provider Provider,
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
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return sb.String(), nil
		}

		var frame responsesStreamFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}

		switch frame.Type {
		case "response.output_text.delta":
			if frame.Delta != "" {
				sb.WriteString(frame.Delta)
				if onDelta != nil {
					onDelta(frame.Delta)
				}
			}
		case "response.completed":
			return sb.String(), nil
		case "error", "response.error":
			if frame.Error != nil && frame.Error.Message != "" {
				return sb.String(), fmt.Errorf("llm: %s responses stream api error: %s",
					provider, frame.Error.Message)
			}
		default:
			// response.created, response.output_item.added,
			// response.content_part.added, response.output_text.done,
			// response.output_item.done, ping — accepted and ignored.
		}
	}
	if err := scanner.Err(); err != nil {
		return sb.String(), fmt.Errorf("llm: %s responses stream read: %w", provider, err)
	}
	return sb.String(), nil
}

// --- OpenAI Responses API request/response types ---

type responsesRequest struct {
	Model           string                  `json:"model"`
	Input           []responsesInputMessage `json:"input"`
	Instructions    string                  `json:"instructions,omitempty"`
	MaxOutputTokens int                     `json:"max_output_tokens,omitempty"`
	Temperature     float64                 `json:"temperature,omitempty"`
	Stream          bool                    `json:"stream,omitempty"`
}

type responsesInputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responsesResponse struct {
	ID                string                `json:"id"`
	Object            string                `json:"object"`
	Status            string                `json:"status"`
	Model             string                `json:"model"`
	Output            []responsesOutputItem `json:"output"`
	OutputText        string                `json:"output_text,omitempty"` // convenience field
	Usage             *responsesUsage       `json:"usage,omitempty"`
	Error             *responsesErrorBody   `json:"error,omitempty"`
	IncompleteDetails *responsesIncomplete  `json:"incomplete_details,omitempty"`
}

type responsesIncomplete struct {
	Reason string `json:"reason,omitempty"` // e.g. "max_output_tokens", "content_filter"
}

// responsesOutputItem covers every variant in `output[]`:
//
//	type=message            visible assistant turn (content[] carries output_text/refusal)
//	type=reasoning          chain-of-thought summary; some models surface text via Summary[]
//	type=function_call      tool call (Name + Arguments populated; Content stays empty)
//	type=tool_call          alias used by some proxies
//	type=output_text        flat shape some opencode-zen builds emit (Text on the item itself)
type responsesOutputItem struct {
	Type    string                 `json:"type"`
	Role    string                 `json:"role,omitempty"`
	Content []responsesContentPart `json:"content,omitempty"`
	Summary []responsesContentPart `json:"summary,omitempty"`
	// Flat output_text — some proxies / models emit text directly on
	// the output item rather than nested under content[].
	Text string `json:"text,omitempty"`
}

type responsesContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type responsesErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

type responsesErrorEnvelope struct {
	Error *responsesErrorBody `json:"error,omitempty"`
}

// responsesStreamFrame matches every event payload the Responses SSE
// channel emits. Fields populated depend on Type; unused fields stay
// zero-valued.
type responsesStreamFrame struct {
	Type  string              `json:"type"`
	Delta string              `json:"delta,omitempty"` // for response.output_text.delta
	Text  string              `json:"text,omitempty"`  // for response.output_text.done
	Error *responsesErrorBody `json:"error,omitempty"`
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
