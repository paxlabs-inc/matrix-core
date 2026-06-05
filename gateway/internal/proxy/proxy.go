// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package proxy implements the gateway's HTTP surface. One http.Handler
// per (router-path × verb) attached at the cmd-level mux. Two endpoints
// are mounted:
//
//	POST /v1/chat/completions  →  upstream chat completions
//	POST /v1/embeddings        →  upstream embeddings
//
// Wire shape vs nginx:
//
//	external:  POST https://matrix.paxeer.app/gw/v1/chat/completions
//	nginx:     proxy_pass http://127.0.0.1:9090/  (strips /gw/)
//	gateway:   sees /v1/chat/completions on the loopback listener
//
// On every request the gateway (in order):
//
//  1. Authenticates the bearer token + X-Matrix-Actor-DID header.
//  2. Decodes the request body just enough to read `model` + `stream`.
//  3. Decides the upstream provider + URL, plus free-tier vs BYO mode.
//  4. Pre-flight rate-limit + budget gate (BYO bypasses both).
//  5. Streams the request body to upstream (preserving SSE).
//  6. On non-2xx upstream: forwards status + body verbatim, no debit.
//  7. On 2xx: extracts upstream `usage` block (final SSE chunk for
//     stream=true), prices via internal/rates, debits ledger, sets
//     X-Matrix-Cost-Pax + X-Matrix-Daily-{Spent,Remaining}-Pax
//     response headers, returns to client.
//
// Concurrency: the Server type is immutable after Configure; safe for
// concurrent Serve. Per-request state is goroutine-local.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"matrix/gateway/internal/auth"
	"matrix/gateway/internal/ledger"
	"matrix/gateway/internal/ratelimit"
	"matrix/gateway/internal/rates"
	"matrix/gateway/internal/routing"
	"matrix/gateway/internal/types"
)

// Server is the HTTP entry point.
type Server struct {
	auth         *auth.Authenticator
	router       *routing.Decider
	ledger       ledger.Ledger
	rl           *ratelimit.Limiter
	provider     ProviderKeys
	httpClient   *http.Client
	logf         func(event string, fields map[string]any)
	disabled     func() bool
	maxBodyBytes int64
	now          func() time.Time
	preEstimate  string // pre-flight projected cost for budget gate
}

// ProviderKeys holds the gateway's own upstream API keys (used when
// the caller is metered and not BYO). Empty values fall back to env
// lookups by the proxy at request time.
type ProviderKeys struct {
	FireworksKey string
	TogetherKey  string
}

// Options drives Server construction.
type Options struct {
	Auth        *auth.Authenticator
	Router      *routing.Decider
	Ledger      ledger.Ledger
	RateLimiter *ratelimit.Limiter
	Provider    ProviderKeys
	HTTPClient  *http.Client
	// Logf is invoked for every audit-worthy event. nil → swallow.
	Logf func(event string, fields map[string]any)
	// Disabled is consulted on every request; when it returns true,
	// the gateway responds 503 to everything except /healthz. Used by
	// the kill switch (env MATRIX_GATEWAY_DISABLED=true).
	Disabled func() bool
	// MaxBodyBytes caps request body size. Default 1 MiB.
	MaxBodyBytes int64
	// Now is the clock function (test injection). nil → time.Now.
	Now func() time.Time
	// PreEstimatePax is a fixed PAX-string used as the pre-flight
	// projected cost for budget gating. Empty defaults to "0.5"
	// (covers the common-case classify call upper bound).
	PreEstimatePax string
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	if opts.Auth == nil {
		return nil, fmt.Errorf("gateway.proxy: nil Auth")
	}
	if opts.Router == nil {
		return nil, fmt.Errorf("gateway.proxy: nil Router")
	}
	if opts.Ledger == nil {
		return nil, fmt.Errorf("gateway.proxy: nil Ledger")
	}
	rl := opts.RateLimiter
	if rl == nil {
		rl = ratelimit.New(0, 0) // disabled by default
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	max := opts.MaxBodyBytes
	if max <= 0 {
		max = 1 << 20
	}
	pre := opts.PreEstimatePax
	if pre == "" {
		pre = "0.5"
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, map[string]any) {}
	}
	disabled := opts.Disabled
	if disabled == nil {
		disabled = func() bool { return false }
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Server{
		auth:         opts.Auth,
		router:       opts.Router,
		ledger:       opts.Ledger,
		rl:           rl,
		provider:     opts.Provider,
		httpClient:   hc,
		logf:         logf,
		disabled:     disabled,
		maxBodyBytes: max,
		now:          now,
		preEstimate:  pre,
	}, nil
}

// Mux returns an *http.ServeMux mounting the gateway's two endpoints.
// Callers wrap with their own middleware (CORS, request id) if needed.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/chat/completions", s.makeHandler(routing.EndpointChat))
	mux.HandleFunc("/v1/embeddings", s.makeHandler(routing.EndpointEmbedding))
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.disabled() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"disabled"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) makeHandler(ep routing.Endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.disabled() {
			http.Error(w, `{"error":"gateway_disabled"}`, http.StatusServiceUnavailable)
			return
		}
		s.handleProxy(w, r, ep)
	}
}

