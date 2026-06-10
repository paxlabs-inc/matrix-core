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
// page-faulting, in order of preference:
//
//  1. the metered Matrix gateway /v1/embeddings route, when the gateway is
//     wired (MATRIX_GATEWAY_URL + MATRIX_GATEWAY_TOKEN + actor DID) — spend
//     is attributed to the actor under slot "neo" exactly like chat calls;
//  2. the provider directly, when FIREWORKS_API_KEY is set;
//  3. the deterministic hash embedder — retrieval quality degrades to
//     pseudo-lexical, but nothing breaks (the pre-v5 behavior).
//
// The cortex embedder worker lazily re-embeds memories whose recorded model
// differs from the active one, so upgrading from hash → API vectors migrates
// the brain automatically in the background.
func pickEmbedder(cfg config.Config) embed.Embedder {
	model := strings.TrimSpace(cfg.EmbedModel)
	if model == "" {
		model = embed.DefaultModelFireworks
	}

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
			return e
		}
	}

	if os.Getenv("FIREWORKS_API_KEY") != "" {
		if e, err := embed.NewAPIEmbedder(embed.APIEmbedderConfig{Model: model}); err == nil && probeEmbedder(e) {
			return e
		}
	}

	return embed.NewHashEmbedder()
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
