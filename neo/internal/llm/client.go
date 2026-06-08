// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	mcllm "matrix/mcl/llm"
)

// Client is an OpenAI-compatible chat-completions client that speaks native
// function calling (tools + tool_calls + tool-role results).
//
// It is constructed from a matrix/mcl/llm.Config so it inherits provider
// detection, the model registry, and — when Config.GatewayURL is set — the
// MatrixGateway metering path (X-Matrix-* headers, bearer from
// MATRIX_GATEWAY_TOKEN). When GatewayURL is empty it talks directly to the
// provider with the provider's API key.
type Client struct {
	model           string
	provider        mcllm.Provider
	endpoint        string
	apiKey          string
	gatewayURL      string
	gatewayTokenEnv string
	actorDID        string
	intentID        string
	slotLabel       string

	temperature float64
	maxTokens   int
	seed        int64

	httpClient *http.Client
}

// New builds a function-calling client from an mcl/llm.Config.
//
// v1 supports the chat-completions API shape only (Together / Fireworks /
// opencode-chat). Models that route to the Anthropic Messages or OpenAI
// Responses shapes use different tool schemas and are rejected with a clear
// error; the default model registry pins Fireworks chat-completions.
func New(cfg mcllm.Config) (*Client, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("neo/llm: empty model")
	}

	provider := cfg.Provider
	if !cfg.ProviderSet {
		p, err := mcllm.DetectProvider(cfg.Model)
		if err != nil {
			return nil, err
		}
		provider = p
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultChatEndpoint(provider)
	}

	// Function calling is wired for the chat-completions shape only. The
	// gateway path always resolves to ${GatewayURL}/v1/chat/completions, so
	// only the direct path needs the shape guard.
	if cfg.GatewayURL == "" {
		if shape := mcllm.DetectAPIShape(endpoint); shape != mcllm.ShapeChatCompletions {
			return nil, fmt.Errorf("neo/llm: endpoint %q resolves to %s shape; v1 function calling supports chat-completions only", endpoint, shape)
		}
	}

	apiKey := cfg.APIKey
	if apiKey == "" && cfg.GatewayURL == "" {
		k, err := envKey(provider)
		if err != nil {
			return nil, err
		}
		apiKey = k
	}

	maxTok := cfg.MaxTokens
	if maxTok == 0 {
		maxTok = 4096
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}

	return &Client{
		model:           cfg.Model,
		provider:        provider,
		endpoint:        endpoint,
		apiKey:          apiKey,
		gatewayURL:      cfg.GatewayURL,
		gatewayTokenEnv: cfg.GatewayTokenEnv,
		actorDID:        cfg.ActorDID,
		intentID:        cfg.IntentID,
		slotLabel:       cfg.SlotLabel,
		temperature:     cfg.Temperature,
		maxTokens:       maxTok,
		seed:            cfg.Seed,
		httpClient:      &http.Client{Timeout: timeout},
	}, nil
}

// Model returns the configured model id.
func (c *Client) Model() string { return c.model }

