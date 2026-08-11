// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// alphaServer stands up a real HTTP server that dispatches on the documented
// `function` query parameter, exactly as the vendor does, and returns the real
// client pointed at it.
func alphaServer(t *testing.T, byFunction map[string]string) *AlphaVantage {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := byFunction[r.URL.Query().Get("function")]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Error Message":"Invalid API call."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewAlphaVantage("test-key", srv.URL, srv.Client())
}

// THE guard. Alpha Vantage answers a throttled request with HTTP 200 and a prose
// body. Parsed naively that becomes an empty series — a flat chart presented as
// real market data. It must be a typed throttle instead.
func TestAlphaVantageThrottleBodyIsAFailureNotAnEmptySeries(t *testing.T) {
	const note = `{"Note":"Thank you for using Alpha Vantage! Our standard API call frequency is 5 calls per minute and 500 calls per day."}`
	c := alphaServer(t, map[string]string{"TIME_SERIES_INTRADAY": note})

	series, err := c.Series(context.Background(), "IBM", Interval5Min, false)
	if series != nil {
		t.Fatalf("a throttle produced a series: %+v", series)
	}
	if KindOf(err) != FailureThrottled {
		t.Fatalf("kind = %q, want throttled", KindOf(err))
	}
	f := FailureOf(err)
	if f == nil || !strings.Contains(f.Message, "rate limiting") {
		t.Fatalf("message not plain-language: %+v", f)
	}
	if !f.Retryable() {
		t.Fatal("a throttle should be retryable")
	}
}

// The same guard on the newer "Information" phrasing, which the vendor uses for
// the daily-cap refusal.
func TestAlphaVantageInformationThrottleIsCaught(t *testing.T) {
	const info = `{"Information":"We have detected your API key as demo. Our standard API rate limit is 25 requests per day."}`
	c := alphaServer(t, map[string]string{"GLOBAL_QUOTE": info})

	_, err := c.GlobalQuote(context.Background(), "IBM")
	if KindOf(err) != FailureThrottled {
		t.Fatalf("kind = %q, want throttled", KindOf(err))
	}
}

// A premium-endpoint notice is a configuration fact, not a retryable throttle —
// retrying it forever would burn quota against a wall.
func TestAlphaVantagePremiumNoticeIsNotConfiguredNotThrottled(t *testing.T) {
	const premium = `{"Information":"Thank you for using Alpha Vantage! This is a premium endpoint. You may subscribe to any of the premium plans."}`
	c := alphaServer(t, map[string]string{"TIME_SERIES_INTRADAY": premium})

	_, err := c.Series(context.Background(), "IBM", Interval5Min, false)
	if KindOf(err) != FailureNotConfigured {
		t.Fatalf("kind = %q, want not_configured", KindOf(err))
	}
	f := FailureOf(err)
	if f == nil || !strings.Contains(f.Message, "plan") {
		t.Fatalf("message should name the plan: %+v", f)
	}
}

func TestAlphaVantageErrorMessageIsABadRequest(t *testing.T) {
	c := alphaServer(t, map[string]string{"GLOBAL_QUOTE": `{"Error Message":"Invalid API call. Please retry."}`})

	_, err := c.GlobalQuote(context.Background(), "NOSUCH")
	if KindOf(err) != FailureBadRequest {
		t.Fatalf("kind = %q, want bad_request", KindOf(err))
	}
}

func TestAlphaVantageGlobalQuoteParsesOrdinalFields(t *testing.T) {
	const quote = `{
      "Global Quote": {
        "01. symbol": "IBM",
        "02. open": "227.2000",
        "03. high": "233.1300",
        "04. low": "226.6500",
        "05. price": "232.8000",
        "06. volume": "44489128",
        "07. latest trading day": "2026-06-05",
        "08. previous close": "228.0100",
        "09. change": "4.7900",
        "10. change percent": "2.1008%"
      }
    }`
	c := alphaServer(t, map[string]string{"GLOBAL_QUOTE": quote})

	q, err := c.GlobalQuote(context.Background(), "IBM")
	if err != nil {
		t.Fatalf("GlobalQuote: %v", err)
	}
	if q.Symbol != "IBM" {
		t.Fatalf("symbol = %q", q.Symbol)
	}
	if q.Price == nil || *q.Price != 232.8 {
		t.Fatalf("price = %v", q.Price)
	}
	// The percent arrives with a trailing sign that must not defeat the parse.
	if q.ChangePercent == nil || *q.ChangePercent != 2.1008 {
		t.Fatalf("change percent = %v, want the %% stripped", q.ChangePercent)
	}
	if q.Volume == nil || *q.Volume != 44489128 {
		t.Fatalf("volume = %v", q.Volume)
	}
	if q.AsOf == nil {
		t.Fatal("latest trading day did not become an as-of stamp")
	}
	if q.Source.Provider != ProviderAlphaVantage {
		t.Fatalf("provider = %q", q.Source.Provider)
	}
}

