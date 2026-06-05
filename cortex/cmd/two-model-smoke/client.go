// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// OpenAI-compatible chat-completions client for Together AI and
// Fireworks AI. Both providers expose the OpenAI v1 chat-completions
// schema verbatim, so a single client switches base URL + auth header
// based on the model string format.
//
// Provider detection:
//   "openai/<model>"               → Together (api.together.xyz)
//   "accounts/fireworks/models/X"  → Fireworks (api.fireworks.ai)
//
// Tool calling follows the OpenAI v1 shape:
//   - Request: messages[].tool_calls (when assistant requests tools)
//   - Request: messages[] of role "tool" carrying tool_call_id + content
//   - Response: choice.message.tool_calls when the model wants tools
//
// stdlib-only; no third-party SDK pulled in.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Provider identifies the API endpoint + env-var the client uses.
type Provider int

const (
	ProviderTogether Provider = iota
	ProviderFireworks
)

func (p Provider) String() string {
	switch p {
	case ProviderTogether:
		return "together"
	case ProviderFireworks:
		return "fireworks"
	}
	return "?"
}

// detectProvider maps a model string to its provider. Together's
// catalog uses "openai/<model>" or "<vendor>/<model>" form; Fireworks
// uses "accounts/fireworks/models/<model>". Anything else returns an
// error so misconfiguration fails loudly.
func detectProvider(model string) (Provider, error) {
	switch {
	case strings.HasPrefix(model, "accounts/fireworks/"):
		return ProviderFireworks, nil
	case strings.Contains(model, "/"):
		return ProviderTogether, nil
	}
	return 0, fmt.Errorf("client: cannot detect provider for model %q (expected 'openai/...' or 'accounts/fireworks/models/...')", model)
}

// providerEndpoint returns the chat-completions URL.
func providerEndpoint(p Provider) string {
	switch p {
	case ProviderTogether:
		return "https://api.together.xyz/v1/chat/completions"
	case ProviderFireworks:
		return "https://api.fireworks.ai/inference/v1/chat/completions"
	}
	return ""
}

// providerKey reads the env var for the given provider.
func providerKey(p Provider) (string, error) {
	var name string
	switch p {
	case ProviderTogether:
		name = "TOGETHER_API_KEY"
	case ProviderFireworks:
		name = "FIREWORKS_API_KEY"
	default:
		return "", fmt.Errorf("client: unknown provider %d", p)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("client: %s is not set", name)
	}
	return v, nil
}

// ChatMessage is the OpenAI-compatible message shape.
//
// Role is one of "system" | "user" | "assistant" | "tool".
// ToolCalls is set on assistant messages that requested tools.
// ToolCallID is set on role=="tool" messages identifying which call this
// result satisfies.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall mirrors OpenAI's `tool_calls[]` entry shape.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc carries the function name + JSON-encoded args string.
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef describes one tool to the model.
type ToolDef struct {
	Type     string          `json:"type"` // "function"
	Function ToolDefFunction `json:"function"`
}

// ToolDefFunction carries the JSON-schema parameter definition.
type ToolDefFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ChatRequest is the request body sent to the chat-completions endpoint.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Tools       []ToolDef     `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"` // "auto" | "none" | object
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

// ChatResponse is the response shape (subset of OpenAI's response —
// only the fields we read).
type ChatResponse struct {
	ID      string         `json:"id"`
	Choices []ChatChoice   `json:"choices"`
	Usage   *ChatUsage     `json:"usage,omitempty"`
	Error   *ChatErrorBody `json:"error,omitempty"`
}

// ChatChoice is one entry from the choices array. We always read
// choices[0].
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatUsage carries token-count diagnostics.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatErrorBody is the structured error body returned by both
// providers on 4xx/5xx.
type ChatErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// Client carries config + a reusable http.Client. Per-call fields
// (model, messages, tools) are passed at Call time.
type Client struct {
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient returns a Client with a sane default timeout (90s — small
// models like deepseek-v4-flash can take 30-60s on long tool-calling
// turns).
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 90 * time.Second},
		timeout:    90 * time.Second,
	}
}

// Call sends one chat-completions request to the provider determined
// by the model string. Returns the assistant's message (which may
// contain tool_calls) plus token usage. Errors include the raw
// provider response body for debuggability.
func (c *Client) Call(model string, messages []ChatMessage, tools []ToolDef, temperature float64) (*ChatMessage, *ChatUsage, error) {
	provider, err := detectProvider(model)
	if err != nil {
		return nil, nil, err
	}
	apiKey, err := providerKey(provider)
	if err != nil {
		return nil, nil, err
	}
	endpoint := providerEndpoint(provider)

	req := ChatRequest{
		Model:       model,
		Messages:    messages,
		Tools:       tools,
		Temperature: temperature,
		MaxTokens:   4096,
	}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("client: marshal request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("client: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("client: %s POST: %w", provider, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("client: %s read body: %w", provider, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to parse structured error; fall back to raw body.
		var parsed ChatResponse
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return nil, nil, fmt.Errorf("client: %s http %d: %s (type=%s code=%s)",
				provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type, parsed.Error.Code)
		}
		return nil, nil, fmt.Errorf("client: %s http %d: %s", provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	var parsed ChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, nil, fmt.Errorf("client: %s parse response: %w (body=%s)",
			provider, err, truncate(string(respBody), 512))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, nil, fmt.Errorf("client: %s api error: %s", provider, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, nil, errors.New("client: empty choices in response")
	}

	msg := parsed.Choices[0].Message
	return &msg, parsed.Usage, nil
}

// truncate is a tiny helper for log-line-friendly bodies.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
