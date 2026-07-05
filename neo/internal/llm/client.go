// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

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
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	mcllm "matrix/mcl/llm"
)

// ErrRequestTooLarge marks a provider reject where the request body exceeded
// the provider's hard byte cap (e.g. Fireworks HTTP 413 "body_too_large",
// 1,048,576 bytes). It is recoverable: the caller can compact the window and
// retry. Detect with errors.Is(err, llm.ErrRequestTooLarge).
var ErrRequestTooLarge = errors.New("neo/llm: request body too large")

// ErrProviderRejected marks a deterministic 4xx validation reject (e.g. a 400
// "InvalidParameter" on the function/tool fields). It is NOT recoverable by
// retrying the identical body — the request shape itself is bad — so callers
// must stop the retry ladder when they see it instead of burning calls (and,
// on a metered gateway, real spend) re-sending the same rejected request.
// Detect with errors.Is(err, llm.ErrProviderRejected).
var ErrProviderRejected = errors.New("neo/llm: provider rejected request (non-retryable)")

// ErrRateLimited marks an upstream 429 (the provider/gateway is rate-limiting
// or overloaded). It IS retryable, but callers should back off HARD rather than
// respawn immediately: a persistent 429 that is retried aggressively across many
// daemons becomes a self-amplifying request storm (the exact failure mode that
// turned "Baseten is slow" into "20 runaway Neos"). The task supervisor reads
// this to lengthen its respawn backoff. Detect with errors.Is(err, llm.ErrRateLimited).
var ErrRateLimited = errors.New("neo/llm: provider rate limited (429)")

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

	temperature    float64
	maxTokens      int
	seed           int64
	enableThinking bool

	httpClient *http.Client
}