// The series object is keyed by a resolution-dependent name. Finding it by shape
// keeps a vendor rename from blanking the chart.
func TestAlphaVantageSeriesIsFoundByShapeAndOrdered(t *testing.T) {
	const body = `{
      "Meta Data": {"1. Information": "Intraday (5min) open, high, low, close prices and volume", "2. Symbol": "IBM"},
      "Time Series (5min)": {
        "2026-06-05 19:55:00": {"1. open":"307.8900","2. high":"307.9400","3. low":"307.3500","4. close":"307.5500","5. volume":"100179"},
        "2026-06-05 19:45:00": {"1. open":"307.1000","2. high":"307.6000","3. low":"307.0000","4. close":"307.4900","5. volume":"80000"},
        "2026-06-05 19:50:00": {"1. open":"307.5000","2. high":"307.9500","3. low":"307.2000","4. close":"307.8800","5. volume":"90000"}
      }
    }`
	c := alphaServer(t, map[string]string{"TIME_SERIES_INTRADAY": body})

	s, err := c.Series(context.Background(), "IBM", Interval5Min, false)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(s.Candles) != 3 {
		t.Fatalf("candles = %d", len(s.Candles))
	}
	// The payload arrives as an unordered map; the result must be ascending.
	for i := 1; i < len(s.Candles); i++ {
		if !s.Candles[i].Time.After(s.Candles[i-1].Time) {
			t.Fatalf("not ascending at %d", i)
		}
	}
	if s.Candles[0].Close != 307.49 {
		t.Fatalf("first close = %v, want the 19:45 bar", s.Candles[0].Close)
	}
	if s.Candles[0].Volume == nil || *s.Candles[0].Volume != 80000 {
		t.Fatalf("volume = %v", s.Candles[0].Volume)
	}
}

func TestAlphaVantageNewsCarriesSentimentFMPCannot(t *testing.T) {
	const body = `{
      "items": "2",
      "feed": [
        {
          "title": "Apple announces new chip",
          "url": "https://example.test/a",
          "time_published": "20260606T124012",
          "summary": "Apple said today.",
          "banner_image": "https://example.test/a.jpg",
          "source": "Reuters",
          "overall_sentiment_score": 0.284,
          "overall_sentiment_label": "Somewhat-Bullish",
          "ticker_sentiment": [{"ticker": "AAPL"}, {"ticker": "TSM"}]
        }
      ]
    }`
	c := alphaServer(t, map[string]string{"NEWS_SENTIMENT": body})

	feed, err := c.NewsSentiment(context.Background(), []string{"AAPL"}, "", 10)
	if err != nil {
		t.Fatalf("NewsSentiment: %v", err)
	}
	if len(feed.Items) != 1 {
		t.Fatalf("items = %d", len(feed.Items))
	}
	item := feed.Items[0]
	if item.Sentiment == nil || item.Sentiment.Score != 0.284 || item.Sentiment.Label != "Somewhat-Bullish" {
		t.Fatalf("sentiment = %+v", item.Sentiment)
	}
	if item.PublishedAt == nil {
		t.Fatal("the compact vendor stamp did not parse")
	}
	if len(item.Symbols) != 2 {
		t.Fatalf("symbols = %v", item.Symbols)
	}
}

func TestAlphaVantageMoversParseStringFiguresAndDropUnparseableOnes(t *testing.T) {
	const body = `{
      "metadata": "Top gainers, losers, and most actively traded US tickers",
      "last_updated": "2026-06-06 16:15:59 US/Eastern",
      "top_gainers": [
        {"ticker":"ABCD","price":"1.85","change_amount":"0.06","change_percentage":"3.35196%","volume":"1000"},
        {"ticker":"EFGH","price":"None","change_amount":"None","change_percentage":"None","volume":"None"}
      ],
      "top_losers": [],
      "most_actively_traded": []
    }`
	c := alphaServer(t, map[string]string{"TOP_GAINERS_LOSERS": body})

	list, err := c.Movers(context.Background(), MoversGainers)
	if err != nil {
		t.Fatalf("Movers: %v", err)
	}
	if len(list.Movers) != 2 {
		t.Fatalf("movers = %d", len(list.Movers))
	}
	if list.Movers[0].ChangePercent == nil || *list.Movers[0].ChangePercent != 3.35196 {
		t.Fatalf("percent = %v", list.Movers[0].ChangePercent)
	}
	// "None" is the vendor saying it has no figure — absent, never zero.
	if list.Movers[1].Price != nil || list.Movers[1].Volume != nil {
		t.Fatalf("unparseable figures became values: %+v", list.Movers[1])
	}
}

