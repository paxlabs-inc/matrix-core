// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package llm implements the interpreter.LLM interface over OpenAI-compatible
// chat-completions endpoints.
//
// Supported providers:
//   - Together AI (api.together.xyz)
//   - Fireworks AI (api.fireworks.ai)
//
// Grammar-constrained decoding is supported via response_format JSON schema
// (both providers) or grammar EBNF (Fireworks-specific). This enables the D18
// compiler model slot to physically emit only well-formed JSON matching the
// Intent IR schema.
//
// Determinism (D11): Seed + temperature=0 ensures same input → same output
// on seedable models. The seed is passed through to the provider API.
//
// stdlib-only; no third-party SDK.
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
	"strings"
	"time"

	"matrix/mcl/mtx/interpreter"
)

// Provider identifies the API backend.
type Provider int

const (
	ProviderTogether  Provider = iota // api.together.xyz
	ProviderFireworks                 // api.fireworks.ai
	ProviderOpencode                  // opencode.ai/zen (Session 34 / Forge)
)

func (p Provider) String() string {
	switch p {
	case ProviderTogether:
		return "together"
	case ProviderFireworks:
		return "fireworks"
	case ProviderOpencode:
		return "opencode"
	}
	return "unknown"
}

// APIShape identifies the on-wire API contract for a given endpoint. Three
// shapes are supported as of Session 34:
//
//   - ShapeChatCompletions — OpenAI /v1/chat/completions (existing legacy path;
//     Together + Fireworks + opencode openai-compatible models).
//   - ShapeMessages        — Anthropic Messages API at /v1/messages (Claude
//     models routed through opencode.ai/zen/v1/messages).
//   - ShapeResponses       — OpenAI Responses API at /v1/responses (GPT-5+
//     models routed through opencode.ai/zen/v1/responses).
//
// APIShape is derived from the endpoint URL suffix at New() time; callers
// rarely set it explicitly. ShapeUnknown is reserved as the zero value and
// triggers fallback to ShapeChatCompletions for backwards-compat.
type APIShape int

const (
	ShapeUnknown         APIShape = iota
	ShapeChatCompletions          // /v1/chat/completions
	ShapeMessages                 // /v1/messages
	ShapeResponses                // /v1/responses
)

func (s APIShape) String() string {
	switch s {
	case ShapeChatCompletions:
		return "chat_completions"
	case ShapeMessages:
		return "messages"
	case ShapeResponses:
		return "responses"
	}
	return "unknown"
}

// DetectAPIShape inspects an endpoint URL suffix and returns the matching
// APIShape. Pure (no allocations beyond the trim).
func DetectAPIShape(endpoint string) APIShape {
	ep := strings.ToLower(strings.TrimRight(endpoint, "/"))
	switch {
	case strings.HasSuffix(ep, "/v1/messages"):
		return ShapeMessages
	case strings.HasSuffix(ep, "/v1/responses"):
		return ShapeResponses
	case strings.HasSuffix(ep, "/v1/chat/completions"),
		strings.HasSuffix(ep, "/chat/completions"):
		return ShapeChatCompletions
	}
	return ShapeUnknown
}

