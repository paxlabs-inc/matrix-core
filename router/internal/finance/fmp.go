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

// FMPBaseURL is the documented stable base. Overridable per client so tests can
// point the real client at a real httptest server serving the documented bodies.
const FMPBaseURL = "https://financialmodelingprep.com/stable"

// FMPKeyEnv is the ONLY place the Financial Modeling Prep key is read from.
const FMPKeyEnv = "FMP_API_KEY"

// FMP is the Financial Modeling Prep client. It is the suite's primary provider:
// quotes, series, profiles, movers, sectors, news, consensus, TTM metrics,
// earnings, dividends, market hours and the macro series all come from here.
//
// Every method parses ONLY fields the published documentation defines, and every
// optional number stays a pointer so a field the vendor omitted is omitted here
// too.
type FMP struct {
	BaseURL string
	APIKey  string
	tr      *transport
	// Loc interprets the vendor's naive timestamps ("2026-06-05 15:59:00"),
	// which carry no zone. The documented examples are US market sessions, so
	// the exchange zone is the default; it is a field rather than a constant so
	// a deployment can correct it without a code change.
	Loc *time.Location
}

// NewFMP builds the client. An empty key is legal: the lane still boots and
// every call answers with a typed not-configured failure naming FMP_API_KEY.
func NewFMP(apiKey, baseURL string, client *http.Client) *FMP {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = FMPBaseURL
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	return &FMP{
		BaseURL: baseURL,
		APIKey:  strings.TrimSpace(apiKey),
		tr:      newTransport(client),
		Loc:     loc,
	}
}

// Configured reports whether a key is present.
func (c *FMP) Configured() bool { return c != nil && c.APIKey != "" }

func (c *FMP) now() time.Time { return c.tr.now().UTC() }

func (c *FMP) source() Source {
	return Source{Provider: ProviderFMP, FetchedAt: c.now()}
}

// fmpList performs one documented FMP GET and decodes its JSON array. FMP
// answers every one of these endpoints with a top-level array; an empty array
// means "the vendor has nothing", which is a not-found rather than empty data
// presented as real.
func fmpList[T any](ctx context.Context, c *FMP, endpoint, path string, params map[string]string) ([]T, error) {
	if !c.Configured() {
		return nil, notConfigured(ProviderFMP, endpoint, FMPKeyEnv)
	}
	if params == nil {
		params = map[string]string{}
	}
	params["apikey"] = c.APIKey
	body, err := c.tr.get(ctx, ProviderFMP, endpoint, buildURL(c.BaseURL, path, params))
	if err != nil {
		return nil, err
	}
	// FMP reports a plan/limit refusal as a JSON OBJECT carrying an "Error
	// Message" where an array was documented. Decoding that into []T yields a
	// bare unmarshal error, so it is detected first and classed honestly.
	if fail := fmpObjectRefusal(body, endpoint); fail != nil {
		return nil, fail
	}
	var out []T
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &Failure{
			Kind: FailureUpstream, Provider: ProviderFMP, Endpoint: endpoint,
			Message: "The market data provider sent an unreadable response.",
			Detail:  redactKeys(err.Error()) + " | " + snippet(body),
		}
	}
	if len(out) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: endpoint,
			Message: "No market data was returned for that request.",
		}
	}
	return out, nil
}

// fmpObjectRefusal detects the documented error/limit object FMP substitutes for
// an array when a request is refused.
func fmpObjectRefusal(body []byte, endpoint string) *Failure {
	trimmed := strings.TrimSpace(string(body))
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	for _, key := range []string{"Error Message", "error", "message"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var msg string
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		kind := FailureUpstream
		low := strings.ToLower(msg)
		switch {
		case strings.Contains(low, "limit") || strings.Contains(low, "quota") || strings.Contains(low, "bandwidth"):
			kind = FailureThrottled
		case strings.Contains(low, "api key") || strings.Contains(low, "apikey") || strings.Contains(low, "unauthorized"):
			kind = FailureNotConfigured
		}
		return &Failure{
			Kind: kind, Provider: ProviderFMP, Endpoint: endpoint,
			Message: refusalMessage(kind),
			Detail:  redactKeys(msg),
		}
	}
	return nil
}

func refusalMessage(kind FailureKind) string {
	switch kind {
	case FailureThrottled:
		return "The market data provider is rate limiting requests right now."
	case FailureNotConfigured:
		return "The market data provider rejected this deployment's credentials."
	default:
		return "The market data provider returned an error."
	}
}

/* ---------------------------------------------------------------- quotes -- */