// Chat performs one round-trip: sends the message list (+ optional tool
// schemas) and returns the model's single assistant turn, which may contain
// tool_calls the caller must execute and feed back as tool-role messages.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	wire := chatRequestWire{
		Model:       c.model,
		Messages:    toWireMessages(req.Messages),
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
		Tools:       req.Tools,
	}
	if c.seed != 0 {
		s := c.seed
		wire.Seed = &s
	}
	if len(req.Tools) > 0 {
		if req.ToolChoice != "" {
			wire.ToolChoice = req.ToolChoice
		} else {
			wire.ToolChoice = "auto"
		}
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("neo/llm: marshal request: %w", err)
	}

	httpReq, err := c.newHTTPRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("neo/llm: %s POST: %w", c.provider, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("neo/llm: %s read body: %w", c.provider, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed chatResponseWire
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return nil, fmt.Errorf("neo/llm: %s http %d: %s (type=%s)", c.provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return nil, fmt.Errorf("neo/llm: %s http %d: %s", c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	var parsed chatResponseWire
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("neo/llm: %s parse response: %w", c.provider, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("neo/llm: %s api error: %s", c.provider, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("neo/llm: empty choices in response")
	}

	choice := parsed.Choices[0]
	res := &ChatResult{
		Message:      fromWireRespMessage(choice.Message),
		FinishReason: choice.FinishReason,
	}
	if parsed.Usage != nil {
		res.Usage = *parsed.Usage
	}
	return res, nil
}

// newHTTPRequest builds the POST. Mirrors matrix/mcl/llm's gateway posture:
// when gatewayURL is set the call is rewritten to
// ${gatewayURL}/v1/chat/completions with a gateway bearer + X-Matrix-*
// metadata; otherwise it goes direct to the provider with Bearer <apiKey>.
func (c *Client) newHTTPRequest(ctx context.Context, body []byte) (*http.Request, error) {
	url := c.endpoint
	if c.gatewayURL != "" {
		url = strings.TrimRight(c.gatewayURL, "/") + "/v1/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("neo/llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if c.gatewayURL == "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		return req, nil
	}

	tokenEnv := c.gatewayTokenEnv
	if tokenEnv == "" {
		tokenEnv = "MATRIX_GATEWAY_TOKEN"
	}
	if tok := os.Getenv(tokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if c.actorDID != "" {
		req.Header.Set("X-Matrix-Actor-DID", c.actorDID)
	}
	if c.intentID != "" {
		req.Header.Set("X-Matrix-Intent-ID", c.intentID)
	}
	if c.slotLabel != "" {
		req.Header.Set("X-Matrix-Slot", c.slotLabel)
	}
	return req, nil
}

func defaultChatEndpoint(p mcllm.Provider) string {
	switch p {
	case mcllm.ProviderTogether:
		return "https://api.together.xyz/v1/chat/completions"
	case mcllm.ProviderFireworks:
		return "https://api.fireworks.ai/inference/v1/chat/completions"
	case mcllm.ProviderOpencode:
		return mcllm.OpencodeChatEndpoint
	}
	return ""
}

func envKey(p mcllm.Provider) (string, error) {
	var name string
	switch p {
	case mcllm.ProviderTogether:
		name = "TOGETHER_API_KEY"
	case mcllm.ProviderFireworks:
		name = "FIREWORKS_API_KEY"
	case mcllm.ProviderOpencode:
		name = "OPENCODE_API_KEY"
	default:
		return "", fmt.Errorf("neo/llm: unknown provider %d", p)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("neo/llm: %s is not set", name)
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- wire types (OpenAI chat-completions with tools) ---

type chatRequestWire struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Seed        *int64        `json:"seed,omitempty"`
	Tools       []Tool        `json:"tools,omitempty"`
	ToolChoice  interface{}   `json:"tool_choice,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// wireMessage is the request-side message shape. It deliberately omits the
// reasoning channel so prior assistant reasoning is never echoed back.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

func toWireMessages(msgs []Message) []wireMessage {
	out := make([]wireMessage, len(msgs))
	for i, m := range msgs {
		out[i] = wireMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
	}
	return out
}

type chatResponseWire struct {
	Choices []struct {
		Index        int             `json:"index"`
		Message      wireRespMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage         `json:"usage,omitempty"`
	Error *chatErrorBody `json:"error,omitempty"`
}

// wireRespMessage is the response-side assistant message; content may be JSON
// null when the model only emitted tool_calls (Go leaves Content "").
type wireRespMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content"`
	ToolCalls        []ToolCall `json:"tool_calls"`
}

func fromWireRespMessage(m wireRespMessage) Message {
	role := m.Role
	if role == "" {
		role = RoleAssistant
	}
	return Message{
		Role:      role,
		Content:   m.Content,
		ToolCalls: m.ToolCalls,
		Reasoning: m.ReasoningContent,
	}
}

type chatErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}
