// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClock is the clock seam: cache tiers and rate limits are exercised by
// moving time, never by sleeping.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *testClock {
	return &testClock{at: time.Date(2026, 7, 27, 14, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// upstream is a real HTTP server standing in for a vendor, counting the calls it
// actually receives. The count is the point: it proves the cache and the
// single-flight collapse are doing what the quota depends on.
type upstream struct {
	*httptest.Server
	hits   int64
	mu     sync.Mutex
	bodies map[string]string
	status map[string]int
	delay  time.Duration
}

func newUpstream(bodies map[string]string) *upstream {
	u := &upstream{bodies: bodies, status: map[string]int{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&u.hits, 1)
		if u.delay > 0 {
			time.Sleep(u.delay)
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "query" {
			path = "query:" + r.URL.Query().Get("function")
		}
		u.mu.Lock()
		body, ok := u.bodies[path]
		code := u.status[path]
		u.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if code != 0 {
			w.WriteHeader(code)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return u
}

func (u *upstream) Hits() int64 { return atomic.LoadInt64(&u.hits) }

func (u *upstream) Set(path, body string, status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies[path] = body
	if status != 0 {
		u.status[path] = status
	} else {
		delete(u.status, path)
	}
}

// newTestService wires the REAL service against real local upstreams.
func newTestService(t *testing.T, fmpBodies, alphaBodies map[string]string) (*Service, *upstream, *upstream, *testClock) {
	t.Helper()
	fmpUp := newUpstream(fmpBodies)
	alphaUp := newUpstream(alphaBodies)
	t.Cleanup(fmpUp.Close)
	t.Cleanup(alphaUp.Close)
	clock := newClock()
	svc := NewService(Config{
		FMPKey: "fmp-key", AlphaKey: "alpha-key",
		FMPBaseURL: fmpUp.URL, AlphaBaseURL: alphaUp.URL,
		HTTPClient: fmpUp.Client(), Now: clock.Now,
		RatePerSecond: 1000, Burst: 1000,
	})
	return svc, fmpUp, alphaUp, clock
}

// A market page opens many panels against one symbol in one instant. Inside the
// tier that must be ONE upstream call.
func TestServiceCacheCollapsesRepeatCallsWithinTheTier(t *testing.T) {
	svc, up, _, clock := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := svc.Quote(ctx, "user-1", "AAPL"); err != nil {
			t.Fatalf("quote %d: %v", i, err)
		}
	}
	if got := up.Hits(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 inside the tier", got)
	}

	// Past the tier, the next read refreshes.
	clock.Advance(TTLFor(ClassQuote) + time.Second)
	if _, err := svc.Quote(ctx, "user-1", "AAPL"); err != nil {
		t.Fatalf("post-expiry quote: %v", err)
	}
	if got := up.Hits(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after expiry", got)
	}
}

// Two users asking for the same symbol share the entry: market data is not
// user-specific, and sharing is what makes the agent and the UI one quota.
func TestServiceCacheIsSharedAcrossUsers(t *testing.T) {
	svc, up, _, _ := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	ctx := context.Background()

	if _, err := svc.Quote(ctx, "user-1", "AAPL"); err != nil {
		t.Fatalf("user-1: %v", err)
	}
	if _, err := svc.Quote(ctx, "user-2", "AAPL"); err != nil {
		t.Fatalf("user-2: %v", err)
	}
	if got := up.Hits(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 shared across users", got)
	}
}

// Case differences must not split the cache entry and double the quota spend.
func TestServiceCacheIsSymbolCaseInsensitive(t *testing.T) {
	svc, up, _, _ := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	ctx := context.Background()

	_, _ = svc.Quote(ctx, "u", "aapl")
	_, _ = svc.Quote(ctx, "u", "AAPL")
	if got := up.Hits(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 — case split the cache", got)
	}
}

// Concurrent identical requests must collapse to ONE upstream call, not race
// each other into N.
func TestServiceSingleFlightCollapsesConcurrentRequests(t *testing.T) {
	svc, up, _, _ := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	up.delay = 40 * time.Millisecond
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Quote(ctx, "user-1", "AAPL")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent quote %d: %v", i, err)
		}
	}
	if got := up.Hits(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 — single flight did not collapse", got)
	}
}

// When the primary refuses with a retryable failure the declared fallback
// answers, and the payload says so.
func TestServiceFallsBackToAlphaVantageAndSaysSo(t *testing.T) {
	const avQuote = `{"Global Quote":{"01. symbol":"AAPL","05. price":"232.80","09. change":"4.79","10. change percent":"2.1008%"}}`
	svc, fmpUp, alphaUp, _ := newTestService(t,
		map[string]string{"quote": `{"Error Message":"Limit Reached - upgrade your plan"}`},
		map[string]string{"query:GLOBAL_QUOTE": avQuote},
	)

	q, err := svc.Quote(context.Background(), "user-1", "AAPL")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Source.Provider != ProviderAlphaVantage || !q.Source.Fallback {
		t.Fatalf("source does not record the fallback: %+v", q.Source)
	}
	if q.Price == nil || *q.Price != 232.8 {
		t.Fatalf("fallback price = %v", q.Price)
	}
	if fmpUp.Hits() != 1 || alphaUp.Hits() != 1 {
		t.Fatalf("hits fmp=%d alpha=%d, want one each", fmpUp.Hits(), alphaUp.Hits())
	}
	stats := svc.Stats()
	if stats.Fallbacks != 1 {
		t.Fatalf("fallbacks = %d, want 1 recorded in the meter", stats.Fallbacks)
	}
}

// A non-retryable failure must NOT spend the fallback's quota.
func TestServiceDoesNotBurnTheFallbackOnANonRetryableFailure(t *testing.T) {
	svc, _, alphaUp, _ := newTestService(t,
		map[string]string{"quote": `[]`}, // the vendor genuinely has nothing
		map[string]string{"query:GLOBAL_QUOTE": `{"Global Quote":{"01. symbol":"X","05. price":"1.00"}}`},
	)

	_, err := svc.Quote(context.Background(), "user-1", "NOSUCH")
	if KindOf(err) != FailureNotFound {
		t.Fatalf("kind = %q, want not_found", KindOf(err))
	}
	if alphaUp.Hits() != 0 {
		t.Fatalf("fallback was called %d times for a not-found", alphaUp.Hits())
	}
}

// A vendor that starts refusing must not blank a panel that had a good answer a
// moment ago: the last good value stands in, marked stale.
func TestServiceServesStaleWhenTheUpstreamStartsRefusing(t *testing.T) {
	svc, up, _, clock := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	ctx := context.Background()

	first, err := svc.Quote(ctx, "user-1", "AAPL")
	if err != nil {
		t.Fatalf("first quote: %v", err)
	}
	if first.Source.Stale {
		t.Fatal("a fresh answer was marked stale")
	}

	up.Set("quote", `{"Error Message":"Limit Reached"}`, 0)
	clock.Advance(TTLFor(ClassQuote) + time.Second)

	stale, err := svc.Quote(ctx, "user-1", "AAPL")
	if err != nil {
		t.Fatalf("stale quote returned an error instead of the last good value: %v", err)
	}
	if !stale.Source.Stale {
		t.Fatal("the stale answer is not labelled stale")
	}
	if stale.Price == nil || *stale.Price != 232.8 {
		t.Fatalf("stale price = %v, want the last good value", stale.Price)
	}

	// Past the grace window the honest answer is the failure, not an
	// indefinitely-old price.
	clock.Advance(staleGrace + time.Minute)
	if _, err := svc.Quote(ctx, "user-1", "AAPL"); err == nil {
		t.Fatal("a value older than the grace window was still served")
	}
}

func TestServiceRateLimitIsOursAndTypedApart(t *testing.T) {
	up := newUpstream(map[string]string{"quote": docQuoteAAPL})
	defer up.Close()
	clock := newClock()
	svc := NewService(Config{
		FMPKey: "k", FMPBaseURL: up.URL, HTTPClient: up.Client(), Now: clock.Now,
		RatePerSecond: 1, Burst: 2,
	})
	ctx := context.Background()

	// The burst is spent on distinct symbols so the cache cannot mask the limit.
	if _, err := svc.Quote(ctx, "user-1", "AAA"); err != nil && KindOf(err) == FailureRateLimited {
		t.Fatalf("limited inside the burst: %v", err)
	}
	if _, err := svc.Quote(ctx, "user-1", "BBB"); err != nil && KindOf(err) == FailureRateLimited {
		t.Fatalf("limited inside the burst: %v", err)
	}
	_, err := svc.Quote(ctx, "user-1", "CCC")
	if KindOf(err) != FailureRateLimited {
		t.Fatalf("kind = %q, want OUR rate_limited (not a vendor throttle)", KindOf(err))
	}

	// A different user has their own bucket.
	if _, err := svc.Quote(ctx, "user-2", "AAA"); KindOf(err) == FailureRateLimited {
		t.Fatal("one user's spend limited another user")
	}

	// Tokens refill on the clock.
	clock.Advance(3 * time.Second)
	if _, err := svc.Quote(ctx, "user-1", "DDD"); KindOf(err) == FailureRateLimited {
		t.Fatal("the bucket did not refill")
	}
}

func TestServiceMeterCountsWhatTheOwnerAskedToSee(t *testing.T) {
	svc, _, _, clock := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, err := svc.Quote(ctx, "user-1", "AAPL"); err != nil {
			t.Fatalf("quote: %v", err)
		}
	}
	clock.Advance(TTLFor(ClassQuote) + time.Second)
	if _, err := svc.Quote(ctx, "user-2", "AAPL"); err != nil {
		t.Fatalf("quote: %v", err)
	}

	stats := svc.Stats()
	if stats.Requests != 5 {
		t.Fatalf("requests = %d, want 5", stats.Requests)
	}
	if stats.UpstreamCall != 2 {
		t.Fatalf("upstream calls = %d, want 2", stats.UpstreamCall)
	}
	if stats.CacheHits != 3 {
		t.Fatalf("cache hits = %d, want 3", stats.CacheHits)
	}
	if stats.CacheHitRate < 0.59 || stats.CacheHitRate > 0.61 {
		t.Fatalf("cache hit rate = %v, want ~0.6", stats.CacheHitRate)
	}
	if stats.Users != 2 {
		t.Fatalf("users = %d, want 2", stats.Users)
	}
	if len(stats.Endpoints) == 0 || stats.Endpoints[0].Endpoint != "quote" {
		t.Fatalf("endpoint rows wrong: %+v", stats.Endpoints)
	}
	if stats.CacheEntries != 1 {
		t.Fatalf("cache entries = %d, want 1", stats.CacheEntries)
	}
}

// The counters must be readable on a deployment with no keys at all — that is
// exactly when an operator needs to see what is being refused.
func TestServiceStatsReadableWithoutAnyKey(t *testing.T) {
	svc := NewService(Config{})
	_, err := svc.Quote(context.Background(), "user-1", "AAPL")
	if KindOf(err) != FailureNotConfigured {
		t.Fatalf("kind = %q, want not_configured", KindOf(err))
	}
	stats := svc.Stats()
	if stats.Requests != 1 || stats.Errors != 1 {
		t.Fatalf("stats did not record the refused call: %+v", stats)
	}
	if stats.Providers[string(ProviderFMP)] || stats.Providers[string(ProviderAlphaVantage)] {
		t.Fatalf("providers reported as configured without keys: %+v", stats.Providers)
	}
}

func TestServiceSeriesTierFollowsResolution(t *testing.T) {
	svc, up, _, clock := newTestService(t, map[string]string{
		"historical-chart/5min":     `[{"date":"2026-06-05 15:59:00","open":1,"low":1,"high":2,"close":2,"volume":10}]`,
		"historical-price-eod/full": `[{"symbol":"AAPL","date":"2026-06-05","open":1,"high":2,"low":1,"close":2,"volume":10}]`,
	}, nil)
	ctx := context.Background()

	if _, err := svc.Series(ctx, "u", "AAPL", Interval5Min, "", ""); err != nil {
		t.Fatalf("intraday series: %v", err)
	}
	if _, err := svc.Series(ctx, "u", "AAPL", IntervalDay, "", ""); err != nil {
		t.Fatalf("daily series: %v", err)
	}
	if up.Hits() != 2 {
		t.Fatalf("hits = %d, want 2 distinct series calls", up.Hits())
	}

	// Past the intraday tier the intraday series refreshes; the daily one, on a
	// much longer tier, still answers from cache.
	clock.Advance(TTLFor(ClassSeriesLive) + time.Second)
	if _, err := svc.Series(ctx, "u", "AAPL", Interval5Min, "", ""); err != nil {
		t.Fatalf("intraday refresh: %v", err)
	}
	if _, err := svc.Series(ctx, "u", "AAPL", IntervalDay, "", ""); err != nil {
		t.Fatalf("daily cached: %v", err)
	}
	if up.Hits() != 3 {
		t.Fatalf("hits = %d, want 3 — the daily tier should not have refreshed", up.Hits())
	}
}

func TestServiceBoardAndNewsAndFundamentalsRideTheSameLane(t *testing.T) {
	svc, _, _, _ := newTestService(t, map[string]string{
		"batch-quote":            `[{"symbol":"^GSPC","price":7521.25,"change":73.75,"volume":0}]`,
		"news/general-latest":    `[{"symbol":null,"publishedDate":"2026-06-06 12:40:12","publisher":"Reuters","title":"Oil plunges","image":"","site":"reuters.com","text":"Crude tumbled.","url":"https://example.test/oil"}]`,
		"key-metrics-ttm":        `[{"symbol":"AAPL","marketCap":3149833928000,"returnOnEquityTTM":1.45}]`,
		"ratios-ttm":             `[{"symbol":"AAPL","priceToEarningsRatioTTM":32.88,"dividendYieldTTM":0.0036}]`,
		"grades-consensus":       `[{"symbol":"AAPL","strongBuy":1,"buy":69,"hold":33,"sell":7,"strongSell":0,"consensus":"Buy"}]`,
		"price-target-consensus": `[{"symbol":"AAPL","targetHigh":400,"targetLow":253,"targetConsensus":323.82,"targetMedian":325}]`,
	}, nil)
	ctx := context.Background()

	board, err := svc.Board(ctx, "u", ClassIndex)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(board.Quotes) != 1 || board.Quotes[0].Symbol != "^GSPC" {
		t.Fatalf("board wrong: %+v", board)
	}
	if board.Source.Provider != ProviderFMP || board.Source.FetchedAt.IsZero() {
		t.Fatalf("board source not stamped: %+v", board.Source)
	}

	feed, err := svc.News(ctx, "u", NewsMarket, nil, 5)
	if err != nil {
		t.Fatalf("News: %v", err)
	}
	if len(feed.Items) != 1 || feed.Items[0].Publisher != "Reuters" {
		t.Fatalf("news wrong: %+v", feed.Items)
	}

	sum, err := svc.Fundamentals(ctx, "u", "AAPL")
	if err != nil {
		t.Fatalf("Fundamentals: %v", err)
	}
	if sum.PERatio == nil || *sum.PERatio != 32.88 {
		t.Fatalf("P/E = %v", sum.PERatio)
	}
	if sum.Consensus == nil || sum.Consensus.Consensus != "Buy" {
		t.Fatalf("consensus missing: %+v", sum.Consensus)
	}
	if sum.Consensus.TargetMedian == nil || *sum.Consensus.TargetMedian != 325 {
		t.Fatalf("price target missing: %+v", sum.Consensus)
	}
}

func TestServiceBoardCoversEveryClientMarketClassWithBoundedBatchQuote(t *testing.T) {
	svc, fmp, _, _ := newTestService(t, map[string]string{
		"batch-quote": `[{"symbol":"AAPL","price":232.8,"change":4.79,"volume":44489128,"exchange":"NASDAQ"}]`,
	}, nil)

	classes := []AssetClass{ClassEquity, ClassIndex, ClassCrypto, ClassForex, ClassCommodity}
	for _, class := range classes {
		board, err := svc.Board(context.Background(), "u", class)
		if err != nil {
			t.Fatalf("Board(%s): %v", class, err)
		}
		if len(board.Quotes) != 1 || board.Quotes[0].Class != class {
			t.Fatalf("Board(%s) = %+v", class, board.Quotes)
		}
	}
	if got := fmp.Hits(); got != int64(len(classes)) {
		t.Fatalf("FMP calls = %d, want one bounded quote call per market class", got)
	}
}