// fmpQuote mirrors the documented /quote row, which is identical for equities,
// indexes, crypto, forex and commodities.
type fmpQuote struct {
	Symbol           string   `json:"symbol"`
	Name             string   `json:"name"`
	Price            *float64 `json:"price"`
	ChangePercentage *float64 `json:"changePercentage"`
	Change           *float64 `json:"change"`
	Volume           *int64   `json:"volume"`
	DayLow           *float64 `json:"dayLow"`
	DayHigh          *float64 `json:"dayHigh"`
	YearHigh         *float64 `json:"yearHigh"`
	YearLow          *float64 `json:"yearLow"`
	MarketCap        *float64 `json:"marketCap"`
	PriceAvg50       *float64 `json:"priceAvg50"`
	PriceAvg200      *float64 `json:"priceAvg200"`
	Exchange         string   `json:"exchange"`
	Open             *float64 `json:"open"`
	PreviousClose    *float64 `json:"previousClose"`
	Timestamp        *int64   `json:"timestamp"`
}

func (q fmpQuote) normalize(src Source) Quote {
	out := Quote{
		Symbol:        q.Symbol,
		Name:          q.Name,
		Exchange:      q.Exchange,
		Class:         classForExchange(q.Exchange, q.Symbol),
		Price:         q.Price,
		Change:        q.Change,
		ChangePercent: q.ChangePercentage,
		Open:          q.Open,
		DayHigh:       q.DayHigh,
		DayLow:        q.DayLow,
		PreviousClose: q.PreviousClose,
		YearHigh:      q.YearHigh,
		YearLow:       q.YearLow,
		Volume:        q.Volume,
		PriceAvg50:    q.PriceAvg50,
		PriceAvg200:   q.PriceAvg200,
		Source:        src,
	}
	// FMP sends marketCap 0 or null for instruments that have none (indexes,
	// forex, commodities). Zero is not a market cap, so it is dropped rather
	// than rendered as "$0".
	if q.MarketCap != nil && *q.MarketCap != 0 {
		out.MarketCap = q.MarketCap
	}
	if q.Timestamp != nil && *q.Timestamp > 0 {
		t := time.Unix(*q.Timestamp, 0).UTC()
		out.AsOf = &t
	}
	return out
}

// classForExchange maps the vendor's exchange label onto the asset class. FMP
// labels its non-equity venues explicitly (INDEX, CRYPTO, FOREX, COMMODITY),
// which is enough to class every row it returns.
func classForExchange(exchange, symbol string) AssetClass {
	switch strings.ToUpper(strings.TrimSpace(exchange)) {
	case "INDEX":
		return ClassIndex
	case "CRYPTO":
		return ClassCrypto
	case "FOREX":
		return ClassForex
	case "COMMODITY":
		return ClassCommodity
	}
	if strings.HasPrefix(symbol, "^") {
		return ClassIndex
	}
	return ClassEquity
}

// Quote reads one instrument. Symbol covers every asset class: AAPL, ^GSPC,
// BTCUSD, EURUSD, GCUSD.
func (c *FMP) Quote(ctx context.Context, symbol string) (*Quote, error) {
	rows, err := fmpList[fmpQuote](ctx, c, "quote", "quote", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	q := rows[0].normalize(c.source())
	return &q, nil
}

// QuoteList reads a deliberately bounded set of symbols through FMP's
// documented batch-quote endpoint. It keeps a market board to one upstream
// request without downloading the provider's entire global asset universe.
func (c *FMP) QuoteList(ctx context.Context, symbols []string, class AssetClass) ([]Quote, error) {
	out, err := c.BatchQuote(ctx, symbols)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if class != "" {
			out[i].Class = class
		}
	}
	return out, nil
}

// BatchQuote reads many instruments in one upstream call — the shape the index
// strip, the watchlist and the per-market lists all want.
func (c *FMP) BatchQuote(ctx context.Context, symbols []string) ([]Quote, error) {
	joined := strings.Join(symbols, ",")
	if strings.TrimSpace(joined) == "" {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderFMP, Endpoint: "batch-quote",
			Message: "No symbols were requested.",
		}
	}
	rows, err := fmpList[fmpQuote](ctx, c, "batch-quote", "batch-quote", map[string]string{"symbols": joined})
	if err != nil {
		return nil, err
	}
	src := c.source()
	out := make([]Quote, 0, len(rows))
	for index := range rows {
		out = append(out, rows[index].normalize(src))
	}
	return out, nil
}

type fmpAftermarketQuote struct {
	Symbol    string   `json:"symbol"`
	BidSize   *int64   `json:"bidSize"`
	BidPrice  *float64 `json:"bidPrice"`
	AskSize   *int64   `json:"askSize"`
	AskPrice  *float64 `json:"askPrice"`
	Volume    *int64   `json:"volume"`
	Timestamp *int64   `json:"timestamp"`
}

type fmpAftermarketTrade struct {
	Symbol    string   `json:"symbol"`
	Price     *float64 `json:"price"`
	TradeSize *int64   `json:"tradeSize"`
	Timestamp *int64   `json:"timestamp"`
}

