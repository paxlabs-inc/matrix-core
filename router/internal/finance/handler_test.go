// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix/router/internal/proxy"
)

// call drives the REAL handler with a subject in context, exactly as the JWT
// middleware supplies it upstream.
func get(t *testing.T, h *Handler, target, user string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r = r.WithContext(proxy.WithSubject(r.Context(), user))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
}

func newTestHandler(t *testing.T, fmpBodies, alphaBodies map[string]string) (*Handler, *upstream) {
	t.Helper()
	svc, up, _, _ := newTestService(t, fmpBodies, alphaBodies)
	return NewHandler(svc, nil), up
}

func TestHandlerServesTheNormalizedQuote(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{"quote": docQuoteAAPL}, nil)

	w := get(t, h, "/finance/quote?symbol=AAPL", "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var q Quote
	decode(t, w, &q)
	if q.Symbol != "AAPL" || q.Price == nil || *q.Price != 232.8 {
		t.Fatalf("quote wrong: %+v", q)
	}
	if q.Source.Provider != ProviderFMP {
		t.Fatalf("provider = %q", q.Source.Provider)
	}
	// The client must never be handed a vendor field name.
	if strings.Contains(w.Body.String(), "changePercentage") {
		t.Fatalf("a vendor field name reached the wire: %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache-control = %q", w.Header().Get("Cache-Control"))
	}
}

// The lane is read-only. Nothing about market data should accept a write.
func TestHandlerRefusesNonGET(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{"quote": docQuoteAAPL}, nil)

	r := httptest.NewRequest(http.MethodPost, "/finance/quote?symbol=AAPL", strings.NewReader("{}"))
	r = r.WithContext(proxy.WithSubject(r.Context(), "user-1"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if w.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow = %q", w.Header().Get("Allow"))
	}
}

func TestHandlerMapsEachFailureKindToItsStatus(t *testing.T) {
	h, up := newTestHandler(t, map[string]string{"quote": docQuoteAAPL}, nil)

	cases := []struct {
		name   string
		body   string
		target string
		want   int
		kind   FailureKind
	}{
		{"vendor has nothing", `[]`, "/finance/quote?symbol=NOPE", http.StatusNotFound, FailureNotFound},
		{"vendor throttles", `{"Error Message":"Limit Reached"}`, "/finance/quote?symbol=THR", http.StatusServiceUnavailable, FailureThrottled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up.Set("quote", tc.body, 0)
			w := get(t, h, tc.target, "user-1")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			var wrapper struct{ Error errorBody }
			decode(t, w, &wrapper)
			if wrapper.Error.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", wrapper.Error.Kind, tc.kind)
			}
			if wrapper.Error.Message == "" {
				t.Fatal("refusal carried no plain-language message")
			}
		})
	}

	// A missing symbol is the caller's error, not the vendor's.
	w := get(t, h, "/finance/quote", "user-1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// OUR per-user limit is a 429 and is never confused with a vendor throttle.
func TestHandlerRateLimitIs429(t *testing.T) {
	up := newUpstream(map[string]string{"quote": docQuoteAAPL})
	defer up.Close()
	clock := newClock()
	svc := NewService(Config{
		FMPKey: "k", FMPBaseURL: up.URL, HTTPClient: up.Client(), Now: clock.Now,
		RatePerSecond: 1, Burst: 1,
	})
	h := NewHandler(svc, nil)

	if w := get(t, h, "/finance/quote?symbol=AAA", "user-1"); w.Code != http.StatusOK {
		t.Fatalf("first call status = %d", w.Code)
	}
	w := get(t, h, "/finance/quote?symbol=BBB", "user-1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("a 429 carried no Retry-After")
	}
	var wrapper struct{ Error errorBody }
	decode(t, w, &wrapper)
	if wrapper.Error.Kind != FailureRateLimited {
		t.Fatalf("kind = %q, want rate_limited", wrapper.Error.Kind)
	}
}

// Rate limiting buckets on the JWT subject, so one user cannot spend another's
// allowance.
func TestHandlerRateLimitsPerSubject(t *testing.T) {
	up := newUpstream(map[string]string{"quote": docQuoteAAPL})
	defer up.Close()
	clock := newClock()
	svc := NewService(Config{
		FMPKey: "k", FMPBaseURL: up.URL, HTTPClient: up.Client(), Now: clock.Now,
		RatePerSecond: 1, Burst: 1,
	})
	h := NewHandler(svc, nil)

	_ = get(t, h, "/finance/quote?symbol=AAA", "user-1")
	if w := get(t, h, "/finance/quote?symbol=BBB", "user-1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("user-1 second call = %d, want 429", w.Code)
	}
	if w := get(t, h, "/finance/quote?symbol=CCC", "user-2"); w.Code == http.StatusTooManyRequests {
		t.Fatal("user-2 was limited by user-1's spend")
	}
}

// The symbol page fetches every panel concurrently and reports each one
// independently: a vendor gap greys ONE panel, not the page.
func TestHandlerSymbolPageDegradesPerPanel(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{
		"quote":                 docQuoteAAPL,
		"historical-chart/5min": `[{"date":"2026-06-05 15:59:00","open":307.89,"low":307.35,"high":307.94,"close":307.55,"volume":100179}]`,
		"profile":               docProfileAAPL,
		// fundamentals, extended, change and news are deliberately unrouted:
		// the vendor has nothing, and the page must still render.
	}, nil)

	w := get(t, h, "/finance/symbol?symbol=AAPL&range=1D", "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var page struct {
		Symbol string             `json:"symbol"`
		Range  string             `json:"range"`
		Panels map[string]partial `json:"panels"`
	}
	decode(t, w, &page)
	if page.Symbol != "AAPL" || page.Range != "1D" {
		t.Fatalf("page identity wrong: %+v", page)
	}
	for _, name := range []string{"quote", "series", "profile", "fundamentals", "extended", "change", "news"} {
		p, ok := page.Panels[name]
		if !ok {
			t.Fatalf("panel %q missing entirely", name)
		}
		if p.Data == nil && p.Error == nil {
			t.Fatalf("panel %q is neither data nor an honest reason", name)
		}
	}
	if page.Panels["quote"].Error != nil {
		t.Fatalf("quote panel failed: %+v", page.Panels["quote"].Error)
	}
	if page.Panels["fundamentals"].Error == nil {
		t.Fatal("the missing fundamentals panel reported no reason")
	}
	if page.Panels["fundamentals"].Error.Message == "" {
		t.Fatal("the failed panel carried no plain-language message")
	}
}

