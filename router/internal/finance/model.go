// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package finance is the router's market-data lane: the ONE place a Centra AI
// finance call is served from, whether it came from the browser's /finance route
// or from Neo's finance MCP bridge.
//
// Everything above this package speaks the normalized model declared here and
// never a vendor's field names. Two rules govern the shapes:
//
//   - Absent means absent. Every optional number is a pointer with omitempty, so
//     a field the vendor did not return is missing from the payload rather than
//     silently zero. A zero price and "no price" are different facts and the UI
//     renders them differently.
//   - Every payload names where it came from and when. Source rides on each
//     result so a stale panel can say so and a fallback is visible rather than
//     inferred.
package finance

import "time"

// Provider identifies an upstream vendor.
type Provider string

const (
	ProviderFMP          Provider = "fmp"
	ProviderAlphaVantage Provider = "alphavantage"
)

// Source is the provenance stamp carried by every result: which vendor answered,
// when it was fetched, and whether the answer came from the declared fallback
// rather than the primary.
type Source struct {
	Provider  Provider  `json:"provider"`
	FetchedAt time.Time `json:"fetched_at"`
	Fallback  bool      `json:"fallback,omitempty"`
	// Stale marks a result served from cache after the upstream refused a
	// refresh (throttle/outage). The UI labels it rather than hiding it.
	Stale bool `json:"stale,omitempty"`
}

// AssetClass groups the instrument kinds the suite covers through one set of
// components.
type AssetClass string

const (
	ClassEquity    AssetClass = "equity"
	ClassIndex     AssetClass = "index"
	ClassCrypto    AssetClass = "crypto"
	ClassForex     AssetClass = "forex"
	ClassCommodity AssetClass = "commodity"
)

// Quote is one instrument's current pricing state. It covers equities, indexes,
// crypto, forex and commodities — the vendors return the same shape for all of
// them, and the UI renders them with the same components.
type Quote struct {
	Symbol        string     `json:"symbol"`
	Name          string     `json:"name,omitempty"`
	Exchange      string     `json:"exchange,omitempty"`
	Class         AssetClass `json:"class,omitempty"`
	Currency      string     `json:"currency,omitempty"`
	Price         *float64   `json:"price,omitempty"`
	Change        *float64   `json:"change,omitempty"`
	ChangePercent *float64   `json:"change_percent,omitempty"`
	Open          *float64   `json:"open,omitempty"`
	DayHigh       *float64   `json:"day_high,omitempty"`
	DayLow        *float64   `json:"day_low,omitempty"`
	PreviousClose *float64   `json:"previous_close,omitempty"`
	YearHigh      *float64   `json:"year_high,omitempty"`
	YearLow       *float64   `json:"year_low,omitempty"`
	Volume        *int64     `json:"volume,omitempty"`
	AvgVolume     *int64     `json:"avg_volume,omitempty"`
	MarketCap     *float64   `json:"market_cap,omitempty"`
	PriceAvg50    *float64   `json:"price_avg_50,omitempty"`
	PriceAvg200   *float64   `json:"price_avg_200,omitempty"`
	// AsOf is the vendor's own timestamp for the quote, distinct from
	// Source.FetchedAt (when we asked).
	AsOf *time.Time `json:"as_of,omitempty"`
	// Extended is the pre/post-market book when the session is outside regular
	// hours and the vendor has one.
	Extended *ExtendedQuote `json:"extended,omitempty"`
	Source   Source         `json:"source"`
}

// QuoteBoard is a set of quotes answered by one upstream call — the index
// strip, a watchlist, or a whole-market board. It carries one Source for the
// set rather than repeating it per row.
type QuoteBoard struct {
	Quotes []Quote `json:"quotes"`
	Source Source  `json:"source"`
}

// ExtendedQuote is the pre/post-market state. Session names the side of the
// regular session it belongs to so the UI can label it ("Pre-market",
// "After hours") without guessing from the clock.
type ExtendedQuote struct {
	Session       string     `json:"session,omitempty"`
	Price         *float64   `json:"price,omitempty"`
	Change        *float64   `json:"change,omitempty"`
	ChangePercent *float64   `json:"change_percent,omitempty"`
	BidPrice      *float64   `json:"bid_price,omitempty"`
	AskPrice      *float64   `json:"ask_price,omitempty"`
	Volume        *int64     `json:"volume,omitempty"`
	AsOf          *time.Time `json:"as_of,omitempty"`
}

// PriceChange is the multi-window performance strip (1D … max) the symbol page
// shows under the chart. Windows are keyed by the vendor's own labels
// ("1D", "5D", "1M", "3M", "6M", "ytd", "1Y", "3Y", "5Y", "10Y", "max") and
// carry percent change.
type PriceChange struct {
	Symbol  string             `json:"symbol"`
	Windows map[string]float64 `json:"windows"`
	Source  Source             `json:"source"`
}

