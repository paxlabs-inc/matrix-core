// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"centra/router/internal/proxy"
)

// Logf is the router's logging shape.
type Logf func(format string, args ...any)

// Handler serves the finance lane under /finance/*. It is mounted on the
// router's PUBLIC mux behind the existing JWT middleware, so every request
// arrives with a verified subject — which is what the metering attributes and
// the rate limiter buckets on.
//
// The vendor keys live in the Service below this handler and are never echoed
// into a response, a header, or a log line.
type Handler struct {
	Service *Service
	Log     Logf
	// TrustSubjectHeader is set ONLY on the internal, admin-token-authed mount
	// that a per-user daemon's finance bridge calls. There the caller is a
	// trusted service naming the user it is acting for, so metering and rate
	// limiting can still attribute per user. It is never set on the public
	// JWT mount, where the subject comes from the verified token alone.
	TrustSubjectHeader bool
}

// SubjectHeader carries the acting user on the internal mount.
const SubjectHeader = "X-Matrix-Subject"

// NewHandler builds the public, JWT-authed handler.
func NewHandler(svc *Service, log Logf) *Handler {
	return &Handler{Service: svc, Log: log}
}

// NewInternalHandler builds the service-to-service handler for the per-user
// daemon's finance bridge. It shares the SAME Service — one cache, one quota,
// one metering record for the agent and the browser alike.
func NewInternalHandler(svc *Service, log Logf) *Handler {
	return &Handler{Service: svc, Log: log, TrustSubjectHeader: true}
}

// errorBody is the wire shape of a refusal. It carries the plain-language line
// and the machine-readable kind — never the vendor's raw text, which is for logs
// only and can quote the request.
type errorBody struct {
	Kind       FailureKind `json:"kind"`
	Message    string      `json:"message"`
	Provider   Provider    `json:"provider,omitempty"`
	RetryAfter int         `json:"retry_after_seconds,omitempty"`
}

func statusFor(kind FailureKind) int {
	switch kind {
	case FailureBadRequest:
		return http.StatusBadRequest
	case FailureNotFound:
		return http.StatusNotFound
	case FailureRateLimited:
		return http.StatusTooManyRequests
	case FailureNotConfigured, FailureThrottled:
		return http.StatusServiceUnavailable
	case FailureTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Market data is per-instrument, not per-user, but it is served behind auth;
	// no shared cache should hold it on our behalf.
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	f := FailureOf(err)
	if f == nil {
		h.writeJSON(w, http.StatusBadGateway, map[string]errorBody{"error": {
			Kind: FailureUpstream, Message: "Market data could not be loaded.",
		}})
		return
	}
	if h.Log != nil && f.Detail != "" {
		h.Log("finance: %s %s refused: %s (%s)", f.Provider, f.Endpoint, f.Message, f.Detail)
	}
	body := errorBody{Kind: f.Kind, Message: f.Message, Provider: f.Provider}
	if f.RetryAfter > 0 {
		body.RetryAfter = int(f.RetryAfter.Seconds())
		w.Header().Set("Retry-After", strconv.Itoa(body.RetryAfter))
	}
	h.writeJSON(w, statusFor(f.Kind), map[string]errorBody{"error": body})
}

// partial is how a composite panel reports itself: either data or an honest
// reason, never a silent blank.
type partial struct {
	Data  any        `json:"data,omitempty"`
	Error *errorBody `json:"error,omitempty"`
}

func part(v any, err error) partial {
	if err != nil {
		f := FailureOf(err)
		if f == nil {
			return partial{Error: &errorBody{Kind: FailureUpstream, Message: "This panel could not be loaded."}}
		}
		return partial{Error: &errorBody{Kind: f.Kind, Message: f.Message, Provider: f.Provider}}
	}
	return partial{Data: v}
}

func query(r *http.Request, name string) string {
	return strings.TrimSpace(r.URL.Query().Get(name))
}

func queryInt(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(query(r, name))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func symbolList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]errorBody{"error": {
			Kind: FailureBadRequest, Message: "Market data is read-only.",
		}})
		return
	}
	if h.Service == nil {
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]errorBody{"error": {
			Kind: FailureNotConfigured, Message: "Market data is not configured for this deployment.",
		}})
		return
	}

	ctx := r.Context()
	user := strings.TrimSpace(proxy.Subject(ctx))
	if user == "" && h.TrustSubjectHeader {
		user = strings.TrimSpace(r.Header.Get(SubjectHeader))
	}
	path := strings.TrimPrefix(r.URL.Path, "/internal")
	path = strings.Trim(strings.TrimPrefix(path, "/finance"), "/")
	svc := h.Service

	switch path {
	case "quote":
		out, err := svc.Quote(ctx, user, query(r, "symbol"))
		h.respond(w, out, err)

	case "quotes":
		out, err := svc.BatchQuote(ctx, user, symbolList(query(r, "symbols")))
		h.respond(w, out, err)

	case "extended":
		out, err := svc.ExtendedQuote(ctx, user, query(r, "symbol"))
		h.respond(w, out, err)

	case "change":
		out, err := svc.PriceChange(ctx, user, query(r, "symbol"))
		h.respond(w, out, err)

	case "series":
		symbol := query(r, "symbol")
		if raw := query(r, "range"); raw != "" {
			out, err := svc.SeriesForRange(ctx, user, symbol, ParseRange(raw))
			h.respond(w, out, err)
			return
		}
		out, err := svc.Series(ctx, user, symbol, Interval(query(r, "interval")), query(r, "from"), query(r, "to"))
		h.respond(w, out, err)

	case "profile":
		out, err := svc.Profile(ctx, user, query(r, "symbol"))
		h.respond(w, out, err)

	case "search":
		out, err := svc.Search(ctx, user, query(r, "q"), queryInt(r, "limit", 10))
		h.respond(w, out, err)

	case "movers":
		out, err := svc.Movers(ctx, user, MoverKind(strings.ToLower(query(r, "kind"))))
		h.respond(w, out, err)

	case "sectors":
		out, err := svc.Sectors(ctx, user, query(r, "date"), query(r, "exchange"))
		h.respond(w, out, err)

	case "board":
		out, err := svc.Board(ctx, user, AssetClass(strings.ToLower(query(r, "class"))))
		h.respond(w, out, err)

	case "news":
		scope := NewsScope(strings.ToLower(query(r, "scope")))
		if scope == "" {
			scope = NewsMarket
		}
		out, err := svc.News(ctx, user, scope, symbolList(query(r, "symbols")), queryInt(r, "limit", 20))
		h.respond(w, out, err)

	case "fundamentals":
		out, err := svc.Fundamentals(ctx, user, query(r, "symbol"))
		h.respond(w, out, err)

	case "earnings":
		out, err := svc.Earnings(ctx, user, query(r, "symbol"), queryInt(r, "limit", 20))
		h.respond(w, out, err)

	case "dividends":
		out, err := svc.Dividends(ctx, user, query(r, "symbol"), queryInt(r, "limit", 20))
		h.respond(w, out, err)

	case "status":
		out, err := svc.MarketStatus(ctx, user, query(r, "exchange"))
		h.respond(w, out, err)

	case "macro":
		out, err := svc.Macro(ctx, user, query(r, "name"), query(r, "from"), query(r, "to"))
		h.respond(w, out, err)

	case "treasury":
		out, err := svc.TreasuryRates(ctx, user, query(r, "from"), query(r, "to"))
		h.respond(w, out, err)

	case "symbol":
		h.symbolPage(w, r, user)

	case "home":
		h.marketsHome(w, r, user)

	case "diag":
		h.writeJSON(w, http.StatusOK, svc.Stats())

	default:
		h.writeJSON(w, http.StatusNotFound, map[string]errorBody{"error": {
			Kind: FailureNotFound, Message: "That market view does not exist.",
		}})
	}
}

