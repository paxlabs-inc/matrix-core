// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package embed

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEmbedder is a programmable Embedder for chain tests. It can be set to
// error, block past a deadline (simulating timeout), or return a canned vector.
// All fields are guarded so the chain can call Embed concurrently under -race.
type fakeEmbedder struct {
	model string
	dim   int

	mu      sync.Mutex
	failErr error // when non-nil, Embed returns this error
	calls   int32 // atomic, counts Embed invocations
	delay   time.Duration
}

func (f *fakeEmbedder) Embed(text string) ([]float32, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-context.Background().Done():
		}
	}
	f.mu.Lock()
	err := f.failErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Deterministic canned vector: derive from model so two fakes differ.
	out := make([]float32, f.dim)
	for i := range out {
		out[i] = float32(len(f.model) + i)
	}
	return out, nil
}

func (f *fakeEmbedder) Dim() int      { return f.dim }
func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) setFail(err error) {
	f.mu.Lock()
	f.failErr = err
	f.mu.Unlock()
}

func newFake(model string, dim int) *fakeEmbedder {
	return &fakeEmbedder{model: model, dim: dim}
}

// TestEmbedderChain_PrimarySuccessNoFallback verifies that when the first
// (primary) provider is healthy, the chain uses it and never consults the
// fallback providers.
func TestEmbedderChain_PrimarySuccessNoFallback(t *testing.T) {
	t.Parallel()
	primary := newFake("primary@v1", DefaultDim)
	secondary := newFake("secondary@v1", DefaultDim)

	chain := NewEmbedderChain(ChainConfig{
		Providers: []Embedder{primary, secondary},
		Timeout:   2 * time.Second,
	})

	vec, err := chain.Embed("hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != DefaultDim {
		t.Fatalf("len(vec) = %d, want %d", len(vec), DefaultDim)
	}
	// Primary must have been called exactly once.
	if got := atomic.LoadInt32(&primary.calls); got != 1 {
		t.Fatalf("primary calls = %d, want 1", got)
	}
	// Secondary must never have been consulted.
	if got := atomic.LoadInt32(&secondary.calls); got != 0 {
		t.Fatalf("secondary calls = %d, want 0 (no fallback on primary success)", got)
	}
	// The chain's Model/Dim must reflect the resolved primary.
	if chain.Model() != primary.Model() {
		t.Errorf("chain.Model() = %q, want %q", chain.Model(), primary.Model())
	}
	if chain.Dim() != primary.Dim() {
		t.Errorf("chain.Dim() = %d, want %d", chain.Dim(), primary.Dim())
	}
}

// TestEmbedderChain_FallsThroughOnError verifies that when the primary
// returns an error, the chain falls through to the next provider and returns
// its result instead of propagating the primary error.
func TestEmbedderChain_FallsThroughOnError(t *testing.T) {
	t.Parallel()
	primary := newFake("primary@v1", DefaultDim)
	primary.setFail(errors.New("provider down"))
	secondary := newFake("secondary@v1", DefaultDim)

	chain := NewEmbedderChain(ChainConfig{
		Providers: []Embedder{primary, secondary},
		Timeout:   2 * time.Second,
	})

	vec, err := chain.Embed("hello")
	if err != nil {
		t.Fatalf("Embed should fall through, got: %v", err)
	}
	if len(vec) != DefaultDim {
		t.Fatalf("len(vec) = %d, want %d", len(vec), DefaultDim)
	}
	// Primary was attempted (and failed).
	if got := atomic.LoadInt32(&primary.calls); got != 1 {
		t.Fatalf("primary calls = %d, want 1", got)
	}
	// Secondary was used as the fallback.
	if got := atomic.LoadInt32(&secondary.calls); got != 1 {
		t.Fatalf("secondary calls = %d, want 1 (should be used on primary error)", got)
	}
	// Chain Model reflects the provider that actually succeeded.
	if chain.Model() != secondary.Model() {
		t.Errorf("chain.Model() = %q, want %q (resolved provider)", chain.Model(), secondary.Model())
	}
}