// Config holds the runtime configuration for an LLM client.
type Config struct {
	// Model is the provider-specific model identifier.
	// Together: "deepseek-ai/DeepSeek-V4-Flash", "openai/gpt-oss-120b", etc.
	// Fireworks: "accounts/fireworks/models/deepseek-v4-flash", etc.
	Model string

	// Provider overrides auto-detection from the model string.
	// If zero-value, DetectProvider(Model) is used.
	Provider Provider

	// ProviderSet indicates whether Provider was explicitly set.
	ProviderSet bool

	// APIKey overrides the environment variable lookup.
	// If empty, the provider's standard env var is read.
	APIKey string

	// Endpoint overrides the provider's default endpoint.
	// Useful for local vLLM or custom deployments.
	Endpoint string

	// Temperature for generation. 0 = deterministic (compiler slot default).
	Temperature float64

	// Seed for reproducible generation (D11). 0 means no seed param sent.
	Seed int64

	// MaxTokens caps output length. Default 4096.
	MaxTokens int

	// Timeout for HTTP requests. Default 90s.
	Timeout time.Duration

	// GrammarMode controls how grammar constraints are passed to the provider.
	GrammarMode GrammarMode

	// Grammars maps grammar IDs (e.g. "intent_frame@1") to their schema/EBNF.
	// The interpreter passes the grammar string to Decode; this map resolves it
	// to the actual constraint payload sent to the provider.
	Grammars map[string]*GrammarDef

	// --- sess#32 ambient-architect MatrixGateway routing (plan §5.16) ---
	//
	// When GatewayURL != "", Decode/Stream redirect every chat-completions
	// POST through the gateway: URL becomes ${GatewayURL}/v1/chat/completions,
	// Authorization carries Bearer ${MATRIX_GATEWAY_TOKEN} (env var name is
	// overridable via GatewayTokenEnv), and the X-Matrix-* metadata headers
	// below are stamped so the gateway can authenticate the actor, route
	// the call against per-slot whitelists + the credit ledger, and stream
	// back X-Matrix-Cost-Pax / Daily-Spent / Daily-Remaining trailers.
	//
	// Empty GatewayURL preserves the legacy direct-provider posture
	// verbatim: APIKey + Endpoint + Bearer <APIKey> exactly as before.
	// All seven fields below are optional and zero-value-clean so a
	// shared config produced before sess#32 still flows.

	// GatewayURL is the host portion of the gateway (no trailing slash);
	// the client appends "/v1/chat/completions" for both Decode + Stream.
	GatewayURL string

	// GatewayTokenEnv overrides the env var name read for the gateway
	// bearer token. Default "MATRIX_GATEWAY_TOKEN".
	GatewayTokenEnv string

	// ActorDID is sent as X-Matrix-Actor-DID. Required when GatewayURL
	// is set in production; tests may leave empty (gateway then 401s).
	ActorDID string

	// IntentID is sent as X-Matrix-Intent-ID so the gateway's audit
	// trail + per-intent cost rollups join cleanly with the daemon's
	// transcript stream.
	IntentID string

	// GoalID is sent as X-Matrix-Goal-ID — optional; populated when
	// the daemon knows the goal (chat-driven runs vs. one-shot CLI).
	GoalID string

	// SlotLabel is sent as X-Matrix-Slot. Free-tier whitelist
	// enforcement on the gateway hinges on this value: "compiler",
	// "planner", "executor", "attest", etc.
	SlotLabel string

	// KindRoute is sent as X-Matrix-Kind-Route. Executor-only sub-
	// route ("reason" | "code" | "classify" | …); compiler/planner
	// callers leave empty.
	KindRoute string

	// OnResponseHeaders, when non-nil, is invoked exactly once per
	// successful AND failed Decode/Stream HTTP round-trip with the
	// raw http.Response.Header. Daemons hook this to capture the
	// gateway's X-Matrix-Cost-Pax / Daily-Spent / Daily-Remaining
	// trailers without having to wrap the LLM call sites. Panics in
	// the callback are swallowed to keep Decode/Stream's contract
	// intact.
	OnResponseHeaders func(http.Header)

	// --- Session 34 (Forge Phase 1) — identity preamble + opencode dispatch ---

	// InjectIdentity, when true, prepends llm.IdentityPreamble as the
	// FIRST system message at every Decode + Stream call. The Forge
	// self-maintenance posture (matrix.kvx sess#34) requires every
	// frontier-model invocation to be reminded that it IS Matrix and
	// its sole purpose is to optimize the codebase at /root/matrix.
	//
	// Defaults to false (legacy paths preserved byte-identically).
	// ForgeRegistry() in model.go sets this to true on every route.
	InjectIdentity bool

	// Shape forces a specific APIShape regardless of the endpoint URL.
	// Zero value (ShapeUnknown) means New() derives the shape from
	// cfg.Endpoint via DetectAPIShape, falling back to chat-completions
	// when the suffix is unrecognized. Callers rarely set this — useful
	// only for tests pointing at a custom mock that doesn't match the
	// real provider URL.
	Shape APIShape
}

// GrammarMode selects how grammar constraints are communicated to the provider.
type GrammarMode int

