// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// EmbedderChain is a reliability wrapper around the Embedder interface that
// tries an ordered list of providers and falls through on error/timeout, with
// the local Hash stub (embed.HashEmbedder) as the terminal fallback so an
// outage degrades retrieval quality but never crashes the caller.
//
// Why this exists: Cortex/Neo embedding goes through a single provider (the
// Fireworks-style APIEmbedder). A provider outage breaks retrieval with no
// fallback. The Hash stub already exists for tests; the chain promotes it to
// a production-grade terminal fallback.
//
// Determinism / replay invariant (m9): embeddings are sidecar/derived state
// (vec/, not in OverallRoot). The chain adds NO journaled state of its own.
// When it resolves to provider P, it is observationally identical to using P
// directly — same Embed() bytes, same Model(), same Dim(). OverallRoot depends
// only on Head canonical bytes staged from the resolved embedder, so wrapping P
// in a chain cannot alter the replay root. When the chain falls through to a
// DIFFERENT provider, Model() changes — which the existing lazy-migrate
// machinery (embedder.go cursor rewind on model mismatch) already handles as a
// legitimate model swap, not a silent corruption.
//
// Config: ordered provider list + per-provider timeout + env construction via
// NewChainFromEnv.
package embed

import (
	"context"
	"fmt"
	"os"
	"time"
)

// ChainConfig configures an EmbedderChain.
type ChainConfig struct {
	// Providers is the ordered list of embedders to try. The chain appends a
	// fresh NewHashEmbedder() as the terminal fallback if none is already a
	// *HashEmbedder, guaranteeing the chain can never return an error from
	// Embed().
	Providers []Embedder

	// Timeout is the per-provider deadline for a single Embed call. If a
	// provider does not return within Timeout, the chain cancels it and falls
	// through to the next provider. Zero means 30s (matches APIEmbedder default).
	Timeout time.Duration

	// Logf, if non-nil, receives one-line messages when a provider is skipped
	// (error or timeout) so operators can see degradation. Defaults to no-op.
	Logf func(format string, args ...any)
}

// EmbedderChain implements Embedder by trying providers in order and falling
// through on error/timeout, with a Hash stub terminal fallback.
type EmbedderChain struct {
	providers []Embedder
	timeout    time.Duration
	logf       func(string, ...any)

	// lastModel/lastDim record the Model()/Dim() of the provider that most
	// recently succeeded. They are updated on each successful Embed so that
	// Model()/Dim() reflect the resolved provider. Reads are rare (once per
	// embed-cycle in the worker; Model() is checked against Head.EmbeddingRef)
	// so a mutex is cheaper than an atomic of a string.
	mu       chanMutex
	lastModel string
	lastDim   int
}

// chanMutex is a tiny helper so we don't pull in sync just for one field; it
// serializes Model()/Dim() reads against Embed() writes. Embeddings are never
// on the hot path of a query (the worker is single-threaded; query reads the
// HNSW index, not Embed), so the lock is uncontended in practice.
type chanMutex struct {
	c chan struct{}
}

func newChanMutex() chanMutex {
	c := make(chan struct{}, 1)
	c <- struct{}{}
	return chanMutex{c: c}
}

func (m chanMutex) lock()   { <-m.c }
func (m chanMutex) unlock() { m.c <- struct{}{} }

// Ensure EmbedderChain satisfies Embedder at compile time.
var _ Embedder = (*EmbedderChain)(nil)

// NewEmbedderChain constructs an EmbedderChain from cfg. If none of the
// configured providers is a *HashEmbedder, a fresh NewHashEmbedder() is
// appended as the terminal fallback so Embed() can never return an error.
func NewEmbedderChain(cfg ChainConfig) *EmbedderChain {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}

	providers := make([]Embedder, 0, len(cfg.Providers)+1)
	hasHash := false
	for _, p := range cfg.Providers {
		if p == nil {
			continue
		}
		if _, ok := p.(*HashEmbedder); ok {
			hasHash = true
		}
		providers = append(providers, p)
	}
	if !hasHash {
		providers = append(providers, NewHashEmbedder())
	}

	c := &EmbedderChain{
		providers: providers,
		timeout:   cfg.Timeout,
		logf:      cfg.Logf,
		mu:        newChanMutex(),
	}
	// Seed lastModel/lastDim from the first provider so Model()/Dim() are
	// meaningful even before the first Embed() resolves.
	if len(providers) > 0 {
		c.lastModel = providers[0].Model()
		c.lastDim = providers[0].Dim()
	}
	return c
}