// TestEmbedderChain_TerminalHashFallback verifies that when every configured
// provider fails, the chain falls through to the built-in Hash stub as the
// terminal fallback — degraded but never fatal.
func TestEmbedderChain_TerminalHashFallback(t *testing.T) {
	t.Parallel()
	primary := newFake("primary@v1", DefaultDim)
	primary.setFail(errors.New("provider down"))
	secondary := newFake("secondary@v1", DefaultDim)
	secondary.setFail(errors.New("provider down"))

	chain := NewEmbedderChain(ChainConfig{
		Providers: []Embedder{primary, secondary},
		Timeout:   2 * time.Second,
	})

	vec, err := chain.Embed("hello")
	if err != nil {
		t.Fatalf("terminal Hash fallback must never error, got: %v", err)
	}
	if len(vec) != DefaultDim {
		t.Fatalf("len(vec) = %d, want %d (Hash stub DefaultDim)", len(vec), DefaultDim)
	}
	// Both real providers were attempted.
	if got := atomic.LoadInt32(&primary.calls); got != 1 {
		t.Fatalf("primary calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&secondary.calls); got != 1 {
		t.Fatalf("secondary calls = %d, want 1", got)
	}
	// The terminal Hash stub is deterministic: a fresh HashEmbedder must
	// produce byte-identical output for the same text.
	hash := NewHashEmbedder()
	want, _ := hash.Embed("hello")
	for i := range vec {
		if vec[i] != want[i] {
			t.Fatalf("terminal fallback vec[%d] = %v, want hash-stub %v", i, vec[i], want[i])
		}
	}
	// Model reflects the hash stub.
	if chain.Model() != hash.Model() {
		t.Errorf("chain.Model() = %q, want %q (hash stub)", chain.Model(), hash.Model())
	}
}

// TestEmbedderChain_DoesNotAffectReplayRoot proves the m9 invariant:
// when the EmbedderChain resolves to provider P (primary healthy), it is
// observationally identical to using P directly — same Embed() bytes, same
// Model(), same Dim(). Since replay OverallRoot depends only on Head canonical
// bytes staged from the resolved embedder (vec/ is NOT an anchored namespace),
// wrapping P in an EmbedderChain cannot alter the replay root.
//
// We also verify the negative: if the chain had fallen through to a different
// provider, the Model() WOULD differ — proving the test is load-bearing
// (it would catch a wrapper that silently substituted a different provider).
func TestEmbedderChain_DoesNotAffectReplayRoot(t *testing.T) {
	t.Parallel()
	primary := newFake("nomic-ai/nomic-embed-text-v1.5@fireworks", DefaultDim)

	// Direct embedder (the "no wrapper" baseline).
	direct := primary

	// Wrapped embedder (chain with primary as the sole real provider).
	chain := NewEmbedderChain(ChainConfig{
		Providers: []Embedder{primary},
		Timeout:   2 * time.Second,
	})

	// 1. Embed output is byte-identical.
	for _, text := range []string{"", "hello", "a longer piece of text for embedding"} {
		vDirect, errDirect := direct.Embed(text)
		vChain, errChain := chain.Embed(text)
		if errDirect != nil || errChain != nil {
			t.Fatalf("Embed(%q) errors: direct=%v chain=%v", text, errDirect, errChain)
		}
		if len(vDirect) != len(vChain) {
			t.Fatalf("Embed(%q) len: direct=%d chain=%d", text, len(vDirect), len(vChain))
		}
		for i := range vDirect {
			if vDirect[i] != vChain[i] {
				t.Fatalf("Embed(%q) byte drift at %d: direct=%v chain=%v", text, i, vDirect[i], vChain[i])
			}
		}
	}

	// 2. Model() and Dim() are identical — these are what feed Head.EmbeddingRef
	// and thus the memories SMT root (the only path by which embedder choice
	// could reach OverallRoot).
	if chain.Model() != direct.Model() {
		t.Errorf("Model: chain=%q direct=%q (must match for replay root stability)", chain.Model(), direct.Model())
	}
	if chain.Dim() != direct.Dim() {
		t.Errorf("Dim: chain=%d direct=%d (must match for replay root stability)", chain.Dim(), direct.Dim())
	}

	// 3. Negative control: a chain that falls through to a DIFFERENT provider
	// DOES report a different Model() — confirming this test would catch a
	// wrapper that silently swapped providers (which WOULD change replay root
	// via Head.EmbeddingRef.Model).
	primaryFail := newFake("nomic-ai/nomic-embed-text-v1.5@fireworks", DefaultDim)
	primaryFail.setFail(errors.New("down"))
	other := newFake("hash-stub@v1", DefaultDim)
	chainFallthrough := NewEmbedderChain(ChainConfig{
		Providers: []Embedder{primaryFail, other},
		Timeout:   2 * time.Second,
	})
	_ = chainFallthrough // force resolution
	if _, err := chainFallthrough.Embed("probe"); err != nil {
		t.Fatalf("fallthrough probe: %v", err)
	}
	if chainFallthrough.Model() == direct.Model() {
		t.Errorf("fallthrough Model = %q should differ from direct %q (negative control)", chainFallthrough.Model(), direct.Model())
	}
}