const (
	// GrammarNone sends no grammar constraint (free-form generation).
	GrammarNone GrammarMode = iota

	// GrammarJSONSchema uses response_format with a JSON schema.
	// Supported by both Together and Fireworks.
	GrammarJSONSchema

	// GrammarEBNF uses Fireworks-specific EBNF grammar constraint.
	// Only works with Fireworks models.
	GrammarEBNF
)

// GrammarDef defines a grammar constraint that can be sent to the provider.
type GrammarDef struct {
	// JSONSchema is the JSON schema object (for GrammarJSONSchema mode).
	JSONSchema map[string]interface{}

	// EBNF is the EBNF grammar string (for GrammarEBNF mode).
	EBNF string

	// Name is a human-readable name for the schema (used in response_format).
	Name string
}

// Client implements interpreter.LLM over an OpenAI-compatible
// /v1/chat/completions endpoint. Returned by New() when the endpoint
// resolves to ShapeChatCompletions (the legacy path, Together + Fireworks
// + opencode openai-compatible models).
//
// For ShapeMessages (Anthropic) New() returns *messagesClient; for
// ShapeResponses (OpenAI Responses) New() returns *responsesClient. All
// three implement interpreter.LLM + interpreter.StreamingLLM so callers
// can keep using the interface contract without caring about the wire
// format.
type Client struct {
	cfg        Config
	httpClient *http.Client
	provider   Provider
	endpoint   string
	apiKey     string
}

// Ensure Client implements interpreter.LLM at compile time.
var _ interpreter.LLM = (*Client)(nil)

// And the streaming capability.
var _ interpreter.StreamingLLM = (*Client)(nil)

// New creates an LLM client for the configured endpoint shape.
//
// Dispatch (Session 34 / Forge Phase 1):
//
//	cfg.Endpoint suffix → APIShape   → returned concrete type
//	/v1/chat/completions  Chat        *Client
//	/v1/messages          Messages    *messagesClient (Anthropic Messages API)
//	/v1/responses         Responses   *responsesClient (OpenAI Responses API)
//
// Falls back to ShapeChatCompletions when the suffix is unrecognized,
// preserving the legacy contract verbatim (Together + Fireworks paths
// pre-sess#34 keep working with no caller changes).
//
// Returns an interpreter.LLM (the streaming capability is opt-in via
// type assertion to interpreter.StreamingLLM, which all three concrete
// types implement).
//
// The provided *Config is copied into the returned client and may be
// reused or discarded by the caller after the call returns.
func New(cfg *Config) (interpreter.LLM, error) {
	if cfg == nil {
		return nil, fmt.Errorf("llm.New: nil config")
	}
	local := *cfg

	// Resolve provider
	var provider Provider
	if local.ProviderSet {
		provider = local.Provider
	} else {
		p, err := DetectProvider(local.Model)
		if err != nil {
			return nil, err
		}
		provider = p
	}

	// Resolve endpoint
	endpoint := local.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint(provider)
	}

	// Resolve API key
	apiKey := local.APIKey
	if apiKey == "" {
		key, err := envKey(provider)
		if err != nil {
			return nil, err
		}
		apiKey = key
	}

	// Defaults
	if local.MaxTokens == 0 {
		local.MaxTokens = 4096
	}
	if local.Timeout == 0 {
		local.Timeout = 90 * time.Second
	}

	// Resolve API shape: explicit cfg.Shape wins; otherwise derive from
	// endpoint URL suffix; fall back to chat-completions for unknown
	// suffixes (legacy compat).
	shape := local.Shape
	if shape == ShapeUnknown {
		shape = DetectAPIShape(endpoint)
		if shape == ShapeUnknown {
			shape = ShapeChatCompletions
		}
	}

	httpClient := &http.Client{Timeout: local.Timeout}

	switch shape {
	case ShapeMessages:
		return newMessagesClient(local, httpClient, provider, endpoint, apiKey), nil
	case ShapeResponses:
		return newResponsesClient(local, httpClient, provider, endpoint, apiKey), nil
	}

	return &Client{
		cfg:        local,
		httpClient: httpClient,
		provider:   provider,
		endpoint:   endpoint,
		apiKey:     apiKey,
	}, nil
}

