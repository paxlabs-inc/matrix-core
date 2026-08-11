// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package routing decides per-request whether the gateway forwards a
// chat-completion to upstream Fireworks/Together (and which provider)
// or rejects it with a typed error.
//
// Two layers:
//
//  1. Free-tier whitelist: when the caller is NOT bring-your-own-key,
//     the requested model MUST be on the slot's whitelist (see
//     internal/rates.FreeTierWhitelist).
//
//  2. Provider selection: model id prefix decides Fireworks vs
//     Together upstream URL. Mirrors the daemon-side
//     MCL/llm.DetectProvider rules so the routing wire shape is
//     symmetric.
//
// Concurrency: Decider is immutable after construction; safe for
// concurrent use.
package routing

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"matrix/gateway/internal/rates"
	"matrix/gateway/internal/types"
)

// Provider identifies the upstream API we forward to.
type Provider int

const (
	ProviderUnknown Provider = iota
	ProviderFireworks
	ProviderTogether
	ProviderBaseten
	ProviderXai
	ProviderXiaomi
)

func (p Provider) String() string {
	switch p {
	case ProviderFireworks:
		return "fireworks"
	case ProviderTogether:
		return "together"
	case ProviderBaseten:
		return "baseten"
	case ProviderXai:
		return "xai"
	case ProviderXiaomi:
		return "xiaomi"
	}
	return "unknown"
}

// Default upstream endpoints. Overridable in tests via Decider opts.
const (
	DefaultFireworksChat       = "https://api.fireworks.ai/inference/v1/chat/completions"
	DefaultFireworksEmbeddings = "https://api.fireworks.ai/inference/v1/embeddings"
	DefaultTogetherChat        = "https://api.together.xyz/v1/chat/completions"
	DefaultTogetherEmbeddings  = "https://api.together.xyz/v1/embeddings"
	DefaultBasetenChat         = "https://inference.baseten.co/v1/chat/completions"
	DefaultBasetenEmbeddings   = "https://inference.baseten.co/v1/embeddings"
	// xAI (Grok) serves grok-* directly on an OpenAI-compatible /v1 surface.
	DefaultXaiChat       = "https://api.x.ai/v1/chat/completions"
	DefaultXaiEmbeddings = "https://api.x.ai/v1/embeddings"
	// Xiaomi serves MiMo directly on an OpenAI-compatible surface.
	DefaultXiaomiChat = "https://api.xiaomimimo.com/v1/chat/completions"
)

// Endpoint identifies which upstream URL to forward to.
type Endpoint string

const (
	EndpointChat      Endpoint = "chat"
	EndpointEmbedding Endpoint = "embedding"
)

// Decision captures the routing outcome for one request.
type Decision struct {
	// Provider selected upstream.
	Provider Provider

	// UpstreamURL is the full URL the proxy will forward to.
	UpstreamURL string

	// Model is the model id from the request body (passes through
	// unchanged so upstream sees the same value).
	Model string

	// FreeTier indicates the call counts against the actor's PAX
	// budget (true) vs uses the caller's BYO API key (false).
	FreeTier bool

	// UserAPIKey carries the BYO key when FreeTier == false. Empty
	// when FreeTier == true. The proxy uses this for the upstream
	// Authorization header when present; otherwise it falls back to
	// the gateway's own provider-side API key.
	UserAPIKey string

	// Slot is the requested ModelSlot label (compiler|planner|
	// executor|liaison|neo). Echoed for downstream metrics.
	Slot string

	// KindRoute is the executor sub-route label, optional.
	KindRoute string
}

// Errors surfaced by Decide. Callers map to HTTP status codes.
var (
	ErrFreeTierNotWhitelisted = errors.New("gateway.routing: model not on free-tier whitelist for slot")
	ErrUnknownProvider        = errors.New("gateway.routing: cannot detect provider for model")
	ErrInvalidSlot            = errors.New("gateway.routing: invalid X-Matrix-Slot value")
	ErrBYOMissingKey          = errors.New("gateway.routing: X-Matrix-BYO-API-Key=true but X-Matrix-User-API-Key is empty")
)

// Decider is the routing brain. Construct via New; safe for concurrent
// use after construction.
type Decider struct {
	freeTierOnly bool

	// chatURL / embeddingURL are per-provider override hooks for
	// tests. Map from Provider value to upstream URL.
	chatURL      map[Provider]string
	embeddingURL map[Provider]string
}

// Options controls Decider behavior.
type Options struct {
	// FreeTierOnly, when true, rejects every BYO request (forcing
	// every call to use the metered free tier). Useful for the
	// initial alpha posture; flip to false once BYO keys are
	// supported.
	FreeTierOnly bool

	// FireworksChatURL / FireworksEmbeddingsURL override the default
	// upstream URLs for tests. Empty falls back to the package
	// defaults.
	FireworksChatURL       string
	FireworksEmbeddingsURL string
	TogetherChatURL        string
	TogetherEmbeddingsURL  string
	BasetenChatURL         string
	BasetenEmbeddingsURL   string
	XaiChatURL             string
	XaiEmbeddingsURL       string
	XiaomiChatURL          string
}