// Without the quote there is no symbol page; saying so beats a shell of empty
// panels.
func TestHandlerSymbolPageFailsWhenTheQuoteIsGone(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{"quote": `[]`}, nil)

	w := get(t, h, "/finance/symbol?symbol=NOPE", "user-1")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestHandlerMarketsHomeReportsEveryPanel(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{
		"batch-quote":         `[{"symbol":"^GSPC","name":"S&P 500","price":7521.25,"change":73.75,"changePercentage":0.99,"exchange":"INDEX","volume":0,"marketCap":0}]`,
		"biggest-gainers":     `[{"symbol":"LVWR","price":1.46,"name":"LiveWire Group","change":0.69,"changesPercentage":89.61,"exchange":"NYSE"}]`,
		"news/general-latest": `[{"symbol":null,"publishedDate":"2026-06-06 12:40:12","publisher":"Reuters","title":"Oil plunges","image":"","site":"reuters.com","text":"Crude tumbled.","url":"https://example.test/oil"}]`,
	}, nil)

	w := get(t, h, "/finance/home", "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var home struct {
		Panels map[string]partial `json:"panels"`
	}
	decode(t, w, &home)
	for _, name := range []string{"strip", "gainers", "losers", "active", "sectors", "status", "news"} {
		p, ok := home.Panels[name]
		if !ok {
			t.Fatalf("panel %q missing", name)
		}
		if p.Data == nil && p.Error == nil {
			t.Fatalf("panel %q is neither data nor a reason", name)
		}
	}
	if home.Panels["strip"].Error != nil {
		t.Fatalf("strip failed: %+v", home.Panels["strip"].Error)
	}
	// The unrouted panels must carry reasons, not silence.
	if home.Panels["losers"].Error == nil {
		t.Fatal("the missing losers panel reported no reason")
	}
}

