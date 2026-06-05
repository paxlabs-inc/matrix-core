// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// API-backed embedder over OpenAI-compatible /v1/embeddings endpoints.
//
// Default target: Fireworks AI (api.fireworks.ai) with the
// nomic-ai/nomic-embed-text-v1.5 model (768-dim, matches DefaultDim, so
// HNSW geometry survives swap from HashEmbedder).
//
// Q-lock decisions baked in (sess#19, 2026-05-24):
//   - Q1 α: model identifier pinned in Embedder.Model() ("<name>@<provider>");
//     vectors participate in Head → OverallRoot, drift across upgrades is
//     accepted (no on-chain anchor in v1, so no external observer cares).
//   - Q2 Fireworks: same OpenAI-compat infra as MCL/llm, zero new deps.
//   - Q3 lazy migrate: the cortex embedder worker already skips memories whose
//     EmbeddingRef.Model == current Embedder.Model() (embedder.go:411-413); on
//     mismatch it re-embeds. APIEmbedder participates in that path.
//   - Q4 768-dim: nomic-embed-text-v1.5 default; explicit Dim override allowed.
//
// Determinism caveat: production embedders are NOT bit-deterministic across
// runs (FP non-determinism in GPU batching). The Embedder interface contract
// says "deterministic"; this implementation is deterministic *per provider
// call* but not *across calls* in the strict sense. For replay-after-rebuild
// we rely on the worker re-running the embedder under the same model id; the
// resulting OverallRoot is "best-effort byte-stable" in v1, formally relaxed
// to "model-id-stable" until the executor model lock has matured.
//
// stdlib-only; reuses no third-party SDK.

package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// APIEmbedderConfig configures an APIEmbedder.
type APIEmbedderConfig struct {
	// Model is the provider-specific model identifier.
	//
	// Fireworks examples:
	//   "nomic-ai/nomic-embed-text-v1.5"           (768d, recommended)
	//   "thenlper/gte-large"                       (1024d)
	//   "WhereIsAI/UAE-Large-V1"                   (1024d)
	//
	// Together examples:
	//   "togethercomputer/m2-bert-80M-8k-retrieval"  (768d)
	//   "BAAI/bge-large-en-v1.5"                     (1024d)
	Model string

	// Endpoint overrides the provider's default /v1/embeddings URL.
	// If empty, FireworksEmbedEndpoint is used.
	Endpoint string

	// APIKey overrides the env-var lookup. If empty, FIREWORKS_API_KEY (or
	// TOGETHER_API_KEY if Endpoint points at Together) is read.
	APIKey string

	// Dim is the advertised vector dimensionality. MUST match what the model
	// actually returns; mismatch causes ErrDimMismatch on every Embed call.
	// Defaults to DefaultDim (768) which fits nomic-embed-text-v1.5.
	Dim int

	// Timeout for HTTP requests. Default 30s.
	Timeout time.Duration

	// MaxRetries on transient errors (5xx, timeout, rate-limit). Default 3.
	MaxRetries int

	// RetryBaseDelay is the base delay for exponential backoff. Default 1s.
	RetryBaseDelay time.Duration

	// ProviderTag is appended to Model() as "@<tag>" to disambiguate which
	// provider produced the vectors. Default "fireworks".
	ProviderTag string

	// HTTPClient is an optional override (mostly for tests). If nil, a fresh
	// http.Client with Timeout is used.
	HTTPClient *http.Client
}

// FireworksEmbedEndpoint is the default OpenAI-compat embeddings URL.
const FireworksEmbedEndpoint = "https://api.fireworks.ai/inference/v1/embeddings"

// TogetherEmbedEndpoint is the Together AI embeddings URL.
const TogetherEmbedEndpoint = "https://api.together.xyz/v1/embeddings"

// DefaultModelFireworks is the recommended Fireworks embedding model:
// nomic-embed-text-v1.5 at 768-dim (Matryoshka-capable, multilingual).
const DefaultModelFireworks = "nomic-ai/nomic-embed-text-v1.5"

// APIEmbedder implements Embedder by calling an OpenAI-compatible
// /v1/embeddings endpoint.
type APIEmbedder struct {
	cfg        APIEmbedderConfig
	endpoint   string
	apiKey     string
	httpClient *http.Client
	model      string // pinned "<model>@<provider>" string for Model()
}

// Ensure APIEmbedder satisfies Embedder at compile time.
var _ Embedder = (*APIEmbedder)(nil)

// NewAPIEmbedder constructs an APIEmbedder from cfg. Returns an error if the
// API key cannot be resolved.
func NewAPIEmbedder(cfg APIEmbedderConfig) (*APIEmbedder, error) {
	if cfg.Model == "" {
		cfg.Model = DefaultModelFireworks
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = FireworksEmbedEndpoint
	}
	if cfg.Dim <= 0 {
		cfg.Dim = DefaultDim
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = time.Second
	}
	if cfg.ProviderTag == "" {
		cfg.ProviderTag = inferProviderTag(cfg.Endpoint)
	}

	apiKey := cfg.APIKey
	if apiKey == "" {
		envName := envVarForEndpoint(cfg.Endpoint)
		apiKey = os.Getenv(envName)
		if apiKey == "" {
			return nil, fmt.Errorf("embed: %s is not set (or pass APIEmbedderConfig.APIKey)", envName)
		}
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}

	return &APIEmbedder{
		cfg:        cfg,
		endpoint:   cfg.Endpoint,
		apiKey:     apiKey,
		httpClient: httpClient,
		model:      cfg.Model + "@" + cfg.ProviderTag,
	}, nil
}

