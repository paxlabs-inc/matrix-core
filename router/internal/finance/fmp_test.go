// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The bodies below are the DOCUMENTED example responses from
// /root/matrix/temp/FMP_API.md, verbatim. The tests drive the real client and
// the real normalizer against a real HTTP server serving them — no doubles.

const docQuoteAAPL = `[
  {
    "symbol": "AAPL",
    "name": "Apple Inc.",
    "price": 232.8,
    "changePercentage": 2.1008,
    "change": 4.79,
    "volume": 44489128,
    "dayLow": 226.65,
    "dayHigh": 233.13,
    "yearHigh": 260.1,
    "yearLow": 164.08,
    "marketCap": 3500823120000,
    "priceAvg50": 240.2278,
    "priceAvg200": 219.98755,
    "exchange": "NASDAQ",
    "open": 227.2,
    "previousClose": 228.01,
    "timestamp": 1738702801
  }
]`

// The documented ^VIX index quote — note marketCap 0, which is "no market cap"
// and must never render as a zero market cap.
const docQuoteVIX = `[
  {
    "symbol": "^VIX",
    "name": "CBOE Volatility Index",
    "price": 16.37,
    "changePercentage": -5.37572,
    "change": -0.93,
    "volume": 0,
    "dayLow": 16.02,
    "dayHigh": 17.22,
    "yearHigh": 60.13,
    "yearLow": 12.7,
    "marketCap": 0,
    "priceAvg50": 16.5992,
    "priceAvg200": 19.3432,
    "exchange": "INDEX",
    "open": 17.02,
    "previousClose": 17.3,
    "timestamp": 1761336901
  }
]`

const docProfileAAPL = `[
  {
    "symbol": "AAPL",
    "price": 262.82,
    "marketCap": 3900351299800,
    "beta": 1.086,
    "lastDividend": 1.05,
    "range": "169.21-265.29",
    "change": 3.24,
    "changePercentage": 1.24817,
    "volume": 36725325,
    "averageVolume": 44645993,
    "companyName": "Apple Inc.",
    "currency": "USD",
    "cik": "0000320193",
    "isin": "US0378331005",
    "cusip": "037833100",
    "exchangeFullName": "NASDAQ Global Select",
    "exchange": "NASDAQ",
    "industry": "Consumer Electronics",
    "website": "https://www.apple.com",
    "description": "Apple Inc. designs, manufactures, and markets smartphones.",
    "ceo": "Timothy D. Cook",
    "sector": "Technology",
    "country": "US",
    "fullTimeEmployees": "164000",
    "phone": "(408) 996-1010",
    "address": "One Apple Park Way",
    "city": "Cupertino",
    "state": "CA",
    "zip": "95014",
    "image": "https://images.financialmodelingprep.com/symbol/AAPL.png",
    "ipoDate": "1980-12-12",
    "defaultImage": false,
    "isEtf": false,
    "isActivelyTrading": true,
    "isAdr": false,
    "isFund": false
  }
]`