// ExtendedQuote reads the pre/post-market book. Session is left to the caller
// (the service labels it from market status) because the vendor does not say
// which side of the session a quote belongs to.
func (c *FMP) ExtendedQuote(ctx context.Context, symbol string) (*ExtendedQuote, error) {
	rows, err := fmpList[fmpAftermarketQuote](ctx, c, "aftermarket-quote", "aftermarket-quote", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	r := rows[0]
	out := &ExtendedQuote{BidPrice: r.BidPrice, AskPrice: r.AskPrice, Volume: r.Volume}
	// FMP's aftermarket timestamps are milliseconds, unlike /quote's seconds.
	if r.Timestamp != nil && *r.Timestamp > 0 {
		t := time.UnixMilli(*r.Timestamp).UTC()
		out.AsOf = &t
	}
	return out, nil
}

// ExtendedTrade reads the last aftermarket print, which carries a price where
// the aftermarket QUOTE carries only a book.
func (c *FMP) ExtendedTrade(ctx context.Context, symbol string) (*ExtendedQuote, error) {
	rows, err := fmpList[fmpAftermarketTrade](ctx, c, "aftermarket-trade", "aftermarket-trade", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	r := rows[0]
	out := &ExtendedQuote{Price: r.Price}
	if r.Timestamp != nil && *r.Timestamp > 0 {
		t := time.UnixMilli(*r.Timestamp).UTC()
		out.AsOf = &t
	}
	return out, nil
}

// PriceChange reads the multi-window performance strip. The vendor keys the
// windows itself; they are carried through unchanged rather than renamed.
func (c *FMP) PriceChange(ctx context.Context, symbol string) (*PriceChange, error) {
	if !c.Configured() {
		return nil, notConfigured(ProviderFMP, "stock-price-change", FMPKeyEnv)
	}
	body, err := c.tr.get(ctx, ProviderFMP, "stock-price-change",
		buildURL(c.BaseURL, "stock-price-change", map[string]string{"symbol": symbol, "apikey": c.APIKey}))
	if err != nil {
		return nil, err
	}
	if fail := fmpObjectRefusal(body, "stock-price-change"); fail != nil {
		return nil, fail
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: "stock-price-change",
			Message: "No performance history was returned for that symbol.",
		}
	}
	out := &PriceChange{Symbol: symbol, Windows: map[string]float64{}, Source: c.source()}
	for key, raw := range rows[0] {
		if key == "symbol" {
			continue
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err == nil {
			out.Windows[key] = v
		}
	}
	return out, nil
}

/* ---------------------------------------------------------------- series -- */

type fmpEODFull struct {
	Symbol string   `json:"symbol"`
	Date   string   `json:"date"`
	Open   *float64 `json:"open"`
	High   *float64 `json:"high"`
	Low    *float64 `json:"low"`
	Close  *float64 `json:"close"`
	Volume *int64   `json:"volume"`
}

type fmpIntradayBar struct {
	Date   string   `json:"date"`
	Open   *float64 `json:"open"`
	Low    *float64 `json:"low"`
	High   *float64 `json:"high"`
	Close  *float64 `json:"close"`
	Volume *int64   `json:"volume"`
}

// intradayPath maps a wire interval onto the documented historical-chart path.
func intradayPath(interval Interval) string {
	switch interval {
	case Interval1Min:
		return "historical-chart/1min"
	case Interval5Min:
		return "historical-chart/5min"
	case Interval15Min:
		return "historical-chart/15min"
	case Interval30Min:
		return "historical-chart/30min"
	case Interval1Hour:
		return "historical-chart/1hour"
	case Interval4Hour:
		return "historical-chart/4hour"
	}
	return ""
}

// Series reads an OHLCV run at the requested resolution. Intraday resolutions
// come from historical-chart/*, daily from historical-price-eod/full. The result
// is always ordered oldest-first — both endpoints return newest-first, and the
// chart reads forward.
func (c *FMP) Series(ctx context.Context, symbol string, interval Interval, from, to string) (*Series, error) {
	params := map[string]string{"symbol": symbol, "from": from, "to": to}
	out := &Series{Symbol: symbol, Interval: interval, Source: c.source()}

	if interval.Intraday() {
		path := intradayPath(interval)
		if path == "" {
			return nil, &Failure{
				Kind: FailureBadRequest, Provider: ProviderFMP, Endpoint: "historical-chart",
				Message: "That chart resolution is not available.",
			}
		}
		rows, err := fmpList[fmpIntradayBar](ctx, c, path, path, params)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			t, ok := c.parseVendorTime(r.Date)
			if !ok || r.Open == nil || r.High == nil || r.Low == nil || r.Close == nil {
				continue
			}
			out.Candles = append(out.Candles, Candle{
				Time: t, Open: *r.Open, High: *r.High, Low: *r.Low, Close: *r.Close, Volume: r.Volume,
			})
		}
	} else {
		rows, err := fmpList[fmpEODFull](ctx, c, "historical-price-eod/full", "historical-price-eod/full", params)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			t, ok := c.parseVendorTime(r.Date)
			if !ok || r.Open == nil || r.High == nil || r.Low == nil || r.Close == nil {
				continue
			}
			out.Candles = append(out.Candles, Candle{
				Time: t, Open: *r.Open, High: *r.High, Low: *r.Low, Close: *r.Close, Volume: r.Volume,
			})
		}
	}

	if len(out.Candles) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: "series",
			Message: "No price history was returned for that symbol.",
		}
	}
	sortCandles(out.Candles)
	return out, nil
}