// NewChatClient is a typed-return constructor preserved for tests that need
// the concrete *Client to introspect endpoint/provider/apiKey fields. New
// production code should use New() which returns interpreter.LLM.
func NewChatClient(cfg *Config) (*Client, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	cc, ok := c.(*Client)
	if !ok {
		return nil, fmt.Errorf("llm.NewChatClient: endpoint %q resolved to %s shape (expected chat_completions)",
			cfg.Endpoint, DetectAPIShape(cfg.Endpoint))
	}
	return cc, nil
}

// Decode implements interpreter.LLM. It sends messages to the configured model
// with an optional grammar constraint and returns the raw text output.
//
// When cfg.InjectIdentity is true (Session 34 / Forge Phase 1), IdentityPreamble
// is prepended as the FIRST system message before serialisation. Legacy paths
// (cfg.InjectIdentity == false) preserve their pre-sess#34 wire bytes byte-
// identically.
func (c *Client) Decode(ctx context.Context, messages []interpreter.Message, grammar string) (string, error) {
	msg, err := c.decodeMessage(ctx, messages, grammar)
	if err != nil {
		return "", err
	}
	return msg.Content, nil
}

// DecodeWithReasoning behaves exactly like Decode but additionally returns
// the model's reasoning_content (chain-of-thought) when the provider
// surfaces it as a separate field. Used by side-channel callers (the
// Liaison) that render reasoning as a DISTINCT, labelled channel — never
// as the answer. The replay-critical pipeline keeps using Decode, which
// is byte-for-byte unchanged.
func (c *Client) DecodeWithReasoning(ctx context.Context, messages []interpreter.Message, grammar string) (content, reasoning string, err error) {
	msg, err := c.decodeMessage(ctx, messages, grammar)
	if err != nil {
		return "", "", err
	}
	return msg.Content, msg.ReasoningContent, nil
}