// New builds a function-calling client from an mcl/llm.Config.
//
// v1 supports the chat-completions API shape only (Baseten / Together /
// Fireworks / opencode-chat). Models that route to the Anthropic Messages or
// OpenAI Responses shapes use different tool schemas and are rejected with a
// clear error; the default model registry pins Baseten chat-completions.
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
		enableThinking:  cfg.EnableThinking,
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
		// Stream unconditionally: some providers (e.g. Together's Qwen3.7-Max)
		// ONLY accept stream=true and 400 otherwise, and the gateway prefers
		// streaming so it can scan usage from the final chunk for metering.
		// Both Fireworks + Together emit the same OpenAI SSE shape, which
		// aggregateStream below folds back into a single assistant turn.
		Stream: true,
	}
	// On the direct path we ask for a usage chunk ourselves; on the gateway
	// path the gateway injects stream_options.include_usage for us (and may
	// rewrite the body), so leave it untouched to avoid a double-insert.
	if c.gatewayURL == "" {
		wire.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	// xAI's grok-4.3 takes the OpenAI-style `reasoning_effort` control — pin it
	// explicitly from the per-client flag ("high" when set, "none" when clear)
	// so token-tight roles keep reasoning off. Kimi (moonshotai/*, Baseten)
	// keeps the legacy opt-in chat_template_args.enable_thinking.
	if mcllm.IsXaiModel(c.model) && supportsReasoningEffort(c.model) {
		wire.ReasoningEffort = reasoningEffort(c.enableThinking)
	} else if c.enableThinking {
		if args := enableThinkingArgs(c.model); args != nil {
			wire.ChatTemplateArgs = args
		}
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
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("neo/llm: %s POST: %w", c.provider, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var parsed chatResponseWire
		_ = json.Unmarshal(respBody, &parsed)
		errMsg, errType := "", ""
		if parsed.Error != nil {
			errMsg, errType = parsed.Error.Message, parsed.Error.Type
		}
		// Body-size reject (recoverable: the caller compacts + retries).
		if resp.StatusCode == http.StatusRequestEntityTooLarge ||
			errType == "body_too_large" ||
			strings.Contains(errMsg, "body_too_large") ||
			strings.Contains(errMsg, "body exceeds") {
			detail := errMsg
			if detail == "" {
				detail = truncate(string(respBody), 256)
			}
			return nil, fmt.Errorf("%w (%s http %d): %s", ErrRequestTooLarge, c.provider, resp.StatusCode, detail)
		}
		// Deterministic 4xx validation rejects (e.g. a 400 "InvalidParameter"
		// on the function/tool fields) are non-retryable: the body itself is
		// bad, so the retry ladder must stop rather than re-send it. 408
		// (timeout) and 429 (rate limit) stay retryable; 413 was handled above.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
			resp.StatusCode != http.StatusRequestTimeout &&
			resp.StatusCode != http.StatusTooManyRequests {
			detail := errMsg
			if detail == "" {
				detail = truncate(string(respBody), 256)
			}
			return nil, fmt.Errorf("%w (%s http %d): %s", ErrProviderRejected, c.provider, resp.StatusCode, detail)
		}
		// Upstream 429: retryable, but marked with a sentinel so the retry
		// ladder + task supervisor back off HARD instead of amplifying an
		// overloaded provider into a request storm.
		if resp.StatusCode == http.StatusTooManyRequests {
			detail := errMsg
			if detail == "" {
				detail = truncate(string(respBody), 256)
			}
			return nil, fmt.Errorf("%w (%s): %s", ErrRateLimited, c.provider, detail)
		}
		if errMsg != "" {
			return nil, fmt.Errorf("neo/llm: %s http %d: %s (type=%s)", c.provider, resp.StatusCode, errMsg, errType)
		}
		return nil, fmt.Errorf("neo/llm: %s http %d: %s", c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	// 2xx: fold the SSE stream into one assistant turn, delivering coalesced,
	// pre-cleaned incremental fragments to req.OnDelta (when set) as the bytes
	// arrive off the wire — real token-by-token streaming, not a post-hoc
	// replay of a buffered body. With OnDelta nil this is a pure parse.
	msg, finish, usage, err := aggregateStreamReader(resp.Body, req.OnDelta)
	if err != nil {
		return nil, fmt.Errorf("neo/llm: %s parse stream: %w", c.provider, err)
	}
	res := &ChatResult{
		Message:      fromWireRespMessage(msg),
		FinishReason: finish,
	}
	if usage != nil {
		res.Usage = *usage
	}
	return res, nil
}

// aggregateStream reconstructs a single assistant turn from a fully-buffered
// OpenAI-style SSE chat-completions stream. It is a thin wrapper over
// aggregateStreamReader with no incremental delivery — kept for callers/tests
// that hold the whole body in memory.
func aggregateStream(body []byte) (wireRespMessage, string, *Usage, error) {
	return aggregateStreamReader(bytes.NewReader(body), nil)
}

// deltaFlushChars is the coalescing threshold for live streaming: incremental
// fragments are buffered until at least this many new visible/reasoning chars
// have accumulated, then flushed via onDelta. Small enough for a smooth
// "typing" cadence, large enough to keep the SSE event volume sane (one event
// per ~handful of tokens rather than one per token).
const deltaFlushChars = 18

// aggregateStreamReader reconstructs a single assistant turn from an
// OpenAI-style SSE chat-completions stream read incrementally off r (the
// response shape Together requires for some models; Fireworks + the gateway
// emit the same shape). It concatenates content + reasoning_content deltas,
// merges tool_call argument fragments by index, and captures the terminal
// finish_reason + usage.
//
// When onDelta is non-nil it ALSO delivers coalesced, pre-cleaned incremental
// fragments as the bytes arrive — true token-by-token streaming. Visible
// content fragments are run through the same normalization the folded turn
// gets (leading <think> blocks and the Kimi tool-call token grammar are
// withheld) plus a partial-marker holdback, so a consumer can render them
// verbatim without leaking internal monologue or control tokens. The folded
// message is still handed to fromWireRespMessage for the authoritative final
// cleaning.
func aggregateStreamReader(r io.Reader, onDelta func(Delta)) (wireRespMessage, string, *Usage, error) {
	msg := wireRespMessage{Role: RoleAssistant}
	var content, reasoning strings.Builder
	type aggCall struct {
		id, typ, name string
		args          strings.Builder
	}
	calls := map[int]*aggCall{}
	var order []int
	finish := ""
	var usage *Usage
	sawData := false

	// Live-streaming cursors: how many cleaned visible / reasoning chars have
	// already been delivered via onDelta. flush emits everything past them,
	// gated by the coalescing threshold (or forced at end of stream).
	var contentEmitted, reasoningEmitted int
	flush := func(force bool) {
		if onDelta == nil {
			return
		}
		var cFrag, rFrag string
		if vis := holdbackPartial(previewVisible(content.String())); len(vis) > contentEmitted {
			if cand := vis[contentEmitted:]; force || len(cand) >= deltaFlushChars {
				cFrag = cand
			}
		}
		if rs := reasoning.String(); len(rs) > reasoningEmitted {
			if cand := rs[reasoningEmitted:]; force || len(cand) >= deltaFlushChars {
				rFrag = cand
			}
		}
		if cFrag == "" && rFrag == "" {
			return
		}
		onDelta(Delta{Content: cFrag, Reasoning: rFrag})
		contentEmitted += len(cFrag)
		reasoningEmitted += len(rFrag)
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Tolerate keepalive/comment lines rather than failing the turn.
			continue
		}
		sawData = true
		if chunk.Error != nil && chunk.Error.Message != "" {
			return msg, finish, usage, fmt.Errorf("%s (type=%s)", chunk.Error.Message, chunk.Error.Type)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Role != "" {
				msg.Role = ch.Delta.Role
			}
			content.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.ReasoningContent)
			for _, tc := range ch.Delta.ToolCalls {
				ac := calls[tc.Index]
				if ac == nil {
					ac = &aggCall{}
					calls[tc.Index] = ac
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					ac.id = tc.ID
				}
				if tc.Type != "" {
					ac.typ = tc.Type
				}
				if tc.Function.Name != "" {
					ac.name = tc.Function.Name
				}
				ac.args.WriteString(tc.Function.Arguments)
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
		flush(false)
	}
	if err := sc.Err(); err != nil {
		return msg, finish, usage, err
	}
	if !sawData {
		return msg, finish, usage, fmt.Errorf("empty stream")
	}
	flush(true)

	msg.Content = content.String()
	msg.ReasoningContent = reasoning.String()
	sort.Ints(order)
	for _, idx := range order {
		ac := calls[idx]
		if ac.name == "" {
			continue
		}
		typ := ac.typ
		if typ == "" {
			typ = "function"
		}
		id := ac.id
		if id == "" {
			id = "call_" + strconv.Itoa(idx)
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:       id,
			Type:     typ,
			Function: FunctionCall{Name: ac.name, Arguments: ac.args.String()},
		})
	}
	return msg, finish, usage, nil
}

// previewVisible returns the portion of an in-progress content buffer that is
// safe to show as the visible answer so far: a leading <think>/<thinking>
// block is moved out (held back until it closes) and any Kimi tool-call token
// grammar is stripped. It reuses the exact helpers the final fold uses, so the
// streamed prefix never diverges from the authoritative cleaned content.
func previewVisible(content string) string {
	v, _ := splitInlineThink(content)
	if stripped, extracted := extractTokenToolCalls(v); len(extracted) > 0 {
		v = stripped
	}
	return v
}

// holdbackPartial withholds a trailing fragment that could be the start of a
// control marker (<think>, <thinking>, or the Kimi tool-call section/call
// tokens) so a half-arrived marker is never streamed as visible text. A normal
// '<' in prose (e.g. "a < b") is left intact — only a tail that is a genuine
// prefix of a known marker is held back, to be resolved once more bytes arrive.
func holdbackPartial(s string) string {
	i := strings.LastIndex(s, "<")
	if i < 0 {
		return s
	}
	tail := s[i:]
	if strings.Contains(tail, ">") {
		return s
	}
	for _, m := range [...]string{"<think>", "<thinking>", kimiSectionBegin, kimiCallBegin} {
		if strings.HasPrefix(m, tail) {
			return s[:i]
		}
	}
	return s
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
	case mcllm.ProviderBaseten:
		return "https://inference.baseten.co/v1/chat/completions"
	case mcllm.ProviderXai:
		return mcllm.XaiChatEndpoint
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
	case mcllm.ProviderBaseten:
		name = "BASETEN_API_KEY"
	case mcllm.ProviderXai:
		name = "XAI_API_KEY"
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

// supportsReasoningEffort reports whether an xAI model accepts the
// `reasoning_effort` request field. Per xAI docs it is currently only honored
// by grok-4.3; other Grok models (grok-build-*, grok-4.20-*-non-reasoning)
// ignore or reject it, so the field is omitted for them.
func supportsReasoningEffort(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "grok-4.3")
}

// reasoningEffort maps the per-role EnableThinking flag onto xAI's
// reasoning_effort enum: reasoning ON → "high", OFF → "none" (disables
// reasoning entirely for token-tight mechanical roles).
func reasoningEffort(enabled bool) string {
	if enabled {
		return "high"
	}
	return "none"
}

// enableThinkingArgs returns the chat_template_args that turn reasoning ON for
// Baseten-served models where it is OPT-IN: Kimi (moonshotai/*) emits its
// chain-of-thought as visible `content` unless enable_thinking makes the
// server split it into the separate reasoning_content channel. Returns nil for
// models that reason by default or use a different provider param (never sent).
func enableThinkingArgs(model string) map[string]any {
	if strings.HasPrefix(strings.ToLower(model), "moonshotai/") {
		return map[string]any{"enable_thinking": true}
	}
	return nil
}

// --- wire types (OpenAI chat-completions with tools) ---

type chatRequestWire struct {
	Model            string         `json:"model"`
	Messages         []wireMessage  `json:"messages"`
	Temperature      float64        `json:"temperature"`
	MaxTokens        int            `json:"max_tokens,omitempty"`
	Seed             *int64         `json:"seed,omitempty"`
	Tools            []Tool         `json:"tools,omitempty"`
	ToolChoice       interface{}    `json:"tool_choice,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
	StreamOptions    *streamOptions `json:"stream_options,omitempty"`
	ChatTemplateArgs map[string]any `json:"chat_template_args,omitempty"`
	// ReasoningEffort is xAI's reasoning-depth control ("none"|"low"|"medium"|
	// "high"); set ONLY for grok-4.3 (omitempty keeps every other model's wire
	// shape unchanged).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// streamOptions asks the provider to emit a final usage chunk on the SSE
// stream (OpenAI `stream_options.include_usage`). Used on the direct path; the
// gateway injects its own on the metered path.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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
			ToolCalls:  sanitizeToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
	}
	return out
}

// sanitizeToolCalls guarantees every REPLAYED assistant tool_call carries a
// well-formed function before it goes back on the wire. A truncated or
// malformed generation can leave function.arguments empty or non-JSON; the
// transcript is re-sent verbatim every turn, so replaying such a call makes a
// strict provider (e.g. Together's DeepSeek-V4-Pro) reject the WHOLE request
// with a 400 on the function field — which permanently wedges the conversation
// (every later turn carries the same poison). Coercing empty/invalid arguments
// to "{}" keeps the call structurally valid AND preserves the assistant↔
// tool_call_id pairing (so the matching tool-result is never orphaned). The
// agent already recorded the real parse error as that tool's result, so the
// model still gets the feedback it needs to re-issue the call correctly.
func sanitizeToolCalls(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return calls
	}
	out := make([]ToolCall, len(calls))
	for i, tc := range calls {
		if tc.Type == "" {
			tc.Type = "function"
		}
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" || !json.Valid([]byte(args)) {
			tc.Function.Arguments = "{}"
		}
		out[i] = tc
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

// chatStreamChunk is one SSE `data:` frame of a streaming chat-completion. The
// delta carries incremental content/reasoning and tool_call fragments (id +
// name arrive once, arguments stream across frames keyed by index).
type chatStreamChunk struct {
	Choices []struct {
		Index        int         `json:"index"`
		Delta        streamDelta `json:"delta"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage         `json:"usage,omitempty"`
	Error *chatErrorBody `json:"error,omitempty"`
}

type streamDelta struct {
	Role             string            `json:"role"`
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content"`
	ToolCalls        []streamToolDelta `json:"tool_calls"`
}

type streamToolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
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
	content := m.Content
	toolCalls := m.ToolCalls
	// Defensive: Kimi/Moonshot K2-family models inline their tool calls in
	// `content` using a special-token grammar instead of the structured
	// `tool_calls` array when the upstream provider does not parse them
	// server-side. Left in place the whole section (control tokens + JSON
	// arguments) leaks into the chat as a "final answer". Extract them so they
	// execute as real calls. Only attempt this when no structured calls came
	// back, so a well-behaved provider response is never disturbed.
	if len(toolCalls) == 0 {
		if stripped, extracted := extractTokenToolCalls(content); len(extracted) > 0 {
			content = stripped
			toolCalls = extracted
		}
	}
	content, inlineReasoning := splitInlineThink(content)
	reasoning := m.ReasoningContent
	if reasoning == "" {
		reasoning = inlineReasoning
	}
	return Message{
		Role:      role,
		Content:   content,
		ToolCalls: toolCalls,
		Reasoning: reasoning,
	}
}

// Special-token grammar Kimi/Moonshot K2 models use to emit tool calls inside
// `content`. A section wraps one or more calls; each call is a
// `functions.<name>:<index>` header, an argument-begin marker, the JSON
// arguments, then a call-end marker.
const (
	kimiSectionBegin = "<|tool_calls_section_begin|>"
	kimiSectionEnd   = "<|tool_calls_section_end|>"
	kimiCallBegin    = "<|tool_call_begin|>"
	kimiArgBegin     = "<|tool_call_argument_begin|>"
	kimiCallEnd      = "<|tool_call_end|>"
)

// extractTokenToolCalls pulls any token-grammar tool calls out of content and
// returns the content with those spans removed plus the structured calls. It
// tolerates a missing section wrapper and an unterminated section (truncated
// generation): everything from the first marker onward is consumed so nothing
// leaks. Returns (content, nil) when there is nothing to extract.
func extractTokenToolCalls(content string) (string, []ToolCall) {
	hasSection := strings.Contains(content, kimiSectionBegin)
	hasCall := strings.Contains(content, kimiCallBegin)
	if !hasSection && !hasCall {
		return content, nil
	}
	if !hasSection {
		// Variant/truncated emission without the section wrapper: treat from
		// the first call marker onward as a single section.
		idx := strings.Index(content, kimiCallBegin)
		calls := parseTokenSection(content[idx:], 0)
		if len(calls) == 0 {
			return content, nil
		}
		return strings.TrimRight(content[:idx], " \t\r\n"), calls
	}

	var b strings.Builder
	var calls []ToolCall
	rest := content
	for {
		start := strings.Index(rest, kimiSectionBegin)
		if start < 0 {
			break
		}
		b.WriteString(rest[:start])
		after := rest[start+len(kimiSectionBegin):]
		end := strings.Index(after, kimiSectionEnd)
		if end < 0 {
			// Unterminated section (truncated): consume the remainder.
			calls = append(calls, parseTokenSection(after, len(calls))...)
			rest = ""
			break
		}
		calls = append(calls, parseTokenSection(after[:end], len(calls))...)
		rest = after[end+len(kimiSectionEnd):]
	}
	b.WriteString(rest)
	if len(calls) == 0 {
		return content, nil
	}
	return strings.TrimSpace(b.String()), calls
}

// parseTokenSection decodes the calls inside one token-grammar section. base is
// the running call count, used to synthesize stable call IDs.
func parseTokenSection(section string, base int) []ToolCall {
	var calls []ToolCall
	for _, part := range strings.Split(section, kimiCallBegin) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		header, args := part, ""
		if i := strings.Index(part, kimiArgBegin); i >= 0 {
			header = part[:i]
			args = part[i+len(kimiArgBegin):]
		}
		if j := strings.Index(args, kimiCallEnd); j >= 0 {
			args = args[:j]
		}
		header = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(header), kimiCallEnd))
		name := tokenFuncName(header)
		if name == "" {
			continue
		}
		calls = append(calls, ToolCall{
			ID:       "call_" + strconv.Itoa(base+len(calls)),
			Type:     "function",
			Function: FunctionCall{Name: name, Arguments: strings.TrimSpace(args)},
		})
	}
	return calls
}