// parseVendorTime reads the two documented stamp shapes. Neither carries a zone,
// so a naive stamp is read in the client's configured exchange location and
// normalized to UTC; a date-only stamp anchors at midnight there.
func (c *FMP) parseVendorTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	loc := c.Loc
	if loc == nil {
		loc = time.UTC
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC(), true
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// sortCandles orders a series oldest-first with an insertion pass. Vendor
// responses arrive newest-first (already sorted), so this is a linear reverse in
// practice and never a general-purpose sort cost.
func sortCandles(c []Candle) {
	for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
		c[i], c[j] = c[j], c[i]
	}
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].Time.Before(c[j-1].Time); j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}

/* --------------------------------------------------------------- profile -- */

type fmpProfile struct {
	Symbol            string   `json:"symbol"`
	CompanyName       string   `json:"companyName"`
	Price             *float64 `json:"price"`
	MarketCap         *float64 `json:"marketCap"`
	Beta              *float64 `json:"beta"`
	Range             string   `json:"range"`
	Currency          string   `json:"currency"`
	Exchange          string   `json:"exchange"`
	ExchangeFullName  string   `json:"exchangeFullName"`
	Industry          string   `json:"industry"`
	Website           string   `json:"website"`
	Description       string   `json:"description"`
	CEO               string   `json:"ceo"`
	Sector            string   `json:"sector"`
	Country           string   `json:"country"`
	FullTimeEmployees string   `json:"fullTimeEmployees"`
	Image             string   `json:"image"`
	IPODate           string   `json:"ipoDate"`
	IsETF             bool     `json:"isEtf"`
	IsFund            bool     `json:"isFund"`
	IsActivelyTrading *bool    `json:"isActivelyTrading"`
}

// Profile reads the company identity rail.
func (c *FMP) Profile(ctx context.Context, symbol string) (*Profile, error) {
	rows, err := fmpList[fmpProfile](ctx, c, "profile", "profile", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	r := rows[0]
	out := &Profile{
		Symbol: r.Symbol, Name: r.CompanyName, Exchange: r.Exchange, ExchangeName: r.ExchangeFullName,
		Currency: r.Currency, Sector: r.Sector, Industry: r.Industry, Country: r.Country,
		CEO: r.CEO, Website: r.Website, Description: r.Description, ImageURL: r.Image,
		IPODate: r.IPODate, Beta: r.Beta, IsETF: r.IsETF, IsFund: r.IsFund,
		IsActive: r.IsActivelyTrading, Range: r.Range, Source: c.source(),
	}
	if r.MarketCap != nil && *r.MarketCap != 0 {
		out.MarketCap = r.MarketCap
	}
	// The vendor sends the headcount as a STRING; a non-numeric or empty value
	// means it does not have one, which stays absent rather than becoming 0.
	if n, err := strconv.ParseInt(strings.TrimSpace(r.FullTimeEmployees), 10, 64); err == nil && n > 0 {
		out.Employees = &n
	}
	return out, nil
}

/* ---------------------------------------------------------------- search -- */

type fmpSearchMatch struct {
	Symbol           string `json:"symbol"`
	Name             string `json:"name"`
	Currency         string `json:"currency"`
	ExchangeFullName string `json:"exchangeFullName"`
	Exchange         string `json:"exchange"`
}

// Search resolves a query to symbols.
func (c *FMP) Search(ctx context.Context, query string, limit int) (*SearchResults, error) {
	if strings.TrimSpace(query) == "" {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderFMP, Endpoint: "search-symbol",
			Message: "No search term was given.",
		}
	}
	params := map[string]string{"query": query}
	if limit > 0 {
		params["limit"] = strconv.Itoa(limit)
	}
	rows, err := fmpList[fmpSearchMatch](ctx, c, "search-symbol", "search-symbol", params)
	if err != nil {
		return nil, err
	}
	out := &SearchResults{Query: query, Source: c.source()}
	for index := range rows {
		r := &rows[index]
		out.Matches = append(out.Matches, SearchMatch{
			Symbol: r.Symbol, Name: r.Name, Exchange: r.Exchange,
			ExchangeName: r.ExchangeFullName, Currency: r.Currency,
			Class: classForExchange(r.Exchange, r.Symbol),
		})
	}
	return out, nil
}