func TestHandlerSeriesRangeChoosesResolutionServerSide(t *testing.T) {
	h, up := newTestHandler(t, map[string]string{
		"historical-chart/5min": `[
          {"date":"2026-06-05 15:59:00","open":307.89,"low":307.35,"high":307.94,"close":307.55,"volume":100179},
          {"date":"2026-06-04 15:59:00","open":300.00,"low":299.00,"high":301.00,"close":300.50,"volume":50000}
        ]`,
		"historical-price-eod/full": `[
          {"symbol":"AAPL","date":"2026-06-05","open":312.86,"high":315.17,"low":307.15,"close":307.34,"volume":65310502},
          {"symbol":"AAPL","date":"2026-06-04","open":310.00,"high":314.00,"low":309.00,"close":312.80,"volume":51000000}
        ]`,
	}, nil)

	// 1D asks the vendor for intraday bars and trims to the LAST session.
	w := get(t, h, "/finance/series?symbol=AAPL&range=1D", "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var s Series
	decode(t, w, &s)
	if s.Interval != Interval5Min {
		t.Fatalf("interval = %q, want 5min for 1D", s.Interval)
	}
	if len(s.Candles) != 1 {
		t.Fatalf("candles = %d, want only the most recent session", len(s.Candles))
	}

	// 1Y asks for daily bars instead — a different upstream endpoint entirely.
	before := up.Hits()
	w = get(t, h, "/finance/series?symbol=AAPL&range=1Y", "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("1Y status = %d: %s", w.Code, w.Body.String())
	}
	decode(t, w, &s)
	if s.Interval != IntervalDay {
		t.Fatalf("interval = %q, want 1day for 1Y", s.Interval)
	}
	if up.Hits() <= before {
		t.Fatal("the 1Y range reused the intraday cache entry")
	}
}

// MAX aggregates real daily bars into real monthly ones — first open, last
// close, extreme high/low, summed volume. Nothing interpolated.
func TestHandlerMaxRangeAggregatesHonestly(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{
		"historical-price-eod/full": `[
          {"symbol":"X","date":"2026-02-02","open":20,"high":30,"low":15,"close":25,"volume":300},
          {"symbol":"X","date":"2026-01-31","open":12,"high":18,"low":9,"close":16,"volume":200},
          {"symbol":"X","date":"2026-01-02","open":10,"high":14,"low":8,"close":11,"volume":100}
        ]`,
	}, nil)

	w := get(t, h, "/finance/series?symbol=X&range=MAX", "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var s Series
	decode(t, w, &s)
	if s.Interval != IntervalMonth {
		t.Fatalf("interval = %q, want 1month", s.Interval)
	}
	if len(s.Candles) != 2 {
		t.Fatalf("candles = %d, want one per month", len(s.Candles))
	}
	jan := s.Candles[0]
	if jan.Open != 10 {
		t.Fatalf("january open = %v, want the FIRST open", jan.Open)
	}
	if jan.Close != 16 {
		t.Fatalf("january close = %v, want the LAST close", jan.Close)
	}
	if jan.High != 18 || jan.Low != 8 {
		t.Fatalf("january extremes = %v/%v, want 18/8", jan.High, jan.Low)
	}
	if jan.Volume == nil || *jan.Volume != 300 {
		t.Fatalf("january volume = %v, want the sum 300", jan.Volume)
	}
}

func TestHandlerDiagnosticsReportTheLaneWithoutLeakingKeys(t *testing.T) {
	h, _ := newTestHandler(t, map[string]string{"quote": docQuoteAAPL}, nil)
	_ = get(t, h, "/finance/quote?symbol=AAPL", "user-1")
	_ = get(t, h, "/finance/quote?symbol=AAPL", "user-2")

	w := get(t, h, "/finance/diag", "user-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var stats Stats
	decode(t, w, &stats)
	if stats.Requests < 2 {
		t.Fatalf("requests = %d", stats.Requests)
	}
	if stats.CacheHits < 1 {
		t.Fatalf("cache hits = %d, want the shared entry counted", stats.CacheHits)
	}
	if len(stats.Endpoints) == 0 {
		t.Fatal("no per-endpoint rows")
	}
	body := w.Body.String()
	for _, secret := range []string{"fmp-key", "alpha-key", "apikey="} {
		if strings.Contains(body, secret) {
			t.Fatalf("the diagnostics surface leaked %q: %s", secret, body)
		}
	}
}

func TestHandlerUnknownViewIs404(t *testing.T) {
	h, _ := newTestHandler(t, nil, nil)
	w := get(t, h, "/finance/nonsense", "user-1")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// A deployment with no keys still serves, and says exactly what is missing in
// language a user can read.
func TestHandlerWithoutKeysAnswersHonestly(t *testing.T) {
	h := NewHandler(NewService(Config{}), nil)

	w := get(t, h, "/finance/quote?symbol=AAPL", "user-1")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var wrapper struct{ Error errorBody }
	decode(t, w, &wrapper)
	if wrapper.Error.Kind != FailureNotConfigured {
		t.Fatalf("kind = %q", wrapper.Error.Kind)
	}
	if !strings.Contains(wrapper.Error.Message, "not configured") {
		t.Fatalf("message = %q", wrapper.Error.Message)
	}
	// The env variable name is operator detail; it belongs in the log, not in a
	// user-facing payload.
	if strings.Contains(w.Body.String(), FMPKeyEnv) {
		t.Fatalf("the wire response named an env variable: %s", w.Body.String())
	}
}
