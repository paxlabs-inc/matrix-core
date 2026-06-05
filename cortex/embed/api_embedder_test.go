// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package embed

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockVector returns a deterministic 768-dim float64 vector keyed by text.
// Not L2-normalised; the APIEmbedder normalises on receive.
func mockVector(text string, dim int) []float64 {
	out := make([]float64, dim)
	seed := 0
	for _, c := range text {
		seed = (seed*31 + int(c)) % 997
	}
	for i := 0; i < dim; i++ {
		out[i] = float64((seed+i*7)%199) / 100.0
	}
	return out
}

// startMockServer returns an httptest.Server emulating the OpenAI-compat
// /v1/embeddings endpoint. The handler counter is incremented per request.
func startMockServer(t *testing.T, dim int, status int, callCounter *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(callCounter, 1)

		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing Bearer token: %q", r.Header.Get("Authorization"))
		}

		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode body: %v", err)
		}

		if status != 200 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(embedResponse{
				Error: &embedErrorBody{Message: "synthetic error", Type: "test"},
			})
			return
		}

		resp := embedResponse{
			Object: "list",
			Model:  req.Model,
			Data: []embedDatum{
				{Object: "embedding", Index: 0, Embedding: mockVector(req.Input, dim)},
			},
			Usage: &embedUsage{PromptTokens: 5, TotalTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestAPIEmbedder_NewDefaults(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "test-key")
	e, err := NewAPIEmbedder(APIEmbedderConfig{})
	if err != nil {
		t.Fatalf("NewAPIEmbedder: %v", err)
	}
	if e.Dim() != DefaultDim {
		t.Errorf("Dim() = %d, want %d", e.Dim(), DefaultDim)
	}
	if e.Model() != DefaultModelFireworks+"@fireworks" {
		t.Errorf("Model() = %q", e.Model())
	}
	if e.cfg.Endpoint != FireworksEmbedEndpoint {
		t.Errorf("endpoint = %q", e.cfg.Endpoint)
	}
}

func TestAPIEmbedder_MissingAPIKey(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "")
	_, err := NewAPIEmbedder(APIEmbedderConfig{})
	if err == nil {
		t.Fatal("expected error when FIREWORKS_API_KEY is unset")
	}
	if !strings.Contains(err.Error(), "FIREWORKS_API_KEY") {
		t.Errorf("error should name env var: %v", err)
	}
}

func TestAPIEmbedder_TogetherEndpointSelectsTogetherKey(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "tg-key")
	e, err := NewAPIEmbedder(APIEmbedderConfig{
		Endpoint: TogetherEmbedEndpoint,
		Model:    "togethercomputer/m2-bert-80M-8k-retrieval",
	})
	if err != nil {
		t.Fatalf("NewAPIEmbedder: %v", err)
	}
	if !strings.HasSuffix(e.Model(), "@together") {
		t.Errorf("Model() = %q, want ProviderTag=together", e.Model())
	}
}

func TestAPIEmbedder_EmbedSuccess(t *testing.T) {
	var calls int32
	server := startMockServer(t, DefaultDim, 200, &calls)
	defer server.Close()

	e, err := NewAPIEmbedder(APIEmbedderConfig{
		Model:    "test-model",
		Endpoint: server.URL,
		APIKey:   "test-key",
		Dim:      DefaultDim,
	})
	if err != nil {
		t.Fatalf("NewAPIEmbedder: %v", err)
	}

	vec, err := e.Embed("hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != DefaultDim {
		t.Errorf("vec len = %d, want %d", len(vec), DefaultDim)
	}

	// L2 norm should be ~1.0 (we normalise on receive).
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if math.Abs(math.Sqrt(sumSq)-1.0) > 1e-5 {
		t.Errorf("vector not unit-normalised: |v| = %f", math.Sqrt(sumSq))
	}

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestAPIEmbedder_EmbedEmptyText(t *testing.T) {
	var calls int32
	server := startMockServer(t, DefaultDim, 200, &calls)
	defer server.Close()

	e, _ := NewAPIEmbedder(APIEmbedderConfig{
		Endpoint: server.URL,
		APIKey:   "test-key",
	})

	_, err := e.Embed("")
	if err != ErrEmptyInput {
		t.Errorf("Embed(\"\") err = %v, want ErrEmptyInput", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("calls = %d, want 0 (no HTTP call on empty input)", calls)
	}
}

func TestAPIEmbedder_EmbedDimMismatch(t *testing.T) {
	var calls int32
	server := startMockServer(t, DefaultDim, 200, &calls)
	defer server.Close()

	// Configure embedder to expect 1024 but mock returns 768 → mismatch.
	e, _ := NewAPIEmbedder(APIEmbedderConfig{
		Endpoint: server.URL,
		APIKey:   "test-key",
		Dim:      1024,
	})

	_, err := e.Embed("text")
	if err == nil || !strings.Contains(err.Error(), "ErrDimMismatch") &&
		!strings.Contains(err.Error(), "dim does not match") {
		t.Errorf("expected dim mismatch error, got %v", err)
	}
}

func TestAPIEmbedder_EmbedRetriesOn5xx(t *testing.T) {
	var calls int32
	// First two calls return 503, third returns 200.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if n < 3 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"temp fail","type":"upstream"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{
			Object: "list",
			Data: []embedDatum{
				{Object: "embedding", Embedding: mockVector(req.Input, DefaultDim)},
			},
		})
	}))
	defer server.Close()

	e, _ := NewAPIEmbedder(APIEmbedderConfig{
		Endpoint:       server.URL,
		APIKey:         "test-key",
		MaxRetries:     5,
		RetryBaseDelay: 10 * time.Millisecond,
	})

	vec, err := e.Embed("retry me")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != DefaultDim {
		t.Errorf("vec len = %d", len(vec))
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestAPIEmbedder_EmbedNoRetryOn4xx(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad input","type":"invalid_request"}}`))
	}))
	defer server.Close()

	e, _ := NewAPIEmbedder(APIEmbedderConfig{
		Endpoint:       server.URL,
		APIKey:         "test-key",
		MaxRetries:     5,
		RetryBaseDelay: 10 * time.Millisecond,
	})

	_, err := e.Embed("text")
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "bad input") {
		t.Errorf("error should surface provider message: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", calls)
	}
}

func TestAPIEmbedder_ImplementsEmbedderInterface(t *testing.T) {
	// Compile-time + runtime double-check that the type satisfies Embedder.
	var _ Embedder = (*APIEmbedder)(nil)

	t.Setenv("FIREWORKS_API_KEY", "test-key")
	e, err := NewAPIEmbedder(APIEmbedderConfig{})
	if err != nil {
		t.Fatalf("NewAPIEmbedder: %v", err)
	}

	var iface Embedder = e
	if iface.Dim() != DefaultDim {
		t.Errorf("interface Dim() = %d", iface.Dim())
	}
	if iface.Model() == "" {
		t.Error("interface Model() returned empty")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