func TestAlphaVantageMoversEmptyListIsNotFound(t *testing.T) {
	const body = `{"top_gainers": [], "top_losers": [], "most_actively_traded": []}`
	c := alphaServer(t, map[string]string{"TOP_GAINERS_LOSERS": body})

	_, err := c.Movers(context.Background(), MoversLosers)
	if KindOf(err) != FailureNotFound {
		t.Fatalf("kind = %q, want not_found", KindOf(err))
	}
}

func TestAlphaVantageMarketStatusReadsTheBoard(t *testing.T) {
	const body = `{
      "endpoint": "Global Market Open & Close Status",
      "markets": [
        {"market_type":"Equity","region":"United States","primary_exchanges":"NASDAQ, NYSE","local_open":"09:30","local_close":"16:00","current_status":"open","notes":""},
        {"market_type":"Equity","region":"United Kingdom","primary_exchanges":"LSE","local_open":"08:00","local_close":"16:30","current_status":"closed","notes":""}
      ]
    }`
	c := alphaServer(t, map[string]string{"MARKET_STATUS": body})

	status, err := c.MarketStatus(context.Background())
	if err != nil {
		t.Fatalf("MarketStatus: %v", err)
	}
	if len(status.Sessions) != 2 {
		t.Fatalf("sessions = %d", len(status.Sessions))
	}
	if !status.Sessions[0].IsOpen || status.Sessions[1].IsOpen {
		t.Fatalf("open flags wrong: %+v", status.Sessions)
	}
	if status.Sessions[0].Region != "United States" {
		t.Fatalf("region = %q", status.Sessions[0].Region)
	}
}

func TestAlphaVantageSearchReadsBestMatches(t *testing.T) {
	const body = `{
      "bestMatches": [
        {"1. symbol":"TSCO.LON","2. name":"Tesco PLC","3. type":"Equity","4. region":"United Kingdom","8. currency":"GBP"}
      ]
    }`
	c := alphaServer(t, map[string]string{"SYMBOL_SEARCH": body})

	found, err := c.Search(context.Background(), "tesco")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found.Matches) != 1 {
		t.Fatalf("matches = %d", len(found.Matches))
	}
	m := found.Matches[0]
	if m.Symbol != "TSCO.LON" || m.Name != "Tesco PLC" || m.Currency != "GBP" {
		t.Fatalf("match wrong: %+v", m)
	}
}

func TestAlphaVantageEconomicSeriesParsesStringValues(t *testing.T) {
	const body = `{
      "name": "Real Gross Domestic Product",
      "interval": "quarterly",
      "unit": "billions of dollars",
      "data": [
        {"date":"2026-01-01","value":"23400.1"},
        {"date":"2025-10-01","value":"."}
      ]
    }`
	c := alphaServer(t, map[string]string{"REAL_GDP": body})

	series, err := c.EconomicSeries(context.Background(), "REAL_GDP", nil)
	if err != nil {
		t.Fatalf("EconomicSeries: %v", err)
	}
	if series.Unit != "billions of dollars" || series.Interval != "quarterly" {
		t.Fatalf("metadata wrong: %+v", series)
	}
	// The vendor writes a missing observation as "."; it is dropped, not zeroed.
	if len(series.Points) != 1 || series.Points[0].Value != 23400.1 {
		t.Fatalf("points = %+v", series.Points)
	}
}

func TestAlphaVantageWithoutAKeyDegradesHonestly(t *testing.T) {
	c := NewAlphaVantage("", "", nil)
	_, err := c.GlobalQuote(context.Background(), "IBM")
	f := FailureOf(err)
	if f == nil || f.Kind != FailureNotConfigured {
		t.Fatalf("kind = %q, want not_configured", KindOf(err))
	}
	if !strings.Contains(f.Detail, AlphaVantageKeyEnv) {
		t.Fatalf("detail %q does not name the missing variable", f.Detail)
	}
}

func TestAlphaVantageFailuresNeverCarryTheAPIKey(t *testing.T) {
	const secret = "AV-SECRET-KEY-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The vendor echoing the call back is the leak path being closed.
		_, _ = w.Write([]byte(`{"Error Message":"Invalid API call for ` + r.URL.RawQuery + `"}`))
	}))
	defer srv.Close()
	c := NewAlphaVantage(secret, srv.URL, srv.Client())

	_, err := c.GlobalQuote(context.Background(), "IBM")
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the API key leaked into an error: %v", err)
	}
}