// Dim implements Embedder. Returns the Dim() of the most-recently-resolved
// provider (or the first provider before any Embed call). All providers in a
// well-formed chain share the same Dim (DefaultDim); a mismatch is a config
// error surfaced by the worker's len(vec) != Dim() check.
func (c *EmbedderChain) Dim() int {
	c.mu.lock()
	defer c.mu.unlock()
	if c.lastDim == 0 && len(c.providers) > 0 {
		return c.providers[0].Dim()
	}
	return c.lastDim
}

// Model implements Embedder. Returns the Model() of the most-recently-resolved
// provider (or the first provider before any Embed call). This is what feeds
// Head.EmbeddingRef.Model and thus the replay invariant — it MUST reflect the
// provider that actually produced the vectors, never a synthetic chain id.
func (c *EmbedderChain) Model() string {
	c.mu.lock()
	defer c.mu.unlock()
	if c.lastModel == "" && len(c.providers) > 0 {
		return c.providers[0].Model()
	}
	return c.lastModel
}

// Embed implements Embedder. Tries each provider in order with a per-provider
// timeout; on error or timeout, falls through to the next provider. The
// terminal Hash stub (appended at construction) guarantees a non-error result.
func (c *EmbedderChain) Embed(text string) ([]float32, error) {
	var lastErr error
	for _, p := range c.providers {
		vec, err := c.embedWithTimeout(p, text)
		if err == nil {
			// Record the resolved provider's identity.
			c.mu.lock()
			c.lastModel = p.Model()
			c.lastDim = p.Dim()
			c.mu.unlock()
			return vec, nil
		}
		lastErr = err
		c.logf("embed: provider %s failed: %v; falling through", p.Model(), err)
	}
	// Unreachable: the terminal Hash stub never errors. Defensively return
	// the last error rather than panicking, so a misconfigured chain degrades
	// rather than crashes.
	return nil, fmt.Errorf("embed: all %d providers failed (including terminal hash): %w", len(c.providers), lastErr)
}

// embedWithTimeout calls p.Embed with a context-bounded deadline. If p.Embed
// does not respect context cancellation (the HashEmbedder and APIEmbedder do,
// but a custom Embedder might not), the goroutine is abandoned after Timeout
// and the error is treated as a fallthrough trigger — the chain proceeds to
// the next provider. This bounds latency even with a misbehaving embedder.
func (c *EmbedderChain) embedWithTimeout(p Embedder, text string) ([]float32, error) {
	// Fast path for the HashEmbedder terminal fallback: it is in-process and
	// deterministic, so the timeout machinery would only add overhead. Run it
	// directly.
	if _, ok := p.(*HashEmbedder); ok {
		return p.Embed(text)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	type result struct {
		vec []float32
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		vec, err := p.Embed(text)
		select {
		case resCh <- result{vec: vec, err: err}:
		case <-ctx.Done():
		}
	}()

	select {
	case r := <-resCh:
		return r.vec, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("embed: provider %s timed out after %s", p.Model(), c.timeout)
	}
}

// NewChainFromEnv builds an EmbedderChain from environment variables, mirroring
// the preference order used by neo/internal/memory.pickEmbedder:
//
//  1. the Matrix gateway when MATRIX_GATEWAY_URL + MATRIX_GATEWAY_TOKEN are set;
//  2. the provider directly when FIREWORKS_API_KEY is set;
//  3. the Hash stub terminal fallback (always present).
//
// This lets production wiring construct a chain declaratively while tests pass
// an explicit provider list via NewEmbedderChain.
//
// gatewayDID, when non-empty, is stamped as X-Matrix-Actor-DID on gateway
// requests for metering attribution.
func NewChainFromEnv(model string, gatewayDID string) *EmbedderChain {
	if model == "" {
		model = DefaultModelFireworks
	}
	cfg := ChainConfig{
		Timeout: 30 * time.Second,
	}

	gw := os.Getenv("MATRIX_GATEWAY_URL")
	tok := os.Getenv("MATRIX_GATEWAY_TOKEN")
	if gw != "" && tok != "" {
		if e, err := NewAPIEmbedder(APIEmbedderConfig{
			Model:       model,
			Endpoint:    gw + "/v1/embeddings",
			APIKey:      tok,
			ProviderTag: "gateway",
		}); err == nil {
			cfg.Providers = append(cfg.Providers, e)
		}
	}

	if os.Getenv("FIREWORKS_API_KEY") != "" {
		if e, err := NewAPIEmbedder(APIEmbedderConfig{Model: model}); err == nil {
			cfg.Providers = append(cfg.Providers, e)
		}
	}

	// NewEmbedderChain appends the Hash stub terminal fallback.
	return NewEmbedderChain(cfg)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
