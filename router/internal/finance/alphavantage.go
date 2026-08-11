// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AlphaVantageBaseURL is the documented query endpoint. Overridable per client so
// tests can point the real client at a real httptest server.
const AlphaVantageBaseURL = "https://www.alphavantage.co"

// AlphaVantageKeyEnv is the ONLY place the Alpha Vantage key is read from.
const AlphaVantageKeyEnv = "ALPHAVANTAGE_API_KEY"

// AlphaVantage is the complementary provider. It carries what FMP does not:
// sentiment-scored news, the market-status board, the long macro series, and a
// declared fallback for quotes and series.
//
// Its defining hazard is that a refused request comes back as HTTP 200 with a
// prose body — "Note", "Information", or "Error Message" — which a naive parse
// turns into an empty series. Every response here is screened for those keys
// BEFORE it is parsed, so a throttle can never be rendered as a flat chart.
type AlphaVantage struct {
	BaseURL string
	APIKey  string
	tr      *transport
	// Loc interprets the vendor's naive intraday stamps, which are documented as
	// US Eastern market time.
	Loc *time.Location
}

// NewAlphaVantage builds the client. An empty key is legal: calls answer with a
// typed not-configured failure naming ALPHAVANTAGE_API_KEY.
func NewAlphaVantage(apiKey, baseURL string, client *http.Client) *AlphaVantage {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = AlphaVantageBaseURL
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	return &AlphaVantage{
		BaseURL: baseURL,
		APIKey:  strings.TrimSpace(apiKey),
		tr:      newTransport(client),
		Loc:     loc,
	}
}

// Configured reports whether a key is present.
func (c *AlphaVantage) Configured() bool { return c != nil && c.APIKey != "" }

func (c *AlphaVantage) source() Source {
	return Source{Provider: ProviderAlphaVantage, FetchedAt: c.tr.now().UTC()}
}

// get performs one documented function call and returns the decoded top-level
// object, screened for the throttle/error bodies first.
func (c *AlphaVantage) get(ctx context.Context, function string, params map[string]string) (map[string]json.RawMessage, error) {
	if !c.Configured() {
		return nil, notConfigured(ProviderAlphaVantage, function, AlphaVantageKeyEnv)
	}
	if params == nil {
		params = map[string]string{}
	}
	params["function"] = function
	params["apikey"] = c.APIKey
	body, err := c.tr.get(ctx, ProviderAlphaVantage, function, buildURL(c.BaseURL, "query", params))
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: "The market data provider sent an unreadable response.",
			Detail:  redactKeys(err.Error()) + " | " + snippet(body),
		}
	}
	if fail := alphaRefusal(obj, function); fail != nil {
		return nil, fail
	}
	return obj, nil
}

// alphaRefusal is THE guard that keeps a rate-limit body from becoming data.
// Alpha Vantage answers a refused call with HTTP 200 and one of three prose
// keys; each is classified rather than parsed.
func alphaRefusal(obj map[string]json.RawMessage, function string) *Failure {
	for key, kind := range map[string]FailureKind{
		"Note":          FailureThrottled,
		"Information":   FailureThrottled,
		"Error Message": FailureBadRequest,
	} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var msg string
		if err := json.Unmarshal(raw, &msg); err != nil || strings.TrimSpace(msg) == "" {
			continue
		}
		low := strings.ToLower(msg)
		// "Information" carries both the throttle notice and the premium-
		// endpoint notice; the latter is a configuration fact, not a retryable
		// throttle, so it is classified apart.
		if key == "Information" && (strings.Contains(low, "premium") || strings.Contains(low, "subscribe")) {
			kind = FailureNotConfigured
		}
		if key == "Error Message" && strings.Contains(low, "invalid api call") {
			kind = FailureBadRequest
		}
		message := "The market data provider is rate limiting requests right now."
		switch kind {
		case FailureNotConfigured:
			message = "That market data is not included in this deployment's plan."
		case FailureBadRequest:
			message = "The market data provider could not answer that request."
		}
		return &Failure{
			Kind: kind, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: message, Detail: redactKeys(msg),
		}
	}
	return nil
}

func avFloat(s string) *float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" || s == "None" || s == "-" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

