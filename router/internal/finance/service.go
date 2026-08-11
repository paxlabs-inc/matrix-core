// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config builds a Service. Keys come from the router's environment and nowhere
// else; the base URLs exist so tests can point the real service at real local
// upstreams.
type Config struct {
	FMPKey       string
	AlphaKey     string
	FMPBaseURL   string
	AlphaBaseURL string
	HTTPClient   *http.Client
	// Now is the clock seam for cache tiers, metering and rate limiting.
	Now func() time.Time
	// RatePerSecond / Burst bound ONE user's calls into the lane.
	RatePerSecond float64
	Burst         float64
}

// ConfigFromEnv reads the deployment's finance configuration. This is the ONLY
// read of the vendor keys in the whole system: nothing below the router — no
// per-user daemon, no browser bundle — ever sees them.
func ConfigFromEnv() Config {
	return Config{
		FMPKey:   strings.TrimSpace(os.Getenv(FMPKeyEnv)),
		AlphaKey: strings.TrimSpace(os.Getenv(AlphaVantageKeyEnv)),
	}
}

// Service is the finance lane: the providers, the cache, the meter and the
// per-user limiter behind one set of methods. Both consumers — the browser's
// /finance route and Neo's finance bridge — call through here, which is what
// makes one cache, one quota and one bill true.
type Service struct {
	FMP     *FMP
	Alpha   *AlphaVantage
	Cache   *Cache
	Meter   *Meter
	Limiter *Limiter
	now     func() time.Time
}

// NewService builds the lane. Absent keys are legal: the service boots and each
// call answers with a typed not-configured failure naming what is missing.
func NewService(cfg Config) *Service {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	fmp := NewFMP(cfg.FMPKey, cfg.FMPBaseURL, client)
	alpha := NewAlphaVantage(cfg.AlphaKey, cfg.AlphaBaseURL, client)
	fmp.tr.now = now
	alpha.tr.now = now
	return &Service{
		FMP:     fmp,
		Alpha:   alpha,
		Cache:   NewCache(now),
		Meter:   NewMeter(now),
		Limiter: NewLimiter(cfg.RatePerSecond, cfg.Burst, now),
		now:     now,
	}
}

// Configured reports which providers hold a key. The diagnostics surface serves
// this without exposing the keys themselves.
func (s *Service) Configured() map[string]bool {
	return map[string]bool{
		string(ProviderFMP):          s.FMP.Configured(),
		string(ProviderAlphaVantage): s.Alpha.Configured(),
	}
}

// Stats is the diagnostics snapshot, including cache size and provider config.
func (s *Service) Stats() Stats {
	out := s.Meter.Snapshot()
	out.CacheEntries = s.Cache.Size()
	out.Providers = s.Configured()
	return out
}

// req describes one served call for the cache key and the metering record.
type req struct {
	User     string
	Class    Class
	Endpoint string
	Key      string
}

// rateLimited is the typed refusal for OUR per-user bound, distinct from a
// vendor throttle so the two are never confused in the counters or the UI.
func rateLimited(endpoint string) *Failure {
	return &Failure{
		Kind: FailureRateLimited, Provider: ProviderFMP, Endpoint: endpoint,
		Message:    "You are asking for market data faster than this account is allowed.",
		RetryAfter: time.Second,
	}
}

// serve is the one path every finance call takes: rate limit, cache with
// single-flight collapse, primary provider, declared fallback on a retryable
// failure, then the metering record.
//
// The fallback is only tried when the primary's failure is retryable — a
// malformed request or a genuine "no such symbol" is not worth a second vendor
// call, and burning the fallback's quota on it would be the expensive kind of
// wrong.
func (s *Service) serve(ctx context.Context, r req, primary func(context.Context) (any, error), fallback func(context.Context) (any, error)) (any, Source, error) {
	if !s.Limiter.Allow(r.User) {
		err := rateLimited(r.Endpoint)
		s.Meter.Observe(Record{Provider: ProviderFMP, Endpoint: r.Endpoint, User: r.User, Err: err})
		return nil, Source{}, err
	}

	usedFallback := false
	value, outcome, err := s.Cache.Do(ctx, r.Key, TTLFor(r.Class), func(ctx context.Context) (any, error) {
		out, primaryErr := primary(ctx)
		if primaryErr == nil {
			return out, nil
		}
		if fallback == nil {
			return nil, primaryErr
		}
		f := FailureOf(primaryErr)
		if f != nil && !f.Retryable() {
			return nil, primaryErr
		}
		out, fallbackErr := fallback(ctx)
		if fallbackErr != nil {
			// The primary's failure is the more informative one to report:
			// it is the provider that was supposed to answer.
			return nil, primaryErr
		}
		usedFallback = true
		return out, nil
	})

	provider := ProviderFMP
	if usedFallback {
		provider = ProviderAlphaVantage
	}
	s.Meter.Observe(Record{
		Provider: provider, Endpoint: r.Endpoint, User: r.User,
		CacheHit: outcome.CacheHit, Stale: outcome.Stale, Fallback: usedFallback,
		Latency: outcome.Latency, Err: err,
	})
	if err != nil {
		return nil, Source{}, err
	}
	return value, Source{Provider: provider, FetchedAt: s.now().UTC(), Fallback: usedFallback, Stale: outcome.Stale}, nil
}