/* ---------------------------------------------------------------- movers -- */

type fmpMover struct {
	Symbol            string   `json:"symbol"`
	Price             *float64 `json:"price"`
	Name              string   `json:"name"`
	Change            *float64 `json:"change"`
	ChangesPercentage *float64 `json:"changesPercentage"`
	Exchange          string   `json:"exchange"`
}

// Movers reads one of the three ranked market lists.
func (c *FMP) Movers(ctx context.Context, kind MoverKind) (*MoverList, error) {
	var path string
	switch kind {
	case MoversGainers:
		path = "biggest-gainers"
	case MoversLosers:
		path = "biggest-losers"
	case MoversActive:
		path = "most-actives"
	default:
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderFMP, Endpoint: "movers",
			Message: "That market list is not available.",
		}
	}
	rows, err := fmpList[fmpMover](ctx, c, path, path, nil)
	if err != nil {
		return nil, err
	}
	out := &MoverList{Kind: kind, Source: c.source()}
	for index := range rows {
		r := &rows[index]
		out.Movers = append(out.Movers, Mover{
			Symbol: r.Symbol, Name: r.Name, Exchange: r.Exchange,
			Price: r.Price, Change: r.Change, ChangePercent: r.ChangesPercentage,
		})
	}
	return out, nil
}

type fmpSectorPerformance struct {
	Date          string   `json:"date"`
	Sector        string   `json:"sector"`
	Exchange      string   `json:"exchange"`
	AverageChange *float64 `json:"averageChange"`
}

// Sectors reads the sector performance board for a session date.
func (c *FMP) Sectors(ctx context.Context, date, exchange string) (*SectorBoard, error) {
	params := map[string]string{"date": date, "exchange": exchange}
	rows, err := fmpList[fmpSectorPerformance](ctx, c, "sector-performance-snapshot", "sector-performance-snapshot", params)
	if err != nil {
		return nil, err
	}
	out := &SectorBoard{Source: c.source()}
	for _, r := range rows {
		if r.AverageChange == nil {
			continue
		}
		out.Sectors = append(out.Sectors, SectorPerformance{
			Sector: r.Sector, Exchange: r.Exchange, ChangePercent: *r.AverageChange, Date: r.Date,
		})
	}
	if len(out.Sectors) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: "sector-performance-snapshot",
			Message: "No sector performance was returned for that session.",
		}
	}
	return out, nil
}

/* ------------------------------------------------------------------ news -- */

type fmpNews struct {
	Symbol        string `json:"symbol"`
	PublishedDate string `json:"publishedDate"`
	Publisher     string `json:"publisher"`
	Title         string `json:"title"`
	Image         string `json:"image"`
	Site          string `json:"site"`
	Text          string `json:"text"`
	URL           string `json:"url"`
}