// Interval is a series resolution. The values are the wire vocabulary the client
// and the agent use; each provider maps them to its own path or function.
type Interval string

const (
	Interval1Min  Interval = "1min"
	Interval5Min  Interval = "5min"
	Interval15Min Interval = "15min"
	Interval30Min Interval = "30min"
	Interval1Hour Interval = "1hour"
	Interval4Hour Interval = "4hour"
	IntervalDay   Interval = "1day"
	IntervalWeek  Interval = "1week"
	IntervalMonth Interval = "1month"
)

// Intraday reports whether an interval is an intraday resolution — the cache
// tier and the vendor endpoint both depend on it.
func (i Interval) Intraday() bool {
	switch i {
	case Interval1Min, Interval5Min, Interval15Min, Interval30Min, Interval1Hour, Interval4Hour:
		return true
	}
	return false
}

// Candle is one OHLCV bar. Volume is optional because index and some forex
// series do not carry it.
type Candle struct {
	Time   time.Time `json:"t"`
	Open   float64   `json:"o"`
	High   float64   `json:"h"`
	Low    float64   `json:"l"`
	Close  float64   `json:"c"`
	Volume *int64    `json:"v,omitempty"`
}

// Series is an ordered OHLCV run, oldest first. The chart consumes it directly.
type Series struct {
	Symbol   string   `json:"symbol"`
	Interval Interval `json:"interval"`
	Candles  []Candle `json:"candles"`
	Source   Source   `json:"source"`
}

// Profile is the company identity rail on the symbol page.
type Profile struct {
	Symbol       string   `json:"symbol"`
	Name         string   `json:"name,omitempty"`
	Exchange     string   `json:"exchange,omitempty"`
	ExchangeName string   `json:"exchange_name,omitempty"`
	Currency     string   `json:"currency,omitempty"`
	Sector       string   `json:"sector,omitempty"`
	Industry     string   `json:"industry,omitempty"`
	Country      string   `json:"country,omitempty"`
	CEO          string   `json:"ceo,omitempty"`
	Employees    *int64   `json:"employees,omitempty"`
	Website      string   `json:"website,omitempty"`
	Description  string   `json:"description,omitempty"`
	ImageURL     string   `json:"image_url,omitempty"`
	IPODate      string   `json:"ipo_date,omitempty"`
	MarketCap    *float64 `json:"market_cap,omitempty"`
	Beta         *float64 `json:"beta,omitempty"`
	IsETF        bool     `json:"is_etf,omitempty"`
	IsFund       bool     `json:"is_fund,omitempty"`
	IsActive     *bool    `json:"is_active,omitempty"`
	Range        string   `json:"range,omitempty"`
	Source       Source   `json:"source"`
}

// SearchMatch is one symbol-search hit.
type SearchMatch struct {
	Symbol       string     `json:"symbol"`
	Name         string     `json:"name,omitempty"`
	Exchange     string     `json:"exchange,omitempty"`
	ExchangeName string     `json:"exchange_name,omitempty"`
	Currency     string     `json:"currency,omitempty"`
	Class        AssetClass `json:"class,omitempty"`
}

// SearchResults is a symbol search answer.
type SearchResults struct {
	Query   string        `json:"query"`
	Matches []SearchMatch `json:"matches"`
	Source  Source        `json:"source"`
}

// Mover is one row of a ranked market list (gainers, losers, most active).
type Mover struct {
	Symbol        string   `json:"symbol"`
	Name          string   `json:"name,omitempty"`
	Exchange      string   `json:"exchange,omitempty"`
	Price         *float64 `json:"price,omitempty"`
	Change        *float64 `json:"change,omitempty"`
	ChangePercent *float64 `json:"change_percent,omitempty"`
	Volume        *int64   `json:"volume,omitempty"`
}

// MoverKind names which ranked list is being asked for.
type MoverKind string

const (
	MoversGainers MoverKind = "gainers"
	MoversLosers  MoverKind = "losers"
	MoversActive  MoverKind = "active"
)

// MoverList is a ranked market list.
type MoverList struct {
	Kind   MoverKind `json:"kind"`
	Movers []Mover   `json:"movers"`
	Source Source    `json:"source"`
}

// SectorPerformance is one sector's average change for a session.
type SectorPerformance struct {
	Sector        string  `json:"sector"`
	Exchange      string  `json:"exchange,omitempty"`
	ChangePercent float64 `json:"change_percent"`
	Date          string  `json:"date,omitempty"`
}

// SectorBoard is the sector strip on the markets home.
type SectorBoard struct {
	Sectors []SectorPerformance `json:"sectors"`
	Source  Source              `json:"source"`
}

// NewsItem is one story. Sentiment is present only when the provider scores it
// (Alpha Vantage does; FMP does not), so it stays a pointer.
type NewsItem struct {
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	Publisher   string     `json:"publisher,omitempty"`
	Site        string     `json:"site,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	ImageURL    string     `json:"image_url,omitempty"`
	Symbols     []string   `json:"symbols,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Sentiment   *Sentiment `json:"sentiment,omitempty"`
}

