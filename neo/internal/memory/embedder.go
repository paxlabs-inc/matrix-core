// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"net/http"
	"os"
	"strings"
	"time"

	"matrix/cortex/embed"

	"matrix/neo/internal/config"
)

// pickEmbedder selects the best available embedding backend for semantic
// page-faulting. It returns an embed.EmbedderChain that tries providers in
// order of preference and falls through on error/timeout, with the local
// Hash stub as the terminal fallback (degraded but non-fatal).
//
// Provider order (P2-8):
//
//  1. the metered Matrix gateway /v1/embeddings route, when the gateway is
//     wired (MATRIX_GATEWAY_URL + MATRIX_GATEWAY_TOKEN + actor DID) — spend
//     is attributed to the actor under slot "neo" exactly like chat calls;
//  2. the provider directly, when FIREWORKS_API_KEY is set;
//  3. the deterministic hash embedder — retrieval quality degrades to
//     pseudo-lexical, but nothing breaks (the pre-v5 behavior).
//
// Each real provider is boot-probed (probeEmbedder) before being added to the
// chain: a provider that fails the probe (misconfigured credentials, 403,
// unreachable) is omitted so we don't pay the latency of a guaranteed-fail
// call on every page-fault. The Hash stub terminal fallback guarantees the
// chain never returns an error even if every real provider is down at call
// time (a provider can pass the probe but fail later under load).
//
// The cortex embedder worker lazily re-embeds memories whose recorded model
// differs from the active one, so upgrading from hash → API vectors migrates
// the brain automatically in the background.
func pickEmbedder(cfg config.Config) embed.Embedder {
	model := strings.TrimSpace(cfg.EmbedModel)
	if model == "" {
		model = embed.DefaultModelFireworks
	}

	var providers []embed.Embedder

	gw := strings.TrimRight(strings.TrimSpace(cfg.GatewayURL), "/")
	tok := os.Getenv("MATRIX_GATEWAY_TOKEN")
	if gw != "" && tok != "" && cfg.ActorDID != "" {
		e, err := embed.NewAPIEmbedder(embed.APIEmbedderConfig{
			Model:       model,
			Endpoint:    gw + "/v1/embeddings",
			APIKey:      tok,
			ProviderTag: "gateway",
			HTTPClient: &http.Client{
				Timeout:   30 * time.Second,
				Transport: gatewayHeaders{actorDID: cfg.ActorDID},
			},
		})
		if err == nil && probeEmbedder(e) {
			providers = append(providers, e)
		}
	}

	if os.Getenv("FIREWORKS_API_KEY") != "" {
		if e, err := embed.NewAPIEmbedder(embed.APIEmbedderConfig{Model: model}); err == nil && probeEmbedder(e) {
			providers = append(providers, e)
		}
	}

	// NewEmbedderChain appends a fresh HashEmbedder as the terminal fallback
	// if none of the providers above is already a *HashEmbedder. This preserves
	// the pre-P2-8 degradation behavior: when no real provider is available,
	// the chain resolves to the hash stub.
	return embed.NewEmbedderChain(embed.ChainConfig{
		Providers: providers,
		Timeout:   30 * time.Second,
	})
}

// probeEmbedder issues one tiny boot-time embed to prove the backend actually
// accepts our model/credentials. Without this, a misconfigured backend (e.g.
// a gateway still running a pre-v5 rate card that 403s the embed model) would
// be selected anyway — and every page-fault would then return NOTHING, which
// is strictly worse than the hash fallback.
func probeEmbedder(e embed.Embedder) bool {
	vec, err := e.Embed("neo embedder boot probe")
	return err == nil && len(vec) == e.Dim()
}

// gatewayHeaders stamps the X-Matrix-* metadata the gateway's auth and
// metering layers require, mirroring neo/internal/llm's chat path.
type gatewayHeaders struct {
	actorDID string
}

func (t gatewayHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Matrix-Actor-DID", t.actorDID)
	r.Header.Set("X-Matrix-Slot", "neo")
	return http.DefaultTransport.RoundTrip(r)
}