// handleProxy is the request lifecycle.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, ep routing.Endpoint) {
	ctx := r.Context()

	actor, err := s.auth.Verify(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if err := s.auth.VerifySignature(r, actor); err != nil {
		writeJSONErr(w, http.StatusUnauthorized, "signature_invalid", err.Error())
		return
	}

	// Rate-limit per actor BEFORE reading the body (1 token = 1 call).
	if !s.rl.Allow(actor) {
		writeJSONErr(w, http.StatusTooManyRequests, "rate_limited",
			"per-actor rate limit exceeded")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBodyBytes+1))
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "body_read", err.Error())
		return
	}
	if int64(len(body)) > s.maxBodyBytes {
		writeJSONErr(w, http.StatusRequestEntityTooLarge, "body_too_large",
			fmt.Sprintf("body exceeds %d bytes", s.maxBodyBytes))
		return
	}

	var head types.ChatCompletionRequest
	if err := json.Unmarshal(body, &head); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "body_invalid_json", err.Error())
		return
	}

	decision, err := s.router.Decide(r, head.Model, ep)
	if err != nil {
		switch {
		case errors.Is(err, routing.ErrFreeTierNotWhitelisted):
			writeJSONErr(w, http.StatusForbidden, "model_not_whitelisted", err.Error())
		case errors.Is(err, routing.ErrInvalidSlot):
			writeJSONErr(w, http.StatusBadRequest, "invalid_slot", err.Error())
		case errors.Is(err, routing.ErrBYOMissingKey):
			writeJSONErr(w, http.StatusBadRequest, "byo_missing_key", err.Error())
		default:
			writeJSONErr(w, http.StatusBadGateway, "routing_error", err.Error())
		}
		return
	}

	intentID := strings.TrimSpace(r.Header.Get(types.HeaderIntentID))
	goalID := strings.TrimSpace(r.Header.Get(types.HeaderGoalID))

	// Pre-flight budget gate. BYO bypasses (caller's own key, no PAX).
	var (
		preSpent string
		preCap   string
	)
	if decision.FreeTier {
		spent, capErr := s.ledger.DailySpend(ctx, actor, s.now())
		if capErr != nil {
			writeJSONErr(w, http.StatusInternalServerError, "ledger_read", capErr.Error())
			return
		}
		cap, capErr := s.ledger.DailyCap(ctx, actor)
		if capErr != nil {
			writeJSONErr(w, http.StatusInternalServerError, "ledger_read", capErr.Error())
			return
		}
		preSpent = spent
		preCap = cap
		_, exhausted, bErr := ledger.CheckBudget(spent, s.preEstimate, cap)
		if bErr != nil {
			writeJSONErr(w, http.StatusInternalServerError, "budget_calc", bErr.Error())
			return
		}
		if exhausted {
			s.logf("gateway.budget.exhausted", map[string]any{
				"actor": actor, "spent": spent, "cap": cap,
			})
			writeBudgetExhausted(w, spent, cap)
			return
		}
	}

	// Forward upstream. Streaming requests pipe the response body
	// through with no buffering; non-streaming requests buffer fully
	// so we can decode `usage` and stamp cost headers BEFORE writing
	// status (HTTP semantics — headers must precede body).
	//
	// Streaming callers (the executor slot is the only one today) get
	// stream_options.include_usage forced on: Fireworks + Together emit
	// the token `usage` block solely in the FINAL SSE chunk, and only
	// when asked. Without it handleStreaming scans no usage, maybeDebit
	// writes no ledger row, and the call slips past the daily budget
	// cap — so we enforce it at the gateway regardless of client intent.
	upstreamBody := body
	if head.Stream {
		upstreamBody = ensureStreamUsage(body)
	}
	upstream, err := s.buildUpstreamRequest(ctx, r, decision, upstreamBody)
	if err != nil {
		writeJSONErr(w, http.StatusBadGateway, "upstream_build", err.Error())
		return
	}

	resp, err := s.httpClient.Do(upstream)
	if err != nil {
		writeJSONErr(w, http.StatusBadGateway, "upstream_call", err.Error())
		return
	}
	defer resp.Body.Close()

	// Forward non-2xx without debiting.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		s.logf("gateway.upstream.error", map[string]any{
			"actor": actor, "status": resp.StatusCode, "model": decision.Model,
		})
		return
	}

	if head.Stream {
		s.handleStreaming(w, resp, decision, ledgerCtx{
			ctx:      ctx,
			actor:    actor,
			intentID: intentID,
			goalID:   goalID,
			preSpent: preSpent,
			preCap:   preCap,
		})
		return
	}
	s.handleBuffered(w, resp, decision, ledgerCtx{
		ctx:      ctx,
		actor:    actor,
		intentID: intentID,
		goalID:   goalID,
		preSpent: preSpent,
		preCap:   preCap,
	})
}