func (h *Handler) respond(w http.ResponseWriter, out any, err error) {
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, out)
}

// symbolPage is the composite behind /finance/{symbol}: everything the page
// opens with, fetched CONCURRENTLY and reported per panel. One vendor gap greys
// one panel; the rest of the page still works.
func (h *Handler) symbolPage(w http.ResponseWriter, r *http.Request, user string) {
	symbol := query(r, "symbol")
	if symbol == "" {
		h.writeError(w, &Failure{Kind: FailureBadRequest, Endpoint: "symbol", Message: "No symbol was given."})
		return
	}
	rng := ParseRange(query(r, "range"))
	ctx := r.Context()
	svc := h.Service

	out := map[string]partial{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(name string, fn func() (any, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := fn()
			mu.Lock()
			out[name] = part(v, err)
			mu.Unlock()
		}()
	}

	run("quote", func() (any, error) { return svc.Quote(ctx, user, symbol) })
	run("series", func() (any, error) { return svc.SeriesForRange(ctx, user, symbol, rng) })
	run("profile", func() (any, error) { return svc.Profile(ctx, user, symbol) })
	run("fundamentals", func() (any, error) { return svc.Fundamentals(ctx, user, symbol) })
	run("extended", func() (any, error) { return svc.ExtendedQuote(ctx, user, symbol) })
	run("change", func() (any, error) { return svc.PriceChange(ctx, user, symbol) })
	run("news", func() (any, error) { return svc.News(ctx, user, NewsSymbols, []string{symbol}, 12) })
	wg.Wait()

	// The quote is the page's spine: without it there is no symbol page to show,
	// and saying so plainly beats rendering a shell of empty panels.
	if q, ok := out["quote"]; ok && q.Error != nil && q.Data == nil {
		h.writeJSON(w, statusFor(q.Error.Kind), map[string]any{"error": q.Error, "symbol": symbol})
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"symbol": symbol, "range": rng, "panels": out})
}

// marketsHome is the composite behind /finance: the index strip, the three
// ranked lists, the sector board, the market status and the news stream, each
// reported independently.
func (h *Handler) marketsHome(w http.ResponseWriter, r *http.Request, user string) {
	ctx := r.Context()
	svc := h.Service
	strip := symbolList(query(r, "symbols"))
	if len(strip) == 0 {
		strip = DefaultStrip
	}

	out := map[string]partial{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(name string, fn func() (any, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := fn()
			mu.Lock()
			out[name] = part(v, err)
			mu.Unlock()
		}()
	}

	run("strip", func() (any, error) { return svc.BatchQuote(ctx, user, strip) })
	run("gainers", func() (any, error) { return svc.Movers(ctx, user, MoversGainers) })
	run("losers", func() (any, error) { return svc.Movers(ctx, user, MoversLosers) })
	run("active", func() (any, error) { return svc.Movers(ctx, user, MoversActive) })
	run("sectors", func() (any, error) { return svc.Sectors(ctx, user, "", "") })
	run("status", func() (any, error) { return svc.MarketStatus(ctx, user, "") })
	run("news", func() (any, error) { return svc.News(ctx, user, NewsMarket, nil, 12) })
	wg.Wait()

	h.writeJSON(w, http.StatusOK, map[string]any{"panels": out})
}

// DefaultStrip is the markets home's opening set: the broad US benchmarks and
// the volatility index. It is a default, not a lock — the client may name its
// own strip.
var DefaultStrip = []string{"^GSPC", "^IXIC", "^DJI", "^VIX"}
