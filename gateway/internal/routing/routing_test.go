// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package routing

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix/gateway/internal/rates"
	"matrix/gateway/internal/types"
)

func newReq(slot, kind string, byo bool, byoKey string) *httptest.ResponseRecorder {
	// Helper unused; keep signature fixed so tests stay terse.
	return nil
}

func TestDecideFreeTierWhitelistedCompiler(t *testing.T) {
	d := New(Options{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "compiler")

	dec, err := d.Decide(r, rates.ModelCompilerFreeTier, EndpointChat)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !dec.FreeTier {
		t.Fatalf("expected FreeTier=true")
	}
	if dec.Provider != ProviderFireworks {
		t.Fatalf("expected fireworks, got %s", dec.Provider)
	}
}

func TestDecideFreeTierRejectsNonWhitelistedModel(t *testing.T) {
	d := New(Options{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "compiler")

	_, err := d.Decide(r, "accounts/fireworks/models/gpt-oss-20b", EndpointChat)
	if !errors.Is(err, ErrFreeTierNotWhitelisted) {
		t.Fatalf("expected ErrFreeTierNotWhitelisted, got %v", err)
	}
}

func TestDecideExecutorWhitelistedDeepSeek(t *testing.T) {
	d := New(Options{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "executor")
	r.Header.Set(types.HeaderKindRoute, "summarize")

	dec, err := d.Decide(r, rates.ModelExecutorFreeTier, EndpointChat)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.KindRoute != "summarize" {
		t.Fatalf("kind echo failed: %q", dec.KindRoute)
	}
}

func TestDecideBYOBypassesWhitelist(t *testing.T) {
	d := New(Options{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "executor")
	r.Header.Set(types.HeaderBYOAPIKey, "true")
	r.Header.Set(types.HeaderUserAPIKey, "fw_user_secret")

	dec, err := d.Decide(r, "Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8", EndpointChat)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.FreeTier {
		t.Fatalf("BYO must clear FreeTier")
	}
	if dec.UserAPIKey != "fw_user_secret" {
		t.Fatalf("UserAPIKey echo failed: %q", dec.UserAPIKey)
	}
	if dec.Provider != ProviderTogether {
		t.Fatalf("expected together for Qwen/, got %s", dec.Provider)
	}
}

func TestDecideBYOMissingKey(t *testing.T) {
	d := New(Options{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "executor")
	r.Header.Set(types.HeaderBYOAPIKey, "true")
	// No HeaderUserAPIKey

	_, err := d.Decide(r, rates.ModelExecutorFreeTier, EndpointChat)
	if !errors.Is(err, ErrBYOMissingKey) {
		t.Fatalf("expected ErrBYOMissingKey, got %v", err)
	}
}

func TestDecideFreeTierOnlyRejectsBYO(t *testing.T) {
	d := New(Options{FreeTierOnly: true})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "executor")
	r.Header.Set(types.HeaderBYOAPIKey, "true")
	r.Header.Set(types.HeaderUserAPIKey, "fw")

	_, err := d.Decide(r, "Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8", EndpointChat)
	if err == nil {
		t.Fatalf("FreeTierOnly should reject BYO")
	}
}

func TestDecideInvalidSlot(t *testing.T) {
	d := New(Options{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "ghost")
	_, err := d.Decide(r, rates.ModelCompilerFreeTier, EndpointChat)
	if !errors.Is(err, ErrInvalidSlot) {
		t.Fatalf("expected ErrInvalidSlot, got %v", err)
	}
}

func TestDecideEmbeddingsEndpoint(t *testing.T) {
	d := New(Options{})
	r := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader("{}"))
	r.Header.Set(types.HeaderSlot, "compiler")

	dec, err := d.Decide(r, rates.ModelCompilerFreeTier, EndpointEmbedding)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !strings.Contains(dec.UpstreamURL, "/embeddings") {
		t.Fatalf("expected embeddings upstream, got %q", dec.UpstreamURL)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