// New constructs a Decider with the supplied options.
func New(opts Options) *Decider {
	pick := func(s, fallback string) string {
		if s == "" {
			return fallback
		}
		return s
	}
	return &Decider{
		freeTierOnly: opts.FreeTierOnly,
		chatURL: map[Provider]string{
			ProviderFireworks: pick(opts.FireworksChatURL, DefaultFireworksChat),
			ProviderTogether:  pick(opts.TogetherChatURL, DefaultTogetherChat),
			ProviderBaseten:   pick(opts.BasetenChatURL, DefaultBasetenChat),
			ProviderXai:       pick(opts.XaiChatURL, DefaultXaiChat),
			ProviderXiaomi:    pick(opts.XiaomiChatURL, DefaultXiaomiChat),
		},
		embeddingURL: map[Provider]string{
			ProviderFireworks: pick(opts.FireworksEmbeddingsURL, DefaultFireworksEmbeddings),
			ProviderTogether:  pick(opts.TogetherEmbeddingsURL, DefaultTogetherEmbeddings),
			ProviderBaseten:   pick(opts.BasetenEmbeddingsURL, DefaultBasetenEmbeddings),
			ProviderXai:       pick(opts.XaiEmbeddingsURL, DefaultXaiEmbeddings),
		},
	}
}

// Decide computes the routing outcome for a request. Returns a typed
// error on rejection so callers can map specific failure modes to
// distinct HTTP status codes (free-tier rejections are 403,
// missing-key bypasses are 400, unknown providers are 502).
func (d *Decider) Decide(r *http.Request, model string, ep Endpoint) (*Decision, error) {
	if r == nil {
		return nil, fmt.Errorf("gateway.routing: nil request")
	}
	slot := strings.TrimSpace(r.Header.Get(types.HeaderSlot))
	kind := strings.TrimSpace(r.Header.Get(types.HeaderKindRoute))
	if !validSlot(slot) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidSlot, slot)
	}

	byo, byoKey := readBYO(r)
	if byo && d.freeTierOnly {
		return nil, fmt.Errorf("gateway.routing: BYO key disabled in free-tier-only mode")
	}
	if byo && byoKey == "" {
		return nil, ErrBYOMissingKey
	}

	if !byo {
		if err := enforceFreeTier(slot, model); err != nil {
			return nil, err
		}
	}

	provider, err := detectProvider(model)
	if err != nil {
		return nil, err
	}

	urlMap := d.chatURL
	if ep == EndpointEmbedding {
		urlMap = d.embeddingURL
	}
	upstream, ok := urlMap[provider]
	if !ok {
		return nil, fmt.Errorf("%w: provider=%s endpoint=%s", ErrUnknownProvider, provider, ep)
	}

	return &Decision{
		Provider:    provider,
		UpstreamURL: upstream,
		Model:       model,
		FreeTier:    !byo,
		UserAPIKey:  byoKey,
		Slot:        slot,
		KindRoute:   kind,
	}, nil
}

// readBYO returns (byoFlag, userKey). Both empty/absent → metered.
func readBYO(r *http.Request) (byoFlag bool, userKey string) {
	flag := strings.TrimSpace(r.Header.Get(types.HeaderBYOAPIKey))
	if !strings.EqualFold(flag, "true") && flag != "1" && !strings.EqualFold(flag, "yes") {
		return false, ""
	}
	return true, strings.TrimSpace(r.Header.Get(types.HeaderUserAPIKey))
}

// enforceFreeTier rejects models outside the per-slot whitelist.
func enforceFreeTier(slot, model string) error {
	whitelist := rates.FreeTierWhitelist()
	allowed, ok := whitelist[slot]
	if !ok {
		return fmt.Errorf("%w: slot=%q model=%q (no whitelist for slot)",
			ErrFreeTierNotWhitelisted, slot, model)
	}
	for _, m := range allowed {
		if m == model {
			return nil
		}
	}
	return fmt.Errorf("%w: slot=%q model=%q allowed=%v",
		ErrFreeTierNotWhitelisted, slot, model, allowed)
}

// validSlot reports whether s is one of the known ModelSlot strings.
// Empty is invalid — every gateway request MUST declare a slot so
// the metering audit has provenance.
func validSlot(s string) bool {
	switch s {
	case types.SlotCompiler, types.SlotPlanner, types.SlotExecutor, types.SlotLiaison, types.SlotNeo, types.SlotCassandra, types.SlotCody:
		return true
	}
	return false
}

// detectProvider mirrors MCL/llm.DetectProvider. Fireworks gets the
// "accounts/fireworks/" prefix; Grok ("grok-*", e.g. "grok-4.3") routes to xAI
// — the primary chat provider — on its OpenAI-compatible /v1 surface, with the
// bare fleet id passed through unchanged (no native-id rewrite); the remaining
// generic "<vendor>/<model>" shape routes to Baseten. Bare non-grok model ids
// (no slash) are an error.
func detectProvider(model string) (Provider, error) {
	switch {
	case strings.HasPrefix(model, "accounts/fireworks/"):
		return ProviderFireworks, nil
	// nomic-ai/* embedding models are Fireworks-hosted but use the bare
	// vendor/model id on the embeddings API — they must not fall into the
	// generic '<vendor>/<model>' → Baseten branch below.
	case strings.HasPrefix(model, "nomic-ai/"):
		return ProviderFireworks, nil
	case strings.HasPrefix(strings.ToLower(model), "grok-"):
		return ProviderXai, nil
	case strings.EqualFold(strings.TrimSpace(model), "mimo-v2.5-pro"), strings.EqualFold(strings.TrimSpace(model), "xiaomimimo/mimo-v2.5-pro"):
		return ProviderXiaomi, nil
	case strings.Contains(model, "/"):
		return ProviderBaseten, nil
	}
	return ProviderUnknown, fmt.Errorf("%w: %q (expected '<vendor>/<model>', 'accounts/fireworks/...', or a 'grok-*' xAI id)",
		ErrUnknownProvider, model)
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
