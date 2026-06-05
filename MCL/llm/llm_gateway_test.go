// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"matrix/mcl/mtx/interpreter"
)

// TestGatewayRouting exercises the sess#32 §5.16 gateway-aware path.
//
// Asserts:
//   - URL is GatewayURL + "/v1/chat/completions" (not provider direct)
//   - Authorization is the gateway token, not the provider key
//   - X-Matrix-* headers carry through verbatim
//   - OnResponseHeaders fires with the upstream response headers
//   - direct-provider behaviour preserved when GatewayURL == ""
func TestGatewayRouting(t *testing.T) {
	var seenURL string
	var seenHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURL = r.URL.Path
		seenHeaders = r.Header.Clone()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Matrix-Cost-Pax", "0.000123000000")
		w.Header().Set("X-Matrix-Daily-Spent-Pax", "0.5")
		w.Header().Set("X-Matrix-Daily-Remaining-Pax", "9.5")
		_, _ = w.Write([]byte(`{
		  "id":"x",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`))
	}))
	defer srv.Close()

	t.Setenv("MATRIX_GATEWAY_TOKEN", "g_token_xyz")

	var hookCalls int32
	var capturedCost string
	cfg := &Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    ProviderFireworks,
		ProviderSet: true,
		APIKey:      "ignored-when-gateway-on",
		GatewayURL:  srv.URL,
		ActorDID:    "did:pax:abcdef",
		IntentID:    "intent_42",
		GoalID:      "goal_99",
		SlotLabel:   "compiler",
		KindRoute:   "classify",
		OnResponseHeaders: func(h http.Header) {
			atomic.AddInt32(&hookCalls, 1)
			capturedCost = h.Get("X-Matrix-Cost-Pax")
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := c.Decode(context.Background(),
		[]interpreter.Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("decoded text mismatch: %q", out)
	}
	if seenURL != "/v1/chat/completions" {
		t.Fatalf("gateway URL path = %q, want /v1/chat/completions", seenURL)
	}
	if got := seenHeaders.Get("Authorization"); got != "Bearer g_token_xyz" {
		t.Fatalf("Authorization = %q, want Bearer g_token_xyz", got)
	}
	if got := seenHeaders.Get("X-Matrix-Actor-DID"); got != "did:pax:abcdef" {
		t.Fatalf("Actor-DID = %q", got)
	}
	if got := seenHeaders.Get("X-Matrix-Intent-ID"); got != "intent_42" {
		t.Fatalf("Intent-ID = %q", got)
	}
	if got := seenHeaders.Get("X-Matrix-Goal-ID"); got != "goal_99" {
		t.Fatalf("Goal-ID = %q", got)
	}
	if got := seenHeaders.Get("X-Matrix-Slot"); got != "compiler" {
		t.Fatalf("Slot = %q", got)
	}
	if got := seenHeaders.Get("X-Matrix-Kind-Route"); got != "classify" {
		t.Fatalf("Kind-Route = %q", got)
	}
	if atomic.LoadInt32(&hookCalls) != 1 {
		t.Fatalf("OnResponseHeaders fired %d times, want 1", hookCalls)
	}
	if capturedCost != "0.000123000000" {
		t.Fatalf("captured cost = %q", capturedCost)
	}
}

// TestDirectProviderPreserved verifies the legacy direct-provider path
// still emits Bearer <provider key> on the configured per-provider URL.
func TestDirectProviderPreserved(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	cfg := &Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    ProviderFireworks,
		ProviderSet: true,
		APIKey:      "fw_real_key",
		Endpoint:    srv.URL,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Decode(context.Background(),
		[]interpreter.Message{{Role: "user", Content: "hi"}}, ""); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if seenAuth != "Bearer fw_real_key" {
		t.Fatalf("Authorization = %q, want Bearer fw_real_key", seenAuth)
	}
}

// TestGatewayBudgetExhaustedSurfacesError ensures a 429 from the
// gateway propagates as an error (existing non-2xx handling) AND
// the OnResponseHeaders hook still fires so the daemon can read
// the spent/limit headers.
func TestGatewayBudgetExhaustedSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Matrix-Daily-Spent-Pax", "10")
		w.Header().Set("X-Matrix-Daily-Remaining-Pax", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		body, _ := json.Marshal(map[string]string{
			"error":     "budget_exhausted",
			"spent_pax": "10",
			"limit_pax": "10",
		})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	t.Setenv("MATRIX_GATEWAY_TOKEN", "tok")
	var saw http.Header
	cfg := &Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    ProviderFireworks,
		ProviderSet: true,
		APIKey:      "x",
		GatewayURL:  srv.URL,
		ActorDID:    "did:pax:a",
		SlotLabel:   "compiler",
		OnResponseHeaders: func(h http.Header) {
			saw = h.Clone()
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Decode(context.Background(),
		[]interpreter.Message{{Role: "user", Content: "x"}}, ""); err == nil {
		t.Fatalf("expected error on 429")
	}
	if saw.Get("X-Matrix-Daily-Remaining-Pax") != "0" {
		t.Fatalf("expected daily-remaining=0 in observed headers; got %v", saw)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