// key builds a cache key. Keys are shared across users on purpose: market data
// is not user-specific, and sharing the entry is what makes the agent and the UI
// one quota.
func key(parts ...string) string {
	return strings.Join(parts, "|")
}

/* ---------------------------------------------------------------- quotes -- */

// Quote reads one instrument, falling back to Alpha Vantage's global quote.
func (s *Service) Quote(ctx context.Context, user, symbol string) (*Quote, error) {
	symbol = normalizeSymbol(symbol)
	if symbol == "" {
		return nil, &Failure{Kind: FailureBadRequest, Endpoint: "quote", Message: "No symbol was given."}
	}
	value, src, err := s.serve(ctx, req{User: user, Class: ClassQuote, Endpoint: "quote", Key: key("quote", symbol)},
		func(ctx context.Context) (any, error) { return s.FMP.Quote(ctx, symbol) },
		func(ctx context.Context) (any, error) { return s.Alpha.GlobalQuote(ctx, symbol) },
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*Quote))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// BatchQuote reads many instruments in one call — the index strip, a watchlist,
// a per-market list.
func (s *Service) BatchQuote(ctx context.Context, user string, symbols []string) (*QuoteBoard, error) {
	cleaned := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		if n := normalizeSymbol(sym); n != "" {
			cleaned = append(cleaned, n)
		}
	}
	if len(cleaned) == 0 {
		return nil, &Failure{Kind: FailureBadRequest, Endpoint: "batch-quote", Message: "No symbols were given."}
	}
	value, src, err := s.serve(ctx, req{User: user, Class: ClassQuote, Endpoint: "batch-quote", Key: key("batch", strings.Join(cleaned, ","))},
		func(ctx context.Context) (any, error) { return s.FMP.BatchQuote(ctx, cleaned) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	return &QuoteBoard{Quotes: value.([]Quote), Source: src}, nil
}

// ExtendedQuote reads the pre/post-market state, folding the aftermarket book
// and the last aftermarket print into one answer.
func (s *Service) ExtendedQuote(ctx context.Context, user, symbol string) (*ExtendedQuote, error) {
	symbol = normalizeSymbol(symbol)
	value, _, err := s.serve(ctx, req{User: user, Class: ClassQuote, Endpoint: "aftermarket", Key: key("extended", symbol)},
		func(ctx context.Context) (any, error) {
			book, bookErr := s.FMP.ExtendedQuote(ctx, symbol)
			trade, tradeErr := s.FMP.ExtendedTrade(ctx, symbol)
			switch {
			case bookErr != nil && tradeErr != nil:
				return nil, bookErr
			case bookErr != nil:
				return trade, nil
			case tradeErr != nil:
				return book, nil
			}
			book.Price = trade.Price
			if trade.AsOf != nil {
				book.AsOf = trade.AsOf
			}
			return book, nil
		},
		nil,
	)
	if err != nil {
		return nil, err
	}
	return value.(*ExtendedQuote), nil
}

// PriceChange reads the multi-window performance strip.
func (s *Service) PriceChange(ctx context.Context, user, symbol string) (*PriceChange, error) {
	symbol = normalizeSymbol(symbol)
	value, src, err := s.serve(ctx, req{User: user, Class: ClassQuote, Endpoint: "price-change", Key: key("change", symbol)},
		func(ctx context.Context) (any, error) { return s.FMP.PriceChange(ctx, symbol) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*PriceChange))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

/* ---------------------------------------------------------------- series -- */

// Series reads a chart's data. The cache tier follows the resolution: an
// intraday bar cannot change faster than its own width, and a daily bar settles
// once a session.
func (s *Service) Series(ctx context.Context, user, symbol string, interval Interval, from, to string) (*Series, error) {
	symbol = normalizeSymbol(symbol)
	if symbol == "" {
		return nil, &Failure{Kind: FailureBadRequest, Endpoint: "series", Message: "No symbol was given."}
	}
	class := ClassSeriesDaily
	if interval.Intraday() {
		class = ClassSeriesLive
	}
	full := !interval.Intraday()
	value, src, err := s.serve(ctx, req{User: user, Class: class, Endpoint: "series", Key: key("series", symbol, string(interval), from, to)},
		func(ctx context.Context) (any, error) { return s.FMP.Series(ctx, symbol, interval, from, to) },
		func(ctx context.Context) (any, error) { return s.Alpha.Series(ctx, symbol, interval, full) },
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*Series))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

/* --------------------------------------------------- profile & reference -- */

// Profile reads the company identity rail.
func (s *Service) Profile(ctx context.Context, user, symbol string) (*Profile, error) {
	symbol = normalizeSymbol(symbol)
	value, src, err := s.serve(ctx, req{User: user, Class: ClassProfile, Endpoint: "profile", Key: key("profile", symbol)},
		func(ctx context.Context) (any, error) { return s.FMP.Profile(ctx, symbol) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*Profile))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// Search resolves a query to symbols, falling back to Alpha Vantage.
func (s *Service) Search(ctx context.Context, user, query string, limit int) (*SearchResults, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, &Failure{Kind: FailureBadRequest, Endpoint: "search", Message: "No search term was given."}
	}
	value, src, err := s.serve(ctx, req{User: user, Class: ClassSearch, Endpoint: "search", Key: key("search", strings.ToLower(query))},
		func(ctx context.Context) (any, error) { return s.FMP.Search(ctx, query, limit) },
		func(ctx context.Context) (any, error) { return s.Alpha.Search(ctx, query) },
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*SearchResults))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

/* ------------------------------------------------------- market surfaces -- */

// Movers reads a ranked market list, falling back to Alpha Vantage.
func (s *Service) Movers(ctx context.Context, user string, kind MoverKind) (*MoverList, error) {
	value, src, err := s.serve(ctx, req{User: user, Class: ClassMovers, Endpoint: "movers", Key: key("movers", string(kind))},
		func(ctx context.Context) (any, error) { return s.FMP.Movers(ctx, kind) },
		func(ctx context.Context) (any, error) { return s.Alpha.Movers(ctx, kind) },
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*MoverList))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// Sectors reads the sector performance board. An absent date means the most
// recent session, which the vendor requires be named explicitly.
func (s *Service) Sectors(ctx context.Context, user, date, exchange string) (*SectorBoard, error) {
	if strings.TrimSpace(date) == "" {
		date = s.now().UTC().Format("2006-01-02")
	}
	value, src, err := s.serve(ctx, req{User: user, Class: ClassMovers, Endpoint: "sectors", Key: key("sectors", date, exchange)},
		func(ctx context.Context) (any, error) { return s.FMP.Sectors(ctx, date, exchange) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*SectorBoard))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// boardSymbols is the bounded, reader-facing universe for each market tab.
// The UI renders at most twelve rows, so asking FMP's bulk endpoints for every
// instrument wastes bandwidth and makes an ordinary page load depend on the
// vendor's bulk-delivery tier.
func boardSymbols(class AssetClass) []string {
	switch class {
	case ClassEquity:
		return []string{"AAPL", "MSFT", "NVDA", "AMZN", "GOOGL", "META", "TSLA", "BRK-B", "JPM", "LLY", "AVGO", "WMT"}
	case ClassIndex:
		return []string{"^GSPC", "^IXIC", "^DJI", "^RUT", "^VIX", "^FTSE", "^GDAXI", "^FCHI", "^N225", "^HSI", "^STOXX50E", "^AXJO"}
	case ClassCrypto:
		return []string{"BTCUSD", "ETHUSD", "SOLUSD", "XRPUSD", "BNBUSD", "DOGEUSD", "ADAUSD", "AVAXUSD", "LINKUSD", "DOTUSD", "LTCUSD", "BCHUSD"}
	case ClassForex:
		return []string{"EURUSD", "GBPUSD", "USDJPY", "USDCHF", "AUDUSD", "USDCAD", "NZDUSD", "EURGBP", "EURJPY", "GBPJPY", "USDSEK", "USDNOK"}
	case ClassCommodity:
		return []string{"GCUSD", "SIUSD", "CLUSD", "NGUSD", "HGUSD", "ZCUSD", "ZWUSD", "ZSUSD", "KCUSD", "CTUSD", "SBUSD", "CCUSD"}
	default:
		return nil
	}
}

// Board reads the curated top instruments for one asset class in one quote
// call. The provider-specific all-market batch clients remain available for
// workflows that genuinely need the full universe, but they are intentionally
// not on this latency- and bandwidth-sensitive page path.
func (s *Service) Board(ctx context.Context, user string, class AssetClass) (*QuoteBoard, error) {
	symbols := boardSymbols(class)
	if len(symbols) == 0 {
		return nil, &Failure{Kind: FailureBadRequest, Endpoint: "board", Message: "That market is not available as a board."}
	}
	value, src, err := s.serve(ctx, req{User: user, Class: ClassQuote, Endpoint: "board", Key: key("board", string(class))},
		func(ctx context.Context) (any, error) { return s.FMP.QuoteList(ctx, symbols, class) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	return &QuoteBoard{Quotes: value.([]Quote), Source: src}, nil
}

/* ------------------------------------------------------------------ news -- */

// NewsScope selects which stream to read.
type NewsScope string

const (
	NewsMarket  NewsScope = "market"
	NewsStocks  NewsScope = "stocks"
	NewsPress   NewsScope = "press"
	NewsSymbols NewsScope = "symbols"
)

// News reads a stream. Symbol news prefers Alpha Vantage's sentiment-scored
// feed as the fallback, which is the one thing FMP's news cannot carry.
func (s *Service) News(ctx context.Context, user string, scope NewsScope, symbols []string, limit int) (*NewsFeed, error) {
	cacheKey := key("news", string(scope), strings.Join(symbols, ","), limitParam(limit, 20))
	var primary, fallback func(context.Context) (any, error)
	switch scope {
	case NewsSymbols:
		if len(symbols) == 0 {
			return nil, &Failure{Kind: FailureBadRequest, Endpoint: "news", Message: "No symbols were given."}
		}
		primary = func(ctx context.Context) (any, error) { return s.FMP.SymbolNews(ctx, symbols, limit) }
		fallback = func(ctx context.Context) (any, error) { return s.Alpha.NewsSentiment(ctx, symbols, "", limit) }
	case NewsPress:
		primary = func(ctx context.Context) (any, error) { return s.FMP.PressReleases(ctx, limit) }
	case NewsStocks:
		primary = func(ctx context.Context) (any, error) { return s.FMP.StockNews(ctx, limit) }
	default:
		primary = func(ctx context.Context) (any, error) { return s.FMP.MarketNews(ctx, limit) }
		fallback = func(ctx context.Context) (any, error) {
			return s.Alpha.NewsSentiment(ctx, nil, "financial_markets", limit)
		}
	}
	value, src, err := s.serve(ctx, req{User: user, Class: ClassNews, Endpoint: "news", Key: cacheKey}, primary, fallback)
	if err != nil {
		return nil, err
	}
	out := *(value.(*NewsFeed))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

/* ---------------------------------------------------------- fundamentals -- */

// Fundamentals reads the stats bar and the analyst rail in one answer. The
// halves are independent: a symbol with metrics but no coverage still gets its
// metrics, and vice versa.
func (s *Service) Fundamentals(ctx context.Context, user, symbol string) (*FundamentalSummary, error) {
	symbol = normalizeSymbol(symbol)
	value, src, err := s.serve(ctx, req{User: user, Class: ClassFundamentals, Endpoint: "fundamentals", Key: key("fundamentals", symbol)},
		func(ctx context.Context) (any, error) {
			summary := &FundamentalSummary{Symbol: symbol, Source: s.FMP.source()}
			// The three halves are independent. A vendor gap in one is folded
			// over; a gap in ALL of them means there is nothing to show, and
			// saying so beats handing the stats bar an empty struct to render
			// as a row of blanks.
			filled := false
			absent := func(err error) bool {
				f := FailureOf(err)
				return f != nil && f.Kind == FailureNotFound
			}
			if _, err := s.FMP.KeyMetrics(ctx, symbol, summary); err == nil {
				filled = true
			} else if !absent(err) {
				return nil, err
			}
			if _, err := s.FMP.Ratios(ctx, symbol, summary); err == nil {
				filled = true
			} else if !absent(err) {
				return nil, err
			}
			if analysts, err := s.FMP.Analysts(ctx, symbol); err == nil {
				summary.Consensus = analysts
				filled = true
			}
			if !filled {
				return nil, &Failure{
					Kind: FailureNotFound, Provider: ProviderFMP, Endpoint: "fundamentals",
					Message: "No fundamentals were returned for that symbol.",
				}
			}
			return summary, nil
		},
		nil,
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*FundamentalSummary))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// Earnings reads a symbol's earnings run.
func (s *Service) Earnings(ctx context.Context, user, symbol string, limit int) (*EarningsHistory, error) {
	symbol = normalizeSymbol(symbol)
	value, src, err := s.serve(ctx, req{User: user, Class: ClassFundamentals, Endpoint: "earnings", Key: key("earnings", symbol, limitParam(limit, 20))},
		func(ctx context.Context) (any, error) { return s.FMP.Earnings(ctx, symbol, limit) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*EarningsHistory))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// Dividends reads a symbol's dividend history.
func (s *Service) Dividends(ctx context.Context, user, symbol string, limit int) (*DividendHistory, error) {
	symbol = normalizeSymbol(symbol)
	value, src, err := s.serve(ctx, req{User: user, Class: ClassFundamentals, Endpoint: "dividends", Key: key("dividends", symbol, limitParam(limit, 20))},
		func(ctx context.Context) (any, error) { return s.FMP.Dividends(ctx, symbol, limit) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*DividendHistory))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

/* ------------------------------------------------------- status  & macro -- */

// MarketStatus reads the open/closed board. Alpha Vantage's global board is the
// primary here — it covers every region in one call, which FMP's per-exchange
// endpoint does not.
func (s *Service) MarketStatus(ctx context.Context, user, exchange string) (*MarketStatus, error) {
	value, src, err := s.serve(ctx, req{User: user, Class: ClassStatus, Endpoint: "market-status", Key: key("status", exchange)},
		func(ctx context.Context) (any, error) {
			if strings.TrimSpace(exchange) != "" {
				return s.FMP.MarketHours(ctx, exchange)
			}
			return s.Alpha.MarketStatus(ctx)
		},
		func(ctx context.Context) (any, error) {
			if strings.TrimSpace(exchange) != "" {
				return s.Alpha.MarketStatus(ctx)
			}
			return s.FMP.MarketHours(ctx, "NASDAQ")
		},
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*MarketStatus))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// Macro reads one economic series by its vendor name.
func (s *Service) Macro(ctx context.Context, user, name, from, to string) (*EconomicSeries, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, &Failure{Kind: FailureBadRequest, Endpoint: "macro", Message: "No economic series was named."}
	}
	value, src, err := s.serve(ctx, req{User: user, Class: ClassMacro, Endpoint: "macro", Key: key("macro", name, from, to)},
		func(ctx context.Context) (any, error) { return s.FMP.EconomicSeries(ctx, name, from, to) },
		func(ctx context.Context) (any, error) {
			return s.Alpha.EconomicSeries(ctx, strings.ToUpper(name), nil)
		},
	)
	if err != nil {
		return nil, err
	}
	out := *(value.(*EconomicSeries))
	out.Source = mergeSource(out.Source, src)
	return &out, nil
}

// TreasuryRates reads the yield curve.
func (s *Service) TreasuryRates(ctx context.Context, user, from, to string) (map[string]*EconomicSeries, error) {
	value, _, err := s.serve(ctx, req{User: user, Class: ClassMacro, Endpoint: "treasury", Key: key("treasury", from, to)},
		func(ctx context.Context) (any, error) { return s.FMP.TreasuryRates(ctx, from, to) },
		nil,
	)
	if err != nil {
		return nil, err
	}
	return value.(map[string]*EconomicSeries), nil
}

/* ----------------------------------------------------------------- utils -- */

// normalizeSymbol upper-cases and trims a ticker. Vendors are case-sensitive on
// some venues and forgiving on others; normalizing here also keeps the cache
// from holding "aapl" and "AAPL" as two entries against one quota.
func normalizeSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// mergeSource keeps the provider's own fetched-at stamp while adopting the
// lane's fallback and staleness facts, which only the service knows.
func mergeSource(inner, outer Source) Source {
	if inner.Provider == "" {
		inner.Provider = outer.Provider
	}
	if inner.FetchedAt.IsZero() {
		inner.FetchedAt = outer.FetchedAt
	}
	inner.Fallback = outer.Fallback
	inner.Stale = outer.Stale
	return inner
}