// decodeMessage performs the one-shot chat request and returns the first
// choice's full message (content + reasoning_content). Decode and
// DecodeWithReasoning are thin wrappers so the wire bytes and behaviour
// of the existing Decode path are preserved exactly.
func (c *Client) decodeMessage(ctx context.Context, messages []interpreter.Message, grammar string) (chatMessage, error) {
	messages = maybeInjectIdentity(c.cfg, messages)
	req, err := c.buildRequest(messages, grammar)
	if err != nil {
		return chatMessage{}, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := c.newHTTPRequest(ctx, body, "application/json")
	if err != nil {
		return chatMessage{}, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return chatMessage{}, fmt.Errorf("llm: %s POST: %w", c.provider, err)
	}
	defer resp.Body.Close()
	c.dispatchHeaders(resp.Header)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatMessage{}, fmt.Errorf("llm: %s read body: %w", c.provider, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed chatResponse
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return chatMessage{}, fmt.Errorf("llm: %s http %d: %s (type=%s)",
				c.provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return chatMessage{}, fmt.Errorf("llm: %s http %d: %s", c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return chatMessage{}, fmt.Errorf("llm: %s parse response: %w", c.provider, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return chatMessage{}, fmt.Errorf("llm: %s api error: %s", c.provider, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return chatMessage{}, errors.New("llm: empty choices in response")
	}

	return parsed.Choices[0].Message, nil
}

// Stream implements interpreter.StreamingLLM. It sends messages to the
// configured model with stream=true and invokes onDelta synchronously
// for every incremental content fragment received over the SSE wire,
// returning the fully accumulated text on completion.
//
// Session 31c (model router · P3a). Both Together AI and Fireworks AI
// expose OpenAI-compatible streaming on /v1/chat/completions: each SSE
// frame is `data: <json>` carrying choices[0].delta.content; the stream
// is terminated by `data: [DONE]`. Grammar-constrained streaming is
// supported on Fireworks for json_schema/grammar modes; Together is
// per-model and may decline the schema. We forward whatever the caller
// passed through applyGrammar; the provider's response_format echo is
// passed through unchanged.
//
// Reliability:
//   - HTTP non-2xx → returns error with the body preview (mirrors Decode).
//   - The first SSE error frame (data: {"error": ...}) terminates the
//     stream with a wrapped error AND the partial text accumulated so far.
//   - Context cancellation interrupts the read loop and surfaces ctx.Err()
//     so callers can distinguish operator abort from provider error.
//   - onDelta == nil is allowed; the stream still drains and returns the
//     accumulated text (callers may use Stream for buffered-but-token-aware
//     decoding without wiring a delta sink).
//
// onDelta MUST NOT be invoked after Stream returns.
//
// When cfg.InjectIdentity is true (Session 34 / Forge Phase 1), IdentityPreamble
// is prepended as the FIRST system message before serialisation. Legacy paths
// preserve their pre-sess#34 wire bytes byte-identically.
func (c *Client) Stream(ctx context.Context, messages []interpreter.Message,
	grammar string, onDelta func(delta string)) (string, error) {

	messages = maybeInjectIdentity(c.cfg, messages)
	req, err := c.buildRequest(messages, grammar)
	if err != nil {
		return "", err
	}
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("llm: marshal stream request: %w", err)
	}

	httpReq, err := c.newHTTPRequest(ctx, body, "text/event-stream")
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("llm: %s stream POST: %w", c.provider, err)
	}
	defer resp.Body.Close()
	c.dispatchHeaders(resp.Header)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var parsed chatResponse
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("llm: %s stream http %d: %s (type=%s)",
				c.provider, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return "", fmt.Errorf("llm: %s stream http %d: %s",
			c.provider, resp.StatusCode, truncate(string(respBody), 512))
	}

	return parseSSEStream(ctx, resp.Body, c.provider, onDelta)
}

// newHTTPRequest builds the *http.Request shared between Decode +
// Stream. When c.cfg.GatewayURL is non-empty (sess#32 ambient-architect
// MatrixGateway routing per plan §5.16) the URL is rewritten to
// ${GatewayURL}/v1/chat/completions, Authorization carries the gateway
// bearer token (env var defaults to MATRIX_GATEWAY_TOKEN; overridable
// via cfg.GatewayTokenEnv), and the X-Matrix-* metadata headers are
// stamped so the gateway can authenticate, route, meter, and bill the
// call. Otherwise the legacy direct-provider posture is preserved
// verbatim (Bearer <APIKey> on c.endpoint).
func (c *Client) newHTTPRequest(ctx context.Context, body []byte, accept string) (*http.Request, error) {
	url := c.endpoint
	if c.cfg.GatewayURL != "" {
		url = strings.TrimRight(c.cfg.GatewayURL, "/") + "/v1/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)

	if c.cfg.GatewayURL == "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		return req, nil
	}

	tokenEnv := c.cfg.GatewayTokenEnv
	if tokenEnv == "" {
		tokenEnv = "MATRIX_GATEWAY_TOKEN"
	}
	if tok := os.Getenv(tokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if c.cfg.ActorDID != "" {
		req.Header.Set("X-Matrix-Actor-DID", c.cfg.ActorDID)
	}
	if c.cfg.IntentID != "" {
		req.Header.Set("X-Matrix-Intent-ID", c.cfg.IntentID)
	}
	if c.cfg.GoalID != "" {
		req.Header.Set("X-Matrix-Goal-ID", c.cfg.GoalID)
	}
	if c.cfg.SlotLabel != "" {
		req.Header.Set("X-Matrix-Slot", c.cfg.SlotLabel)
	}
	if c.cfg.KindRoute != "" {
		req.Header.Set("X-Matrix-Kind-Route", c.cfg.KindRoute)
	}
	return req, nil
}

// dispatchHeaders invokes the configured OnResponseHeaders hook with
// the upstream response headers. Panics in the callback are swallowed
// so a buggy hook never propagates into Decode/Stream's contract.
// Daemons read X-Matrix-Cost-Pax + Daily-Spent + Daily-Remaining off
// the headers passed in; gateway proxy stamps these on every metered
// call (plan §5.15).
func (c *Client) dispatchHeaders(h http.Header) {
	if c == nil || c.cfg.OnResponseHeaders == nil {
		return
	}
	defer func() { _ = recover() }()
	c.cfg.OnResponseHeaders(h)
}

// buildRequest assembles the chatRequest skeleton shared between Decode
// and Stream. Pure (no I/O); keeps the seed/grammar/temp logic in one
// place so streaming and non-streaming paths can never diverge.
func (c *Client) buildRequest(messages []interpreter.Message, grammar string) (*chatRequest, error) {
	chatMsgs := make([]chatMessage, len(messages))
	for i, m := range messages {
		chatMsgs[i] = chatMessage{Role: m.Role, Content: m.Content}
	}

	req := &chatRequest{
		Model:       c.cfg.Model,
		Messages:    chatMsgs,
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
	}

	if c.cfg.Seed != 0 {
		seed := c.cfg.Seed
		req.Seed = &seed
	}

	if grammar != "" && c.cfg.GrammarMode != GrammarNone {
		if err := c.applyGrammar(req, grammar); err != nil {
			return nil, fmt.Errorf("llm: grammar %q: %w", grammar, err)
		}
	}
	return req, nil
}

// parseSSEStream consumes an OpenAI-compatible text/event-stream body
// emitting onDelta(content) for each chunk and returning the full
// accumulated text on `data: [DONE]` or EOF. Exported for testing
// (parseSSEStreamForTest).
//
// The OpenAI streaming framing is one event per `data: <json>\n\n`
// block; we tolerate both LF and CRLF line endings and drop any
// non-data lines (event:, id:, retry:, comments) as the chat
// completions schema only carries data frames.
func parseSSEStream(ctx context.Context, r io.Reader, provider Provider,
	onDelta func(delta string)) (string, error) {

	scanner := bufio.NewScanner(r)
	// Bump the buffer because individual data lines can carry sizable
	// JSON deltas (especially with tool-call payloads); 1 MiB matches
	// what OpenAI's reference client uses.
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	var sb strings.Builder
	for scanner.Scan() {
		// Cooperative cancellation between frames; mid-frame is fine
		// because the server will close on its end when ctx is done.
		select {
		case <-ctx.Done():
			return sb.String(), ctx.Err()
		default:
		}

		line := scanner.Text()
		// Strip optional CR before LF (RFC 8895 SSE permits CRLF).
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue // event boundary; nothing to parse
		}
		if !strings.HasPrefix(line, "data:") {
			continue // ignore event:, id:, retry:, comment frames
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return sb.String(), nil
		}

		var frame streamFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			// Malformed frame is non-fatal — log silently by skipping
			// (some providers emit keepalive comments as `data: ` with
			// non-JSON sentinels that should not abort the stream).
			continue
		}
		if frame.Error != nil && frame.Error.Message != "" {
			return sb.String(), fmt.Errorf("llm: %s stream api error: %s",
				provider, frame.Error.Message)
		}
		if len(frame.Choices) == 0 {
			continue
		}
		delta := frame.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		sb.WriteString(delta)
		if onDelta != nil {
			onDelta(delta)
		}
	}
	if err := scanner.Err(); err != nil {
		return sb.String(), fmt.Errorf("llm: %s stream read: %w", provider, err)
	}
	// Stream closed without a [DONE] sentinel — accept whatever we got.
	// Some proxies strip the sentinel; the accumulated text is still
	// the authoritative response.
	return sb.String(), nil
}

// applyGrammar sets the response_format or grammar field on the request.
func (c *Client) applyGrammar(req *chatRequest, grammarID string) error {
	def, ok := c.cfg.Grammars[grammarID]
	if !ok {
		// No grammar def registered — skip constraint silently
		// (allows unconstrained fallback for unknown grammar IDs)
		return nil
	}

	switch c.cfg.GrammarMode {
	case GrammarJSONSchema:
		if def.JSONSchema == nil {
			return fmt.Errorf("no JSON schema defined for grammar %q", grammarID)
		}
		name := def.Name
		if name == "" {
			name = grammarID
		}
		req.ResponseFormat = &responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchemaRef{
				Name:   name,
				Schema: def.JSONSchema,
				Strict: true,
			},
		}
	case GrammarEBNF:
		if def.EBNF == "" {
			return fmt.Errorf("no EBNF defined for grammar %q", grammarID)
		}
		req.ResponseFormat = &responseFormat{
			Type:    "grammar",
			Grammar: def.EBNF,
		}
	}
	return nil
}

// DetectProvider maps a model string to its provider.
//
//	"accounts/fireworks/models/<X>"                 → Fireworks
//	"claude-*" / "gpt-5*" / opencode bare-model id  → Opencode (sess#34)
//	"<vendor>/<model>"                              → Together
func DetectProvider(model string) (Provider, error) {
	switch {
	case strings.HasPrefix(model, "accounts/fireworks/"):
		return ProviderFireworks, nil
	case isOpencodeModelID(model):
		return ProviderOpencode, nil
	case strings.Contains(model, "/"):
		return ProviderTogether, nil
	}
	return 0, fmt.Errorf("llm: cannot detect provider for model %q (expected '<vendor>/<model>', 'accounts/fireworks/models/<X>', or an opencode bare model id like 'claude-opus-4-7' / 'gpt-5.5')", model)
}

// isOpencodeModelID returns true for the bare model ids served by
// opencode.ai/zen (matrix/temp/model.opencode 2026-05-27 catalog):
// claude-*, gpt-5*, gemini-3*, qwen3.*-plus, kimi-k*, glm-*, minimax-m*,
// grok-build-*, big-pickle, deepseek-v4-flash-free, mimo-v2.5-free,
// nemotron-3-super-free.
func isOpencodeModelID(model string) bool {
	if strings.Contains(model, "/") {
		return false
	}
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-"):
		return true
	case strings.HasPrefix(m, "gpt-5"):
		return true
	case strings.HasPrefix(m, "gemini-"):
		return true
	case strings.HasPrefix(m, "qwen3."):
		return true
	case strings.HasPrefix(m, "kimi-k"):
		return true
	case strings.HasPrefix(m, "glm-"):
		return true
	case strings.HasPrefix(m, "minimax-m"):
		return true
	case strings.HasPrefix(m, "grok-build"):
		return true
	case m == "big-pickle":
		return true
	case strings.HasPrefix(m, "deepseek-v4-flash-free"):
		return true
	case strings.HasPrefix(m, "mimo-v2"):
		return true
	case strings.HasPrefix(m, "nemotron-"):
		return true
	}
	return false
}