// fmpServer stands up a real HTTP server routing the documented paths, and
// returns the real client pointed at it. Every test drives production code from
// the request out.
func fmpServer(t *testing.T, routes map[string]string) (*FMP, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		seen = append(seen, path)
		body, ok := routes[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"Error Message":"not routed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewFMP("test-key", srv.URL, srv.Client()), &seen
}

func TestFMPQuoteNormalizesTheDocumentedPayload(t *testing.T) {
	c, _ := fmpServer(t, map[string]string{"quote": docQuoteAAPL})

	q, err := c.Quote(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Symbol != "AAPL" || q.Name != "Apple Inc." || q.Exchange != "NASDAQ" {
		t.Fatalf("identity wrong: %+v", q)
	}
	if q.Class != ClassEquity {
		t.Fatalf("class = %q, want equity", q.Class)
	}
	if q.Price == nil || *q.Price != 232.8 {
		t.Fatalf("price = %v, want 232.8", q.Price)
	}
	if q.ChangePercent == nil || *q.ChangePercent != 2.1008 {
		t.Fatalf("change percent = %v", q.ChangePercent)
	}
	if q.MarketCap == nil || *q.MarketCap != 3500823120000 {
		t.Fatalf("market cap = %v", q.MarketCap)
	}
	if q.AsOf == nil || q.AsOf.Unix() != 1738702801 {
		t.Fatalf("as-of = %v, want the vendor timestamp", q.AsOf)
	}
	if q.Source.Provider != ProviderFMP || q.Source.FetchedAt.IsZero() {
		t.Fatalf("source not stamped: %+v", q.Source)
	}
}

func TestFMPQuoteListUsesOneBoundedMultiSymbolRequest(t *testing.T) {
	var gotSymbol string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSymbol = r.URL.Query().Get("symbol")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"symbol":"^GSPC","price":7521.25,"change":73.75,"volume":0,"exchange":"INDEX"},
			{"symbol":"^VIX","price":16.37,"change":-0.93,"volume":0,"exchange":"INDEX"}
		]`))
	}))
	defer srv.Close()
	c := NewFMP("test-key", srv.URL, srv.Client())

	quotes, err := c.QuoteList(context.Background(), []string{"^GSPC", "^VIX"}, ClassIndex)
	if err != nil {
		t.Fatalf("QuoteList: %v", err)
	}
	if gotSymbol != "^GSPC,^VIX" {
		t.Fatalf("symbol query = %q, want one comma-separated request", gotSymbol)
	}
	if len(quotes) != 2 || quotes[0].Class != ClassIndex || quotes[1].Class != ClassIndex {
		t.Fatalf("quotes = %+v", quotes)
	}
}

// An index carries no market cap. The vendor says so with 0, and 0 must not
// reach the UI as a real figure.
func TestFMPZeroMarketCapIsAbsentNotZero(t *testing.T) {
	c, _ := fmpServer(t, map[string]string{"quote": docQuoteVIX})

	q, err := c.Quote(context.Background(), "^VIX")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.MarketCap != nil {
		t.Fatalf("market cap = %v, want absent for an index", *q.MarketCap)
	}
	if q.Class != ClassIndex {
		t.Fatalf("class = %q, want index", q.Class)
	}
	// Volume 0 IS a real figure for an index and stays present.
	if q.Volume == nil || *q.Volume != 0 {
		t.Fatalf("volume = %v, want a present zero", q.Volume)
	}
}

func TestFMPSeriesIsOrderedOldestFirst(t *testing.T) {
	// The vendor returns newest-first; the chart reads forward.
	body := `[
      {"date":"2026-06-05 15:59:00","open":307.89,"low":307.35,"high":307.94,"close":307.55,"volume":100179},
      {"date":"2026-06-05 15:58:00","open":307.50,"low":307.20,"high":307.95,"close":307.88,"volume":90000},
      {"date":"2026-06-05 15:57:00","open":307.10,"low":307.00,"high":307.60,"close":307.49,"volume":80000}
    ]`
	c, _ := fmpServer(t, map[string]string{"historical-chart/1min": body})

	s, err := c.Series(context.Background(), "AAPL", Interval1Min, "", "")
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(s.Candles) != 3 {
		t.Fatalf("candles = %d, want 3", len(s.Candles))
	}
	for i := 1; i < len(s.Candles); i++ {
		if !s.Candles[i].Time.After(s.Candles[i-1].Time) {
			t.Fatalf("series not ascending at %d: %v then %v", i, s.Candles[i-1].Time, s.Candles[i].Time)
		}
	}
	if s.Candles[0].Close != 307.49 {
		t.Fatalf("first close = %v, want the OLDEST bar 307.49", s.Candles[0].Close)
	}
	// The naive vendor stamp is read in the exchange zone, so 15:57 ET is 19:57Z.
	if got := s.Candles[0].Time.UTC().Hour(); got != 19 {
		t.Fatalf("hour = %d, want 19 (15:57 New York in UTC)", got)
	}
}

func TestFMPSeriesEODParsesDateOnlyStamps(t *testing.T) {
	body := `[
      {"symbol":"AAPL","date":"2026-06-05","open":312.86,"high":315.17,"low":307.15,"close":307.34,"volume":65310502,"change":-5.52,"changePercent":-1.76,"vwap":310.63},
      {"symbol":"AAPL","date":"2026-06-04","open":310.00,"high":314.00,"low":309.00,"close":312.80,"volume":51000000,"change":2.8,"changePercent":0.9,"vwap":311.10}
    ]`
	c, _ := fmpServer(t, map[string]string{"historical-price-eod/full": body})

	s, err := c.Series(context.Background(), "AAPL", IntervalDay, "2026-06-01", "2026-06-05")
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(s.Candles) != 2 || s.Candles[0].Close != 312.80 {
		t.Fatalf("EOD series wrong: %+v", s.Candles)
	}
	if s.Candles[0].Volume == nil || *s.Candles[0].Volume != 51000000 {
		t.Fatalf("volume missing: %+v", s.Candles[0])
	}
}

func TestFMPProfileParsesStringEmployeeCount(t *testing.T) {
	c, _ := fmpServer(t, map[string]string{"profile": docProfileAAPL})

	p, err := c.Profile(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if p.Employees == nil || *p.Employees != 164000 {
		t.Fatalf("employees = %v, want 164000 parsed from the vendor's string", p.Employees)
	}
	if p.CEO != "Timothy D. Cook" || p.Sector != "Technology" || p.IPODate != "1980-12-12" {
		t.Fatalf("profile fields wrong: %+v", p)
	}
	if p.ExchangeName != "NASDAQ Global Select" {
		t.Fatalf("exchange name = %q", p.ExchangeName)
	}
}

// A non-numeric headcount means the vendor does not have one. It must not become
// a zero-employee company.
func TestFMPProfileAbsentEmployeeCountStaysAbsent(t *testing.T) {
	body := `[{"symbol":"XYZ","companyName":"Example","fullTimeEmployees":"","exchange":"NASDAQ"}]`
	c, _ := fmpServer(t, map[string]string{"profile": body})

	p, err := c.Profile(context.Background(), "XYZ")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if p.Employees != nil {
		t.Fatalf("employees = %v, want absent", *p.Employees)
	}
}

func TestFMPEmptyArrayIsNotFoundNotEmptyData(t *testing.T) {
	c, _ := fmpServer(t, map[string]string{"quote": `[]`})

	_, err := c.Quote(context.Background(), "NOSUCH")
	if KindOf(err) != FailureNotFound {
		t.Fatalf("kind = %q, want not_found", KindOf(err))
	}
}

// FMP substitutes an error OBJECT for the documented array when it refuses a
// request. That must be classified, never decoded as data.
func TestFMPLimitObjectIsThrottledNotUnreadable(t *testing.T) {
	body := `{"Error Message":"Limit Reached - Your current plan allows 250 requests per day."}`
	c, _ := fmpServer(t, map[string]string{"quote": body})

	_, err := c.Quote(context.Background(), "AAPL")
	if KindOf(err) != FailureThrottled {
		t.Fatalf("kind = %q, want throttled", KindOf(err))
	}
	f := FailureOf(err)
	if f == nil || !strings.Contains(f.Message, "rate limiting") {
		t.Fatalf("message not plain-language: %+v", f)
	}
	if !f.Retryable() {
		t.Fatal("a throttle should be retryable against the fallback provider")
	}
}

func TestFMPMoversAndSectorsAndSearch(t *testing.T) {
	c, _ := fmpServer(t, map[string]string{
		"biggest-gainers":             `[{"symbol":"MOTS","price":0.0002,"name":"Motus GI Holdings, Inc.","change":0.0001,"changesPercentage":100,"exchange":"OTC"}]`,
		"sector-performance-snapshot": `[{"date":"2024-02-01","sector":"Basic Materials","exchange":"NASDAQ","averageChange":-0.31481377464310634}]`,
		"search-symbol":               `[{"symbol":"AAPL","name":"Apple Inc.","currency":"USD","exchangeFullName":"NASDAQ Global Select","exchange":"NASDAQ"}]`,
	})
	ctx := context.Background()

	movers, err := c.Movers(ctx, MoversGainers)
	if err != nil {
		t.Fatalf("Movers: %v", err)
	}
	if len(movers.Movers) != 1 || movers.Movers[0].Symbol != "MOTS" || movers.Kind != MoversGainers {
		t.Fatalf("movers wrong: %+v", movers)
	}

	board, err := c.Sectors(ctx, "2024-02-01", "NASDAQ")
	if err != nil {
		t.Fatalf("Sectors: %v", err)
	}
	if len(board.Sectors) != 1 || board.Sectors[0].Sector != "Basic Materials" {
		t.Fatalf("sectors wrong: %+v", board)
	}

	found, err := c.Search(ctx, "apple", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Matches) != 1 || found.Matches[0].Symbol != "AAPL" || found.Matches[0].Class != ClassEquity {
		t.Fatalf("search wrong: %+v", found)
	}
}

func TestFMPNewsParsesTheDocumentedStream(t *testing.T) {
	body := `[
      {
        "symbol": "AAPL",
        "publishedDate": "2026-06-06 14:13:36",
        "publisher": "TechCrunch",
        "title": "What to expect from WWDC 2026",
        "image": "https://images.financialmodelingprep.com/news/wwdc.jpg",
        "site": "techcrunch.com",
        "text": "As Apple's Worldwide Developers Conference approaches.",
        "url": "https://techcrunch.com/2026/06/06/wwdc/"
      }
    ]`
	c, _ := fmpServer(t, map[string]string{"news/stock": body})

	feed, err := c.SymbolNews(context.Background(), []string{"AAPL"}, 20)
	if err != nil {
		t.Fatalf("SymbolNews: %v", err)
	}
	if len(feed.Items) != 1 {
		t.Fatalf("items = %d", len(feed.Items))
	}
	item := feed.Items[0]
	if item.Publisher != "TechCrunch" || item.Site != "techcrunch.com" {
		t.Fatalf("attribution wrong: %+v", item)
	}
	if item.PublishedAt == nil {
		t.Fatal("published-at missing")
	}
	if len(item.Symbols) != 1 || item.Symbols[0] != "AAPL" {
		t.Fatalf("symbols = %v", item.Symbols)
	}
	// FMP does not score sentiment; the field stays absent rather than 0.
	if item.Sentiment != nil {
		t.Fatalf("sentiment = %+v, want absent for FMP", item.Sentiment)
	}
}

func TestFMPPriceChangeCarriesVendorWindows(t *testing.T) {
	body := `[{"symbol":"AAPL","1D":2.1008,"5D":-2.45946,"1M":-4.33925,"ytd":-4.53147,"1Y":24.04092,"max":181279.04168}]`
	c, _ := fmpServer(t, map[string]string{"stock-price-change": body})

	pc, err := c.PriceChange(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("PriceChange: %v", err)
	}
	if pc.Windows["1D"] != 2.1008 || pc.Windows["ytd"] != -4.53147 {
		t.Fatalf("windows wrong: %+v", pc.Windows)
	}
	if _, ok := pc.Windows["symbol"]; ok {
		t.Fatal("the symbol field leaked into the numeric windows")
	}
}

func TestFMPShortBoardDerivesPercentOnlyFromRealNumbers(t *testing.T) {
	body := `[{"symbol":"^GSPC","price":100,"change":10,"volume":0},{"symbol":"^NDX","price":50,"volume":0}]`
	c, _ := fmpServer(t, map[string]string{"batch-index-quotes": body})

	board, err := c.IndexBoard(context.Background())
	if err != nil {
		t.Fatalf("IndexBoard: %v", err)
	}
	if len(board) != 2 {
		t.Fatalf("board = %d rows", len(board))
	}
	if board[0].ChangePercent == nil || *board[0].ChangePercent < 11.10 || *board[0].ChangePercent > 11.12 {
		t.Fatalf("derived percent = %v, want ~11.111 from 10/90", board[0].ChangePercent)
	}
	// No change figure means no derivable percent — absent, not zero.
	if board[1].ChangePercent != nil {
		t.Fatalf("percent = %v, want absent when change is missing", *board[1].ChangePercent)
	}
}

// The lane boots without a key and says exactly what is missing.
func TestFMPWithoutAKeyDegradesHonestly(t *testing.T) {
	c := NewFMP("", "", nil)
	if c.Configured() {
		t.Fatal("client reports configured without a key")
	}
	_, err := c.Quote(context.Background(), "AAPL")
	f := FailureOf(err)
	if f == nil || f.Kind != FailureNotConfigured {
		t.Fatalf("kind = %q, want not_configured", KindOf(err))
	}
	if !strings.Contains(f.Detail, FMPKeyEnv) {
		t.Fatalf("detail %q does not name the missing variable", f.Detail)
	}
}

// The key rides in the query string, so any error carrying vendor text must be
// scrubbed before it reaches a log.
func TestFMPFailuresNeverCarryTheAPIKey(t *testing.T) {
	const secret = "super-secret-key-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// A vendor that echoes the request back is the leak path being closed.
		_, _ = w.Write([]byte("upstream failed for " + r.URL.String()))
	}))
	defer srv.Close()
	c := NewFMP(secret, srv.URL, srv.Client())

	_, err := c.Quote(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the API key leaked into an error: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("the key was not redacted: %v", err)
	}
}

func TestFMPTimeoutIsTypedNotGeneric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewFMP("k", srv.URL, srv.Client())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := c.Quote(ctx, "AAPL")
	if k := KindOf(err); k != FailureTimeout && k != FailureUpstream {
		t.Fatalf("kind = %q, want a typed timeout/upstream failure", k)
	}
}

func TestFMPAnalystsSurvivesAHalfCoveredSymbol(t *testing.T) {
	// Grades present, price target absent — a thinly covered symbol still gets
	// the half the vendor has.
	c, _ := fmpServer(t, map[string]string{
		"grades-consensus":       `[{"symbol":"XYZ","strongBuy":1,"buy":2,"hold":3,"sell":0,"strongSell":0,"consensus":"Buy"}]`,
		"price-target-consensus": `[]`,
	})

	a, err := c.Analysts(context.Background(), "XYZ")
	if err != nil {
		t.Fatalf("Analysts: %v", err)
	}
	if a.Consensus != "Buy" || a.Buy == nil || *a.Buy != 2 {
		t.Fatalf("grades wrong: %+v", a)
	}
	if a.TargetMedian != nil {
		t.Fatalf("target = %v, want absent", *a.TargetMedian)
	}
}

func TestFMPTreasuryRatesSplitIntoPerTenorSeries(t *testing.T) {
	body := `[{"date":"2026-06-05","month1":3.71,"year2":4.17,"year10":4.55,"year30":5.01}]`
	c, _ := fmpServer(t, map[string]string{"treasury-rates": body})

	curve, err := c.TreasuryRates(context.Background(), "", "")
	if err != nil {
		t.Fatalf("TreasuryRates: %v", err)
	}
	ten, ok := curve["year10"]
	if !ok || len(ten.Points) != 1 || ten.Points[0].Value != 4.55 {
		t.Fatalf("10y series wrong: %+v", curve)
	}
	if _, leaked := curve["date"]; leaked {
		t.Fatal("the date column became a series")
	}
}