// Sentiment is Alpha Vantage's scored read on a story.
type Sentiment struct {
	Score float64 `json:"score"`
	Label string  `json:"label,omitempty"`
}

// NewsFeed is a news stream (market-wide or per symbol).
type NewsFeed struct {
	Items  []NewsItem `json:"items"`
	Source Source     `json:"source"`
}

// FundamentalSummary is the stats bar and the analyst rail: the handful of TTM
// figures a market page actually shows, not a full statement dump.
type FundamentalSummary struct {
	Symbol            string    `json:"symbol"`
	MarketCap         *float64  `json:"market_cap,omitempty"`
	PERatio           *float64  `json:"pe_ratio,omitempty"`
	ForwardPE         *float64  `json:"forward_pe,omitempty"`
	PriceToBook       *float64  `json:"price_to_book,omitempty"`
	PriceToSales      *float64  `json:"price_to_sales,omitempty"`
	DividendYield     *float64  `json:"dividend_yield,omitempty"`
	EPS               *float64  `json:"eps,omitempty"`
	ReturnOnEquity    *float64  `json:"return_on_equity,omitempty"`
	NetProfitMargin   *float64  `json:"net_profit_margin,omitempty"`
	DebtToEquity      *float64  `json:"debt_to_equity,omitempty"`
	CurrentRatio      *float64  `json:"current_ratio,omitempty"`
	EnterpriseValue   *float64  `json:"enterprise_value,omitempty"`
	FreeCashFlowYield *float64  `json:"free_cash_flow_yield,omitempty"`
	Consensus         *Analysts `json:"analysts,omitempty"`
	Source            Source    `json:"source"`
}

// Analysts is the consensus rail: the grade distribution and the price target.
type Analysts struct {
	StrongBuy    *int     `json:"strong_buy,omitempty"`
	Buy          *int     `json:"buy,omitempty"`
	Hold         *int     `json:"hold,omitempty"`
	Sell         *int     `json:"sell,omitempty"`
	StrongSell   *int     `json:"strong_sell,omitempty"`
	Consensus    string   `json:"consensus,omitempty"`
	TargetHigh   *float64 `json:"target_high,omitempty"`
	TargetLow    *float64 `json:"target_low,omitempty"`
	TargetMedian *float64 `json:"target_median,omitempty"`
	TargetMean   *float64 `json:"target_mean,omitempty"`
}

// EarningsEvent is one reported or scheduled earnings date.
type EarningsEvent struct {
	Symbol           string   `json:"symbol"`
	Date             string   `json:"date"`
	EPSActual        *float64 `json:"eps_actual,omitempty"`
	EPSEstimated     *float64 `json:"eps_estimated,omitempty"`
	RevenueActual    *float64 `json:"revenue_actual,omitempty"`
	RevenueEstimated *float64 `json:"revenue_estimated,omitempty"`
}

// EarningsHistory is a symbol's earnings run.
type EarningsHistory struct {
	Symbol string          `json:"symbol"`
	Events []EarningsEvent `json:"events"`
	Source Source          `json:"source"`
}

// DividendEvent is one dividend.
type DividendEvent struct {
	Symbol      string   `json:"symbol"`
	Date        string   `json:"date"`
	PaymentDate string   `json:"payment_date,omitempty"`
	RecordDate  string   `json:"record_date,omitempty"`
	Amount      *float64 `json:"amount,omitempty"`
	Yield       *float64 `json:"yield,omitempty"`
	Frequency   string   `json:"frequency,omitempty"`
}

// DividendHistory is a symbol's dividend run.
type DividendHistory struct {
	Symbol string          `json:"symbol"`
	Events []DividendEvent `json:"events"`
	Source Source          `json:"source"`
}

// EconomicPoint is one observation in a macro series.
type EconomicPoint struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

// EconomicSeries is a macro series (GDP, CPI, unemployment, treasury yields …).
type EconomicSeries struct {
	Name     string          `json:"name"`
	Unit     string          `json:"unit,omitempty"`
	Interval string          `json:"interval,omitempty"`
	Points   []EconomicPoint `json:"points"`
	Source   Source          `json:"source"`
}

// MarketSession is one exchange's open/closed state.
type MarketSession struct {
	Exchange  string `json:"exchange"`
	Name      string `json:"name,omitempty"`
	Region    string `json:"region,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
	OpenTime  string `json:"open_time,omitempty"`
	CloseTime string `json:"close_time,omitempty"`
	IsOpen    bool   `json:"is_open"`
	Note      string `json:"note,omitempty"`
}

// MarketStatus is the open/closed board.
type MarketStatus struct {
	Sessions []MarketSession `json:"sessions"`
	Source   Source          `json:"source"`
}