func (c *FMP) news(ctx context.Context, endpoint, path string, params map[string]string) (*NewsFeed, error) {
	rows, err := fmpList[fmpNews](ctx, c, endpoint, path, params)
	if err != nil {
		return nil, err
	}
	out := &NewsFeed{Source: c.source()}
	for index := range rows {
		r := &rows[index]
		item := NewsItem{
			Title: r.Title, URL: r.URL, Publisher: r.Publisher,
			Site: r.Site, Summary: r.Text, ImageURL: r.Image,
		}
		if r.Symbol != "" {
			item.Symbols = []string{r.Symbol}
		}
		if t, ok := c.parseVendorTime(r.PublishedDate); ok {
			item.PublishedAt = &t
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// MarketNews reads the general market stream.
func (c *FMP) MarketNews(ctx context.Context, limit int) (*NewsFeed, error) {
	return c.news(ctx, "news/general-latest", "news/general-latest", map[string]string{"limit": limitParam(limit, 20)})
}

// StockNews reads the latest stock-market stream.
func (c *FMP) StockNews(ctx context.Context, limit int) (*NewsFeed, error) {
	return c.news(ctx, "news/stock-latest", "news/stock-latest", map[string]string{"limit": limitParam(limit, 20)})
}

// PressReleases reads the latest company announcements.
func (c *FMP) PressReleases(ctx context.Context, limit int) (*NewsFeed, error) {
	return c.news(ctx, "news/press-releases-latest", "news/press-releases-latest", map[string]string{"limit": limitParam(limit, 20)})
}

// SymbolNews reads the stream for specific symbols.
func (c *FMP) SymbolNews(ctx context.Context, symbols []string, limit int) (*NewsFeed, error) {
	joined := strings.Join(symbols, ",")
	if strings.TrimSpace(joined) == "" {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderFMP, Endpoint: "news/stock",
			Message: "No symbols were requested.",
		}
	}
	return c.news(ctx, "news/stock", "news/stock", map[string]string{"symbols": joined, "limit": limitParam(limit, 20)})
}

func limitParam(limit, def int) string {
	if limit <= 0 {
		limit = def
	}
	if limit > 250 {
		limit = 250
	}
	return strconv.Itoa(limit)
}

/* ---------------------------------------------------------- fundamentals -- */

type fmpKeyMetricsTTM struct {
	Symbol               string   `json:"symbol"`
	MarketCap            *float64 `json:"marketCap"`
	EnterpriseValueTTM   *float64 `json:"enterpriseValueTTM"`
	CurrentRatioTTM      *float64 `json:"currentRatioTTM"`
	ReturnOnEquityTTM    *float64 `json:"returnOnEquityTTM"`
	FreeCashFlowYieldTTM *float64 `json:"freeCashFlowYieldTTM"`
	NetDebtToEBITDATTM   *float64 `json:"netDebtToEBITDATTM"`
}

type fmpRatiosTTM struct {
	Symbol                               string   `json:"symbol"`
	NetProfitMarginTTM                   *float64 `json:"netProfitMarginTTM"`
	PriceToEarningsRatioTTM              *float64 `json:"priceToEarningsRatioTTM"`
	ForwardPriceToEarningsGrowthRatioTTM *float64 `json:"forwardPriceToEarningsGrowthRatioTTM"`
	PriceToBookRatioTTM                  *float64 `json:"priceToBookRatioTTM"`
	PriceToSalesRatioTTM                 *float64 `json:"priceToSalesRatioTTM"`
	DividendYieldTTM                     *float64 `json:"dividendYieldTTM"`
	DebtToEquityRatioTTM                 *float64 `json:"debtToEquityRatioTTM"`
	CurrentRatioTTM                      *float64 `json:"currentRatioTTM"`
	NetIncomePerShareTTM                 *float64 `json:"netIncomePerShareTTM"`
}

type fmpGradesConsensus struct {
	Symbol     string `json:"symbol"`
	StrongBuy  *int   `json:"strongBuy"`
	Buy        *int   `json:"buy"`
	Hold       *int   `json:"hold"`
	Sell       *int   `json:"sell"`
	StrongSell *int   `json:"strongSell"`
	Consensus  string `json:"consensus"`
}

type fmpPriceTargetConsensus struct {
	Symbol          string   `json:"symbol"`
	TargetHigh      *float64 `json:"targetHigh"`
	TargetLow       *float64 `json:"targetLow"`
	TargetConsensus *float64 `json:"targetConsensus"`
	TargetMedian    *float64 `json:"targetMedian"`
}

// KeyMetrics reads the TTM metric set behind the stats bar and folds it into
// `into`, which may be nil. Folding rather than returning a fresh summary lets
// the service compose the metric half and the ratio half independently — a
// symbol the vendor has one of but not the other still gets a real stats bar.
func (c *FMP) KeyMetrics(ctx context.Context, symbol string, into *FundamentalSummary) (*FundamentalSummary, error) {
	metrics, err := fmpList[fmpKeyMetricsTTM](ctx, c, "key-metrics-ttm", "key-metrics-ttm", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	if into == nil {
		into = &FundamentalSummary{Symbol: symbol, Source: c.source()}
	}
	m := metrics[0]
	if m.MarketCap != nil && *m.MarketCap != 0 {
		into.MarketCap = m.MarketCap
	}
	into.EnterpriseValue = m.EnterpriseValueTTM
	into.ReturnOnEquity = m.ReturnOnEquityTTM
	into.FreeCashFlowYield = m.FreeCashFlowYieldTTM
	into.CurrentRatio = m.CurrentRatioTTM
	return into, nil
}

// Ratios reads the TTM ratio set and folds it into the same summary.
func (c *FMP) Ratios(ctx context.Context, symbol string, into *FundamentalSummary) (*FundamentalSummary, error) {
	rows, err := fmpList[fmpRatiosTTM](ctx, c, "ratios-ttm", "ratios-ttm", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	if into == nil {
		into = &FundamentalSummary{Symbol: symbol, Source: c.source()}
	}
	r := rows[0]
	into.PERatio = r.PriceToEarningsRatioTTM
	into.PriceToBook = r.PriceToBookRatioTTM
	into.PriceToSales = r.PriceToSalesRatioTTM
	into.DividendYield = r.DividendYieldTTM
	into.DebtToEquity = r.DebtToEquityRatioTTM
	into.NetProfitMargin = r.NetProfitMarginTTM
	into.EPS = r.NetIncomePerShareTTM
	if into.CurrentRatio == nil {
		into.CurrentRatio = r.CurrentRatioTTM
	}
	return into, nil
}

// Analysts reads the consensus rail: the grade distribution and the price
// target. Either half may be missing for a thinly covered symbol, so a missing
// half is dropped rather than failing the whole rail.
func (c *FMP) Analysts(ctx context.Context, symbol string) (*Analysts, error) {
	out := &Analysts{}
	found := false
	if rows, err := fmpList[fmpGradesConsensus](ctx, c, "grades-consensus", "grades-consensus", map[string]string{"symbol": symbol}); err == nil {
		g := rows[0]
		out.StrongBuy, out.Buy, out.Hold, out.Sell, out.StrongSell = g.StrongBuy, g.Buy, g.Hold, g.Sell, g.StrongSell
		out.Consensus = g.Consensus
		found = true
	} else if f := FailureOf(err); f != nil && !f.Retryable() && f.Kind != FailureNotFound {
		return nil, err
	}
	if rows, err := fmpList[fmpPriceTargetConsensus](ctx, c, "price-target-consensus", "price-target-consensus", map[string]string{"symbol": symbol}); err == nil {
		t := rows[0]
		out.TargetHigh, out.TargetLow, out.TargetMedian, out.TargetMean = t.TargetHigh, t.TargetLow, t.TargetMedian, t.TargetConsensus
		found = true
	} else if f := FailureOf(err); f != nil && !f.Retryable() && f.Kind != FailureNotFound {
		return nil, err
	}
	if !found {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: "analysts",
			Message: "No analyst coverage was returned for that symbol.",
		}
	}
	return out, nil
}

/* -------------------------------------------------- earnings & dividends -- */

type fmpEarnings struct {
	Symbol           string   `json:"symbol"`
	Date             string   `json:"date"`
	EPSActual        *float64 `json:"epsActual"`
	EPSEstimated     *float64 `json:"epsEstimated"`
	RevenueActual    *float64 `json:"revenueActual"`
	RevenueEstimated *float64 `json:"revenueEstimated"`
}

// Earnings reads a symbol's reported and scheduled earnings.
func (c *FMP) Earnings(ctx context.Context, symbol string, limit int) (*EarningsHistory, error) {
	rows, err := fmpList[fmpEarnings](ctx, c, "earnings", "earnings", map[string]string{
		"symbol": symbol, "limit": limitParam(limit, 20),
	})
	if err != nil {
		return nil, err
	}
	out := &EarningsHistory{Symbol: symbol, Source: c.source()}
	for _, r := range rows {
		out.Events = append(out.Events, EarningsEvent{
			Symbol: r.Symbol, Date: r.Date,
			EPSActual: r.EPSActual, EPSEstimated: r.EPSEstimated,
			RevenueActual: r.RevenueActual, RevenueEstimated: r.RevenueEstimated,
		})
	}
	return out, nil
}

type fmpDividend struct {
	Symbol      string   `json:"symbol"`
	Date        string   `json:"date"`
	RecordDate  string   `json:"recordDate"`
	PaymentDate string   `json:"paymentDate"`
	Dividend    *float64 `json:"dividend"`
	Yield       *float64 `json:"yield"`
	Frequency   string   `json:"frequency"`
}

// Dividends reads a symbol's dividend history.
func (c *FMP) Dividends(ctx context.Context, symbol string, limit int) (*DividendHistory, error) {
	rows, err := fmpList[fmpDividend](ctx, c, "dividends", "dividends", map[string]string{
		"symbol": symbol, "limit": limitParam(limit, 20),
	})
	if err != nil {
		return nil, err
	}
	out := &DividendHistory{Symbol: symbol, Source: c.source()}
	for _, r := range rows {
		out.Events = append(out.Events, DividendEvent{
			Symbol: r.Symbol, Date: r.Date, RecordDate: r.RecordDate, PaymentDate: r.PaymentDate,
			Amount: r.Dividend, Yield: r.Yield, Frequency: r.Frequency,
		})
	}
	return out, nil
}

/* ------------------------------------------------- market status & macro -- */

type fmpMarketHours struct {
	Exchange     string `json:"exchange"`
	Name         string `json:"name"`
	OpeningHour  string `json:"openingHour"`
	ClosingHour  string `json:"closingHour"`
	Timezone     string `json:"timezone"`
	IsMarketOpen bool   `json:"isMarketOpen"`
}

// MarketHours reads one exchange's session state.
func (c *FMP) MarketHours(ctx context.Context, exchange string) (*MarketStatus, error) {
	rows, err := fmpList[fmpMarketHours](ctx, c, "exchange-market-hours", "exchange-market-hours", map[string]string{"exchange": exchange})
	if err != nil {
		return nil, err
	}
	out := &MarketStatus{Source: c.source()}
	for _, r := range rows {
		out.Sessions = append(out.Sessions, MarketSession{
			Exchange: r.Exchange, Name: r.Name, Timezone: r.Timezone,
			OpenTime: r.OpeningHour, CloseTime: r.ClosingHour, IsOpen: r.IsMarketOpen,
		})
	}
	return out, nil
}

type fmpTreasuryRates map[string]json.RawMessage

// TreasuryRates reads the yield curve as one series per tenor, keyed by the
// vendor's own tenor names.
func (c *FMP) TreasuryRates(ctx context.Context, from, to string) (map[string]*EconomicSeries, error) {
	rows, err := fmpList[fmpTreasuryRates](ctx, c, "treasury-rates", "treasury-rates", map[string]string{"from": from, "to": to})
	if err != nil {
		return nil, err
	}
	src := c.source()
	out := map[string]*EconomicSeries{}
	for _, row := range rows {
		var date string
		if raw, ok := row["date"]; ok {
			_ = json.Unmarshal(raw, &date)
		}
		if date == "" {
			continue
		}
		for tenor, raw := range row {
			if tenor == "date" {
				continue
			}
			var v float64
			if err := json.Unmarshal(raw, &v); err != nil {
				continue
			}
			s, ok := out[tenor]
			if !ok {
				s = &EconomicSeries{Name: tenor, Unit: "percent", Source: src}
				out[tenor] = s
			}
			s.Points = append(s.Points, EconomicPoint{Date: date, Value: v})
		}
	}
	if len(out) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: "treasury-rates",
			Message: "No treasury rates were returned for that range.",
		}
	}
	return out, nil
}

type fmpEconomicPoint struct {
	Name  string   `json:"name"`
	Date  string   `json:"date"`
	Value *float64 `json:"value"`
}

// EconomicSeries reads one documented macro series by its vendor name.
func (c *FMP) EconomicSeries(ctx context.Context, name, from, to string) (*EconomicSeries, error) {
	if strings.TrimSpace(name) == "" {
		return nil, &Failure{
			Kind: FailureBadRequest, Provider: ProviderFMP, Endpoint: "economic-indicators",
			Message: "No economic series was named.",
		}
	}
	rows, err := fmpList[fmpEconomicPoint](ctx, c, "economic-indicators", "economic-indicators", map[string]string{
		"name": name, "from": from, "to": to,
	})
	if err != nil {
		return nil, err
	}
	out := &EconomicSeries{Name: name, Source: c.source()}
	for _, r := range rows {
		if r.Value == nil {
			continue
		}
		out.Points = append(out.Points, EconomicPoint{Date: r.Date, Value: *r.Value})
	}
	if len(out.Points) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: "economic-indicators",
			Message: "No observations were returned for that series.",
		}
	}
	return out, nil
}