func avInt(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "None" {
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

/* ----------------------------------------------------------- global quote -- */

// GlobalQuote reads one symbol's quote — the declared fallback when FMP cannot
// answer. The documented payload nests under "Global Quote" with ordinal field
// names ("01. symbol", "05. price", …).
func (c *AlphaVantage) GlobalQuote(ctx context.Context, symbol string) (*Quote, error) {
	obj, err := c.get(ctx, "GLOBAL_QUOTE", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	raw, ok := obj["Global Quote"]
	if !ok {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "GLOBAL_QUOTE",
			Message: "No quote was returned for that symbol.",
		}
	}
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "GLOBAL_QUOTE",
			Message: "No quote was returned for that symbol.",
		}
	}
	// The ordinal prefixes are stripped so a vendor renumbering cannot silently
	// blank a field.
	get := func(name string) string {
		for k, v := range fields {
			if strings.EqualFold(strings.TrimSpace(stripOrdinal(k)), name) {
				return v
			}
		}
		return ""
	}
	out := &Quote{
		Symbol:        strings.TrimSpace(firstNonEmpty(get("symbol"), symbol)),
		Class:         classForExchange("", symbol),
		Price:         avFloat(get("price")),
		Change:        avFloat(get("change")),
		ChangePercent: avFloat(get("change percent")),
		Open:          avFloat(get("open")),
		DayHigh:       avFloat(get("high")),
		DayLow:        avFloat(get("low")),
		PreviousClose: avFloat(get("previous close")),
		Volume:        avInt(get("volume")),
		Source:        c.source(),
	}
	if day := strings.TrimSpace(get("latest trading day")); day != "" {
		if t, err := time.ParseInLocation("2006-01-02", day, c.location()); err == nil {
			u := t.UTC()
			out.AsOf = &u
		}
	}
	if out.Price == nil {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "GLOBAL_QUOTE",
			Message: "No quote was returned for that symbol.",
		}
	}
	return out, nil
}

// stripOrdinal removes the documented "NN. " prefix from a Global Quote field.
func stripOrdinal(k string) string {
	if i := strings.Index(k, ". "); i > 0 && i <= 3 {
		return k[i+2:]
	}
	return k
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (c *AlphaVantage) location() *time.Location {
	if c.Loc == nil {
		return time.UTC
	}
	return c.Loc
}

/* ------------------------------------------------------------ time series -- */

// avFunction maps a wire interval onto the documented time-series function and
// its interval parameter.
func avFunction(interval Interval) (function, param string) {
	switch interval {
	case Interval1Min, Interval5Min, Interval15Min, Interval30Min:
		return "TIME_SERIES_INTRADAY", string(interval)
	case Interval1Hour:
		return "TIME_SERIES_INTRADAY", "60min"
	case IntervalDay:
		return "TIME_SERIES_DAILY", ""
	case IntervalWeek:
		return "TIME_SERIES_WEEKLY", ""
	case IntervalMonth:
		return "TIME_SERIES_MONTHLY", ""
	}
	return "", ""
}

// Series reads an OHLCV run — the declared fallback for charts. The vendor keys
// the payload by a resolution-dependent name ("Time Series (5min)", "Time Series
// (Daily)", "Weekly Time Series", …), so the series object is found by shape
// rather than by a hardcoded key.
func (c *AlphaVantage) Series(ctx context.Context, symbol string, interval Interval, full bool) (*Series, error) {
	function, param := avFunction(interval)
	if function == "" {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderAlphaVantage, Endpoint: "TIME_SERIES",
			Message: "That chart resolution is not available.",
		}
	}
	params := map[string]string{"symbol": symbol}
	if param != "" {
		params["interval"] = param
	}
	if full {
		params["outputsize"] = "full"
	}
	obj, err := c.get(ctx, function, params)
	if err != nil {
		return nil, err
	}

	var seriesRaw json.RawMessage
	for key, raw := range obj {
		if strings.Contains(strings.ToLower(key), "time series") {
			seriesRaw = raw
			break
		}
	}
	if seriesRaw == nil {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: "No price history was returned for that symbol.",
		}
	}
	var bars map[string]map[string]string
	if err := json.Unmarshal(seriesRaw, &bars); err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: "The market data provider sent an unreadable price history.",
			Detail:  redactKeys(err.Error()),
		}
	}

	out := &Series{Symbol: symbol, Interval: interval, Source: c.source()}
	for stamp, fields := range bars {
		t, ok := c.parseStamp(stamp)
		if !ok {
			continue
		}
		open, high, low, cl := avFloat(fields["1. open"]), avFloat(fields["2. high"]), avFloat(fields["3. low"]), avFloat(fields["4. close"])
		if open == nil || high == nil || low == nil || cl == nil {
			continue
		}
		candle := Candle{Time: t, Open: *open, High: *high, Low: *low, Close: *cl}
		// The adjusted variants number volume differently; both documented
		// positions are accepted.
		if v := avInt(firstNonEmpty(fields["5. volume"], fields["6. volume"])); v != nil {
			candle.Volume = v
		}
		out.Candles = append(out.Candles, candle)
	}
	if len(out.Candles) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: "No price history was returned for that symbol.",
		}
	}
	sortCandlesAscending(out.Candles)
	return out, nil
}

func (c *AlphaVantage) parseStamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, c.location()); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// sortCandlesAscending orders a map-sourced series, which arrives unordered.
func sortCandlesAscending(c []Candle) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].Time.Before(c[j-1].Time); j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}