func defaultEndpoint(p Provider) string {
	switch p {
	case ProviderTogether:
		return "https://api.together.xyz/v1/chat/completions"
	case ProviderFireworks:
		return "https://api.fireworks.ai/inference/v1/chat/completions"
	case ProviderOpencode:
		// Bare opencode default; ForgeRegistry overrides per-route to
		// the actual /v1/messages or /v1/responses URL.
		return "https://opencode.ai/zen/v1/chat/completions"
	}
	return ""
}

func envKey(p Provider) (string, error) {
	var name string
	switch p {
	case ProviderTogether:
		name = "TOGETHER_API_KEY"
	case ProviderFireworks:
		name = "FIREWORKS_API_KEY"
	case ProviderOpencode:
		name = "OPENCODE_API_KEY"
	default:
		return "", fmt.Errorf("llm: unknown provider %d", p)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("llm: %s is not set", name)
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- OpenAI-compatible request/response types ---

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ReasoningContent carries a reasoning model's chain-of-thought when
	// the provider returns it as a sibling of `content` (Fireworks /
	// DeepSeek-style). It is parsed so callers that want to surface
	// reasoning as a SEPARATE channel (never as the answer) can read it;
	// the replay-critical Decode path ignores it and returns only Content.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Seed           *int64          `json:"seed,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	// Stream toggles SSE delivery on /v1/chat/completions. Set by
	// (*Client).Stream; unset by Decode (omitempty preserves wire
	// shape parity with pre-P3 Decode requests for replay tests).
	Stream bool `json:"stream,omitempty"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema *jsonSchemaRef `json:"json_schema,omitempty"`
	Grammar    string         `json:"grammar,omitempty"`
}

type jsonSchemaRef struct {
	Name   string                 `json:"name"`
	Schema map[string]interface{} `json:"schema"`
	Strict bool                   `json:"strict"`
}

type chatResponse struct {
	ID      string         `json:"id"`
	Choices []chatChoice   `json:"choices"`
	Usage   *chatUsage     `json:"usage,omitempty"`
	Error   *chatErrorBody `json:"error,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// --- OpenAI-compatible streaming frame ---
//
// The streaming wire format mirrors choices[0].message but with
// "delta" replacing "message" so a frame can carry just the new
// token(s). The role-only opening frame (delta.role="assistant",
// no content) is permitted and ignored by parseSSEStream.

type streamFrame struct {
	ID      string         `json:"id"`
	Choices []streamChoice `json:"choices"`
	Error   *chatErrorBody `json:"error,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