/* --------------------------------------------------------- market boards -- */

type fmpShortQuote struct {
	Symbol string   `json:"symbol"`
	Price  *float64 `json:"price"`
	Change *float64 `json:"change"`
	Volume *int64   `json:"volume"`
}

func (c *FMP) shortBoard(ctx context.Context, endpoint, path string, class AssetClass) ([]Quote, error) {
	rows, err := fmpList[fmpShortQuote](ctx, c, endpoint, path, map[string]string{"short": "true"})
	if err != nil {
		return nil, err
	}
	src := c.source()
	out := make([]Quote, 0, len(rows))
	for _, r := range rows {
		q := Quote{Symbol: r.Symbol, Class: class, Price: r.Price, Change: r.Change, Volume: r.Volume, Source: src}
		// The short board carries no previous close, so percent change is
		// derived only when both sides of the arithmetic are real.
		if r.Price != nil && r.Change != nil {
			if prev := *r.Price - *r.Change; prev != 0 {
				pct := (*r.Change / prev) * 100
				q.ChangePercent = &pct
			}
		}
		out = append(out, q)
	}
	return out, nil
}

// IndexBoard reads every index quote in one call.
func (c *FMP) IndexBoard(ctx context.Context) ([]Quote, error) {
	return c.shortBoard(ctx, "batch-index-quotes", "batch-index-quotes", ClassIndex)
}

// CryptoBoard reads every crypto quote in one call.
func (c *FMP) CryptoBoard(ctx context.Context) ([]Quote, error) {
	return c.shortBoard(ctx, "batch-crypto-quotes", "batch-crypto-quotes", ClassCrypto)
}

// CommodityBoard reads every commodity quote in one call.
func (c *FMP) CommodityBoard(ctx context.Context) ([]Quote, error) {
	return c.shortBoard(ctx, "batch-commodity-quotes", "batch-commodity-quotes", ClassCommodity)
}