/* -------------------------------------------------------- news sentiment -- */

type avNewsFeed struct {
	Feed []struct {
		Title                 string   `json:"title"`
		URL                   string   `json:"url"`
		TimePublished         string   `json:"time_published"`
		Summary               string   `json:"summary"`
		BannerImage           string   `json:"banner_image"`
		Source                string   `json:"source"`
		OverallSentimentScore *float64 `json:"overall_sentiment_score"`
		OverallSentimentLabel string   `json:"overall_sentiment_label"`
		TickerSentiment       []struct {
			Ticker string `json:"ticker"`
		} `json:"ticker_sentiment"`
	} `json:"feed"`
}

// NewsSentiment reads the sentiment-scored stream — the one thing FMP's news
// does not carry, and the reason Alpha Vantage is in the suite for news at all.
func (c *AlphaVantage) NewsSentiment(ctx context.Context, tickers []string, topics string, limit int) (*NewsFeed, error) {
	params := map[string]string{}
	if joined := strings.Join(tickers, ","); strings.TrimSpace(joined) != "" {
		params["tickers"] = joined
	}
	if strings.TrimSpace(topics) != "" {
		params["topics"] = topics
	}
	if limit > 0 {
		params["limit"] = strconv.Itoa(limit)
	}
	obj, err := c.get(ctx, "NEWS_SENTIMENT", params)
	if err != nil {
		return nil, err
	}
	// Re-marshal the screened object so the feed decodes through one typed pass.
	blob, err := json.Marshal(obj)
	if err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: "NEWS_SENTIMENT",
			Message: "The market data provider sent an unreadable news feed.",
		}
	}
	var decoded avNewsFeed
	if err := json.Unmarshal(blob, &decoded); err != nil || len(decoded.Feed) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "NEWS_SENTIMENT",
			Message: "No news was returned for that request.",
		}
	}
	out := &NewsFeed{Source: c.source()}
	for index := range decoded.Feed {
		r := &decoded.Feed[index]
		item := NewsItem{
			Title: r.Title, URL: r.URL, Publisher: r.Source,
			Summary: r.Summary, ImageURL: r.BannerImage,
		}
		// The documented stamp is compact ISO ("20260606T124012").
		if t, err := time.ParseInLocation("20060102T150405", strings.TrimSpace(r.TimePublished), c.location()); err == nil {
			u := t.UTC()
			item.PublishedAt = &u
		}
		if r.OverallSentimentScore != nil {
			item.Sentiment = &Sentiment{Score: *r.OverallSentimentScore, Label: r.OverallSentimentLabel}
		}
		for _, ts := range r.TickerSentiment {
			if strings.TrimSpace(ts.Ticker) != "" {
				item.Symbols = append(item.Symbols, ts.Ticker)
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

/* ------------------------------------------------------- movers & status -- */

type avMoverRow struct {
	Ticker           string `json:"ticker"`
	Price            string `json:"price"`
	ChangeAmount     string `json:"change_amount"`
	ChangePercentage string `json:"change_percentage"`
	Volume           string `json:"volume"`
}

type avTopMovers struct {
	TopGainers []avMoverRow `json:"top_gainers"`
	TopLosers  []avMoverRow `json:"top_losers"`
	MostActive []avMoverRow `json:"most_actively_traded"`
}

// Movers reads the ranked market lists — the declared fallback for the movers
// board. Every figure arrives as a string and becomes absent rather than zero
// when it does not parse.
func (c *AlphaVantage) Movers(ctx context.Context, kind MoverKind) (*MoverList, error) {
	obj, err := c.get(ctx, "TOP_GAINERS_LOSERS", nil)
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(obj)
	if err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: "TOP_GAINERS_LOSERS",
			Message: "The market data provider sent an unreadable market list.",
		}
	}
	var decoded avTopMovers
	if err := json.Unmarshal(blob, &decoded); err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: "TOP_GAINERS_LOSERS",
			Message: "The market data provider sent an unreadable market list.",
		}
	}
	var rows []avMoverRow
	switch kind {
	case MoversGainers:
		rows = decoded.TopGainers
	case MoversLosers:
		rows = decoded.TopLosers
	case MoversActive:
		rows = decoded.MostActive
	}
	if len(rows) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "TOP_GAINERS_LOSERS",
			Message: "No market list was returned.",
		}
	}
	out := &MoverList{Kind: kind, Source: c.source()}
	for _, r := range rows {
		out.Movers = append(out.Movers, Mover{
			Symbol: r.Ticker, Price: avFloat(r.Price),
			Change: avFloat(r.ChangeAmount), ChangePercent: avFloat(r.ChangePercentage),
			Volume: avInt(r.Volume),
		})
	}
	return out, nil
}