// Dim implements Embedder.
func (e *APIEmbedder) Dim() int { return e.cfg.Dim }

// Model implements Embedder. Format: "<model-id>@<provider>".
// e.g. "nomic-ai/nomic-embed-text-v1.5@fireworks".
func (e *APIEmbedder) Model() string { return e.model }

// Embed implements Embedder. The text is sent verbatim to the provider; the
// response vector is L2-normalised before return (most providers already
// normalise but we enforce for HNSW cosine-via-dot-product correctness).
//
// Empty text: returns ErrEmptyInput rather than the deterministic-stub fallback
// of HashEmbedder. Production embedders typically reject empty inputs.
//
// Retries: transient errors (network failure, 5xx, 429 rate-limit) are
// retried with exponential backoff up to MaxRetries.
func (e *APIEmbedder) Embed(text string) ([]float32, error) {
	if text == "" {
		return nil, ErrEmptyInput
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
	defer cancel()
	return e.embedCtx(ctx, text)
}

// embedCtx is the cancellation-aware embed call. Exposed for tests; the
// public Embed wraps it with the configured timeout.
func (e *APIEmbedder) embedCtx(ctx context.Context, text string) ([]float32, error) {
	body := embedRequest{
		Model: e.cfg.Model,
		Input: text,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= e.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with cap at 8s.
			delay := e.cfg.RetryBaseDelay * (1 << (attempt - 1))
			if delay > 8*time.Second {
				delay = 8 * time.Second
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		vec, retryable, err := e.embedOnce(ctx, payload)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("embed: %d attempts failed: %w", e.cfg.MaxRetries+1, lastErr)
}

// embedOnce issues a single HTTP request. Returns (vec, retryable, err).
func (e *APIEmbedder) embedOnce(ctx context.Context, payload []byte) ([]float32, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("embed: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		// Network errors are retryable.
		return nil, true, fmt.Errorf("embed: POST %s: %w", e.cfg.ProviderTag, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("embed: read body: %w", err)
	}

	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("embed: %s http %d: %s",
			e.cfg.ProviderTag, resp.StatusCode, truncate(string(respBody), 256))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed embedResponse
		_ = json.Unmarshal(respBody, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return nil, false, fmt.Errorf("embed: %s http %d: %s (type=%s)",
				e.cfg.ProviderTag, resp.StatusCode, parsed.Error.Message, parsed.Error.Type)
		}
		return nil, false, fmt.Errorf("embed: %s http %d: %s",
			e.cfg.ProviderTag, resp.StatusCode, truncate(string(respBody), 256))
	}

	var parsed embedResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, false, fmt.Errorf("embed: parse response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, false, fmt.Errorf("embed: %s api error: %s", e.cfg.ProviderTag, parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		return nil, false, errors.New("embed: empty data array in response")
	}

	raw := parsed.Data[0].Embedding
	if len(raw) != e.cfg.Dim {
		return nil, false, fmt.Errorf("embed: %w: got %d, want %d (model %q)",
			ErrDimMismatch, len(raw), e.cfg.Dim, e.cfg.Model)
	}

	// Convert []float64 → []float32 and L2-normalise.
	out := make([]float32, len(raw))
	var sumSq float64
	for i, v := range raw {
		out[i] = float32(v)
		sumSq += v * v
	}
	if sumSq > 0 {
		inv := float32(1.0 / math.Sqrt(sumSq))
		for i := range out {
			out[i] *= inv
		}
	}

	return out, false, nil
}

// envVarForEndpoint picks the canonical API-key env var based on endpoint URL.
func envVarForEndpoint(endpoint string) string {
	switch {
	case strings.Contains(endpoint, "fireworks"):
		return "FIREWORKS_API_KEY"
	case strings.Contains(endpoint, "together"):
		return "TOGETHER_API_KEY"
	default:
		return "EMBEDDING_API_KEY"
	}
}

// inferProviderTag returns a short identifier for the provider, used in
// Model() string to keep audit logs unambiguous.
func inferProviderTag(endpoint string) string {
	switch {
	case strings.Contains(endpoint, "fireworks"):
		return "fireworks"
	case strings.Contains(endpoint, "together"):
		return "together"
	default:
		return "custom"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- OpenAI-compatible request/response types ---

// embedRequest is the request body for /v1/embeddings. The OpenAI spec allows
// `input` to be a string or []string; we always send a string for now (single
// call per Embed). Batch support is deferred until the worker layer adopts it.
type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Object string          `json:"object"` // "list"
	Data   []embedDatum    `json:"data"`
	Model  string          `json:"model"`
	Usage  *embedUsage     `json:"usage,omitempty"`
	Error  *embedErrorBody `json:"error,omitempty"`
}

type embedDatum struct {
	Object    string    `json:"object"` // "embedding"
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

type embedUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type embedErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