// ledgerCtx bundles the per-request fields that the cost-debit step
// needs after the upstream call returns.
type ledgerCtx struct {
	ctx      context.Context
	actor    string
	intentID string
	goalID   string
	preSpent string
	preCap   string
}

// handleBuffered reads upstream response fully, extracts usage, debits
// ledger, then forwards body + cost headers to the client.
func (s *Server) handleBuffered(w http.ResponseWriter, resp *http.Response, dec *routing.Decision, lc ledgerCtx) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSONErr(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	usage := extractUsage(body)
	cost, costErr := s.maybeDebit(dec, usage, lc)

	copyHeaders(w.Header(), resp.Header)
	if costErr == nil {
		s.stampCostHeaders(w.Header(), cost, lc)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// handleStreaming pipes the SSE response through to the client while
// scanning for the trailing `data: {"usage":...}` chunk so cost can
// be extracted + debited BEFORE the stream terminates from the
// client's perspective. Cost headers can NOT be added after the
// initial 200 OK is flushed, so streaming clients must read trailing
// SSE events `data: {"matrix": {"cost_pax": "..."}}` if they want the
// cost surface — kept off the wire for v1; the daemon receives cost
// via the per-call ledger snapshot endpoint instead.
func (s *Server) handleStreaming(w http.ResponseWriter, resp *http.Response, dec *routing.Decision, lc ledgerCtx) {
	// Set up SSE-friendly response headers + flush early.
	header := w.Header()
	copyHeaders(header, resp.Header)
	header.Set("X-Matrix-Stream", "true")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewReader(resp.Body)
	var lastUsage *types.UpstreamUsage
	for {
		chunk, err := scanner.ReadBytes('\n')
		if len(chunk) > 0 {
			if u, ok := scanUsageFromChunk(chunk); ok {
				lastUsage = u
			}
			if _, werr := w.Write(chunk); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			s.logf("gateway.upstream.stream_err", map[string]any{
				"actor": lc.actor, "error": err.Error(),
			})
			return
		}
	}
	if lastUsage != nil {
		_, _ = s.maybeDebit(dec, lastUsage, lc)
	}
}

// scanUsageFromChunk attempts to parse a `data: {...}` SSE chunk for
// a `usage` field. Returns the usage + true on success.
func scanUsageFromChunk(b []byte) (*types.UpstreamUsage, bool) {
	line := bytes.TrimSpace(b)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	var env types.UpstreamResponseEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, false
	}
	if env.Usage == nil || env.Usage.TotalTokens == 0 {
		return nil, false
	}
	return env.Usage, true
}

// extractUsage reads `usage` from a buffered (non-stream) response.
func extractUsage(body []byte) *types.UpstreamUsage {
	var env types.UpstreamResponseEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	return env.Usage
}

// ensureStreamUsage rewrites a streaming chat-completions request body to
// set stream_options.include_usage=true. Fireworks + Together emit the
// token `usage` block only in the trailing SSE chunk, and only when this
// flag is present; handleStreaming relies on it to debit the ledger.
//
// All other request fields are preserved verbatim. An existing
// stream_options object is merged (not clobbered) so caller-set fields
// survive. Fail-open: a body that isn't a JSON object is returned
// untouched — metering completeness must never break a live request.
func ensureStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok {
		// Tolerate a non-object value (e.g. null): opts stays empty and
		// is overwritten with a valid object below.
		_ = json.Unmarshal(raw, &opts)
	}
	opts["include_usage"] = json.RawMessage("true")
	optBytes, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	m["stream_options"] = json.RawMessage(optBytes)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// maybeDebit prices the call + writes a ledger row, returning the
// cost-pax string for response-header stamping. BYO calls return ""
// (no debit; caller pays direct). Errors are logged but not returned
// to the client (the call already succeeded upstream — losing the
// ledger row is preferable to a confusing client-side error).
func (s *Server) maybeDebit(dec *routing.Decision, usage *types.UpstreamUsage, lc ledgerCtx) (string, error) {
	if !dec.FreeTier {
		return "", nil
	}
	if usage == nil {
		s.logf("gateway.usage.missing", map[string]any{
			"actor": lc.actor, "model": dec.Model,
		})
		return "", nil
	}
	cost, err := rates.Cost(dec.Model, usage.PromptTokens, usage.CompletionTokens)
	if err != nil {
		s.logf("gateway.cost.error", map[string]any{
			"actor": lc.actor, "model": dec.Model, "error": err.Error(),
		})
		return "", err
	}
	entry := ledger.Entry{
		ActorDID:         lc.actor,
		IntentID:         lc.intentID,
		GoalID:           lc.goalID,
		Model:            dec.Model,
		Slot:             dec.Slot,
		KindRoute:        dec.KindRoute,
		TokensInput:      usage.PromptTokens,
		TokensOutput:     usage.CompletionTokens,
		CostPax:          cost,
		RateTableVersion: rates.RateTableVersion,
		OccurredAt:       s.now().UTC(),
	}
	// The debit is a post-response side-effect. On streamed calls the
	// client (daemon) closes the connection the instant it reads
	// `data: [DONE]`, cancelling lc.ctx (= r.Context()) before this
	// Postgres insert lands — losing the executor row and silently
	// under-counting daily spend (observed in prod as record_err
	// "insert: context canceled"). Detach from the request's
	// cancellation with a bounded timeout so the row is durably written;
	// request-scoped values (tracing) survive via WithoutCancel.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(lc.ctx), 5*time.Second)
	defer cancel()
	if err := s.ledger.Record(recCtx, entry); err != nil {
		s.logf("gateway.ledger.record_err", map[string]any{
			"actor": lc.actor, "error": err.Error(),
		})
		return cost, err
	}
	return cost, nil
}

// stampCostHeaders sets the X-Matrix-Cost-Pax / Daily-Spent /
// Daily-Remaining headers on the response. Best-effort: any
// arithmetic failure logs but never blocks the response.
func (s *Server) stampCostHeaders(h http.Header, cost string, lc ledgerCtx) {
	if cost == "" {
		return
	}
	h.Set(types.HeaderCostPax, cost)
	h.Set(types.HeaderRateTableVersion, fmt.Sprintf("%d", rates.RateTableVersion))
	if lc.preSpent == "" || lc.preCap == "" {
		return
	}
	totalSpent, err := rates.AddPax(lc.preSpent, cost)
	if err != nil {
		return
	}
	h.Set(types.HeaderDailySpentPax, totalSpent)
	if rem, err := rates.SubPax(lc.preCap, totalSpent); err == nil {
		h.Set(types.HeaderDailyRemainingPax, rem)
	}
}

// buildUpstreamRequest copies the inbound request body to a new
// upstream request bound to dec.UpstreamURL. Sets Authorization +
// content type. BYO calls use the caller's UserAPIKey; metered
// calls use the gateway's own provider key.
func (s *Server) buildUpstreamRequest(ctx context.Context, r *http.Request, dec *routing.Decision, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dec.UpstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gateway.proxy: build upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", r.Header.Get("Accept"))
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	var apiKey string
	if dec.UserAPIKey != "" {
		apiKey = dec.UserAPIKey
	} else {
		switch dec.Provider {
		case routing.ProviderFireworks:
			apiKey = s.provider.FireworksKey
		case routing.ProviderTogether:
			apiKey = s.provider.TogetherKey
		}
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

// writeAuthError maps auth failures to JSON 401/400.
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrMissingActor),
		errors.Is(err, auth.ErrMalformedActor):
		writeJSONErr(w, http.StatusBadRequest, "actor_invalid", err.Error())
	case errors.Is(err, auth.ErrUnauthorized):
		writeJSONErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
	default:
		writeJSONErr(w, http.StatusUnauthorized, "auth_error", err.Error())
	}
}

func writeJSONErr(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{
		"error":   kind,
		"message": msg,
	})
	_, _ = w.Write(body)
}

func writeBudgetExhausted(w http.ResponseWriter, spent, cap string) {
	body, _ := json.Marshal(types.BudgetExhaustedResponse{
		Error:    "budget_exhausted",
		SpentPax: spent,
		LimitPax: cap,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(types.HeaderDailySpentPax, spent)
	w.Header().Set(types.HeaderDailyRemainingPax, "0")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// copyHeaders copies non-hop-by-hop headers from src to dst. Skips
// hop headers that are illegal to forward per RFC 7230 §6.1, and
// drops any Authorization header that the upstream may have echoed
// (we never want to leak provider-side credentials downstream).
func copyHeaders(dst, src http.Header) {
	hopByHop := map[string]struct{}{
		"Connection":          {},
		"Proxy-Connection":    {},
		"Keep-Alive":          {},
		"Te":                  {},
		"Trailer":             {},
		"Transfer-Encoding":   {},
		"Upgrade":             {},
		"Proxy-Authorization": {},
		"Authorization":       {}, // never leak upstream creds
	}
	for k, vs := range src {
		if _, drop := hopByHop[http.CanonicalHeaderKey(k)]; drop {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