type avMarketStatus struct {
	Markets []struct {
		MarketType       string `json:"market_type"`
		Region           string `json:"region"`
		PrimaryExchanges string `json:"primary_exchanges"`
		LocalOpen        string `json:"local_open"`
		LocalClose       string `json:"local_close"`
		CurrentStatus    string `json:"current_status"`
		Notes            string `json:"notes"`
	} `json:"markets"`
}

// MarketStatus reads the global open/closed board.
func (c *AlphaVantage) MarketStatus(ctx context.Context) (*MarketStatus, error) {
	obj, err := c.get(ctx, "MARKET_STATUS", nil)
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(obj)
	if err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: "MARKET_STATUS",
			Message: "The market data provider sent an unreadable market status.",
		}
	}
	var decoded avMarketStatus
	if err := json.Unmarshal(blob, &decoded); err != nil || len(decoded.Markets) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "MARKET_STATUS",
			Message: "No market status was returned.",
		}
	}
	out := &MarketStatus{Source: c.source()}
	for _, m := range decoded.Markets {
		out.Sessions = append(out.Sessions, MarketSession{
			Exchange: m.PrimaryExchanges, Name: m.MarketType, Region: m.Region,
			OpenTime: m.LocalOpen, CloseTime: m.LocalClose,
			IsOpen: strings.EqualFold(strings.TrimSpace(m.CurrentStatus), "open"),
			Note:   m.Notes,
		})
	}
	return out, nil
}

/* ----------------------------------------------------------------- search -- */

type avSearch struct {
	BestMatches []map[string]string `json:"bestMatches"`
}

// Search resolves a query to symbols — the declared fallback for search.
func (c *AlphaVantage) Search(ctx context.Context, query string) (*SearchResults, error) {
	if strings.TrimSpace(query) == "" {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderAlphaVantage, Endpoint: "SYMBOL_SEARCH",
			Message: "No search term was given.",
		}
	}
	obj, err := c.get(ctx, "SYMBOL_SEARCH", map[string]string{"keywords": query})
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(obj)
	if err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: "SYMBOL_SEARCH",
			Message: "The market data provider sent an unreadable search result.",
		}
	}
	var decoded avSearch
	if err := json.Unmarshal(blob, &decoded); err != nil || len(decoded.BestMatches) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "SYMBOL_SEARCH",
			Message: "Nothing matched that search.",
		}
	}
	out := &SearchResults{Query: query, Source: c.source()}
	for _, m := range decoded.BestMatches {
		get := func(name string) string {
			for k, v := range m {
				if strings.EqualFold(strings.TrimSpace(stripOrdinal(k)), name) {
					return strings.TrimSpace(v)
				}
			}
			return ""
		}
		symbol := get("symbol")
		if symbol == "" {
			continue
		}
		out.Matches = append(out.Matches, SearchMatch{
			Symbol: symbol, Name: get("name"), Exchange: get("region"),
			Currency: get("currency"), Class: classForExchange("", symbol),
		})
	}
	if len(out.Matches) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: "SYMBOL_SEARCH",
			Message: "Nothing matched that search.",
		}
	}
	return out, nil
}

/* ------------------------------------------------------------------ macro -- */

type avEconomic struct {
	Name     string `json:"name"`
	Interval string `json:"interval"`
	Unit     string `json:"unit"`
	Data     []struct {
		Date  string `json:"date"`
		Value string `json:"value"`
	} `json:"data"`
}

// EconomicSeries reads one documented macro function (REAL_GDP, CPI,
// UNEMPLOYMENT, TREASURY_YIELD, FEDERAL_FUNDS_RATE, …).
func (c *AlphaVantage) EconomicSeries(ctx context.Context, function string, params map[string]string) (*EconomicSeries, error) {
	if strings.TrimSpace(function) == "" {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderAlphaVantage, Endpoint: "economic",
			Message: "No economic series was named.",
		}
	}
	obj, err := c.get(ctx, function, params)
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(obj)
	if err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: "The market data provider sent an unreadable series.",
		}
	}
	var decoded avEconomic
	if err := json.Unmarshal(blob, &decoded); err != nil || len(decoded.Data) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: "No observations were returned for that series.",
		}
	}
	out := &EconomicSeries{
		Name: firstNonEmpty(decoded.Name, function), Unit: decoded.Unit,
		Interval: decoded.Interval, Source: c.source(),
	}
	for _, p := range decoded.Data {
		v := avFloat(p.Value)
		if v == nil {
			continue
		}
		out.Points = append(out.Points, EconomicPoint{Date: p.Date, Value: *v})
	}
	if len(out.Points) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderAlphaVantage, Endpoint: function,
			Message: "No observations were returned for that series.",
		}
	}
	return out, nil
}