// tokenFuncName turns a `functions.<name>:<index>` header into the bare tool
// name. The `functions.` prefix and a trailing `:<int>` index are both
// optional in observed emissions.
func tokenFuncName(header string) string {
	h := strings.TrimSpace(header)
	h = strings.TrimPrefix(h, "functions.")
	if i := strings.LastIndex(h, ":"); i >= 0 {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			h = h[:i]
		}
	}
	return strings.TrimSpace(h)
}

// splitInlineThink moves a provider-inlined chain-of-thought block out of the
// visible content and into the reasoning channel. Some providers/models emit
// `<think>…</think>` (or `<thinking>…</thinking>`) inside `content` instead of
// the separate `reasoning_content` field; leaving it in place leaks internal
// monologue into the chat. An unterminated opening tag (truncated generation)
// means the WHOLE remainder is reasoning.
func splitInlineThink(content string) (visible, reasoning string) {
	trimmed := strings.TrimLeft(content, " \t\r\n")
	for _, tag := range [2]string{"think", "thinking"} {
		open, close := "<"+tag+">", "</"+tag+">"
		if !strings.HasPrefix(trimmed, open) {
			continue
		}
		rest := trimmed[len(open):]
		if i := strings.Index(rest, close); i >= 0 {
			return strings.TrimSpace(rest[i+len(close):]), strings.TrimSpace(rest[:i])
		}
		return "", strings.TrimSpace(rest)
	}
	return content, ""
}

type chatErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}
