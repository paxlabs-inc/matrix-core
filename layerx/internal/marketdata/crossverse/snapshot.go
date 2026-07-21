package crossverse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const recentTradeCap = 256

var errResubscribe = errors.New("crossverse feed requires resubscribe")

type Level struct {
	PriceCents int64
	Contracts  int64
}

type Trade struct {
	Side              string
	PriceCents        int64
	Contracts         int64
	NotionalMicroUSDX int64
	Liquidation       bool
	TradeTimeMs       int64
}

type NormalizedSnapshot struct {
	Symbol                        string
	SnapshotID                    string
	OrderbookSeq                  int64
	TradeSeq                      int64
	StatsSeq                      int64
	SourceTimestampMs             int64
	ReceivedTimestampMs           int64
	MarkPriceCents                int64
	IndexPriceCents               int64
	BasisPpb                      int64
	BestBidCents                  int64
	BestAskCents                  int64
	Bids                          []Level
	Asks                          []Level
	EstimatedFundingPpb           int64
	LastFundingPpb                int64
	ExternalOpenInterestMicroUSDX int64
	LongShortRatioPpb             int64
	LiquidationVolumeMicroUSDX    int64
	Volume24hMicroUSDX            int64
	Health                        Health
}

type SnapshotRef struct {
	SnapshotID        string
	OrderbookSeq      int64
	StatsSeq          int64
	SourceTimestampMs int64
}

type frame struct {
	Event      string          `json:"event"`
	Topic      string          `json:"topic"`
	TS         json.Number     `json:"ts"`
	SnapshotID string          `json:"snapshotId"`
	Seq        json.Number     `json:"seq"`
	Snapshot   bool            `json:"snapshot"`
	Data       json.RawMessage `json:"data"`
	Message    string          `json:"message"`
}

type topicState struct {
	lastSeq          int64
	snapshotAccepted bool
}

type feed struct {
	mu                 sync.Mutex
	symbol             string
	divergenceLimitBps int64

	phase      Health
	snapshotID string
	orderbook  topicState
	trade      topicState
	stats      topicState

	bids                          []Level
	asks                          []Level
	markPriceCents                int64
	indexPriceCents               int64
	basisPpb                      int64
	bookSourceTsMs                int64
	bookReceivedTsMs              int64
	estimatedFundingPpb           int64
	lastFundingPpb                int64
	externalOpenInterestMicroUSDX int64
	longShortRatioPpb             int64
	liquidationVolumeMicroUSDX    int64
	volume24hMicroUSDX            int64
	statsReceivedTsMs             int64
	recentTrades                  []Trade
}

func newFeed(symbol string, divergenceLimitBps int64) *feed {
	return &feed{symbol: symbol, divergenceLimitBps: divergenceLimitBps, phase: HealthStopped}
}

func (f *feed) setPhase(phase Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase = phase
}

func (f *feed) disconnected() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetTopics()
	f.phase = HealthConnecting
}

func (f *feed) resetTopics() {
	f.orderbook = topicState{}
	f.trade = topicState{}
	f.stats = topicState{}
}

func (f *feed) allTopicsAccepted() bool {
	return f.orderbook.snapshotAccepted && f.trade.snapshotAccepted && f.stats.snapshotAccepted
}

func (f *feed) handleMessage(raw []byte, receivedMs int64) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var fr frame
	if err := dec.Decode(&fr); err != nil {
		return fmt.Errorf("crossverse %s: invalid frame: %w", f.symbol, err)
	}
	if fr.Event != "" {
		return f.handleControl(fr)
	}
	if fr.Topic == "" {
		return fmt.Errorf("crossverse %s: frame has neither event nor topic", f.symbol)
	}
	return f.handleData(fr, receivedMs)
}

func (f *feed) handleControl(fr frame) error {
	switch fr.Event {
	case "welcome", "subscribed":
		if fr.SnapshotID == "" {
			return nil
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.snapshotID != "" && f.snapshotID != fr.SnapshotID {
			f.snapshotID = fr.SnapshotID
			f.resetTopics()
			f.phase = HealthAwaitingSnapshot
			return nil
		}
		f.snapshotID = fr.SnapshotID
		return nil
	case "unsubscribed":
		return fmt.Errorf("crossverse %s: unexpected unsubscribe for %q", f.symbol, fr.Topic)
	case "pong":
		return nil
	case "error":
		return fmt.Errorf("crossverse %s: server error %q", f.symbol, fr.Message)
	default:
		return fmt.Errorf("crossverse %s: unknown control event %q", f.symbol, fr.Event)
	}
}

func (f *feed) handleData(fr frame, receivedMs int64) error {
	prefix := strings.ToUpper(f.symbol) + "@"
	if !strings.HasPrefix(fr.Topic, prefix) {
		return fmt.Errorf("crossverse %s: frame topic %q is for another symbol", f.symbol, fr.Topic)
	}
	kind := strings.TrimPrefix(fr.Topic, prefix)
	seq, err := ParseScaled(fr.Seq.String(), 1, RoundExact)
	if err != nil || seq < 0 {
		return fmt.Errorf("crossverse %s: frame seq %q is invalid", f.symbol, fr.Seq.String())
	}
	sourceTs, err := ParseTimestampMs(fr.TS.String())
	if err != nil {
		return fmt.Errorf("crossverse %s: frame ts %q is invalid", f.symbol, fr.TS.String())
	}
	if fr.SnapshotID == "" {
		return fmt.Errorf("crossverse %s: frame on %q has no snapshotId", f.symbol, fr.Topic)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.snapshotID == "" {
		f.snapshotID = fr.SnapshotID
	} else if f.snapshotID != fr.SnapshotID {
		f.snapshotID = fr.SnapshotID
		f.resetTopics()
		f.phase = HealthAwaitingSnapshot
		if !fr.Snapshot {
			return errResubscribe
		}
	}

	var topic *topicState
	switch kind {
	case "perp_orderbook":
		topic = &f.orderbook
	case "perp_trade":
		topic = &f.trade
	case "perp_stats":
		topic = &f.stats
	default:
		return fmt.Errorf("crossverse %s: unexpected topic %q", f.symbol, fr.Topic)
	}

	if fr.Snapshot {
		if err := f.applyPayload(kind, fr, sourceTs, receivedMs, true); err != nil {
			return err
		}
		topic.lastSeq = seq
		topic.snapshotAccepted = true
		if f.phase == HealthAwaitingSnapshot && f.allTopicsAccepted() {
			f.phase = HealthHealthy
		}
		return nil
	}

	if !topic.snapshotAccepted {
		return nil
	}
	if seq <= topic.lastSeq {
		return nil
	}
	if seq > topic.lastSeq+1 {
		f.resetTopics()
		f.phase = HealthStaleGap
		return errResubscribe
	}
	if err := f.applyPayload(kind, fr, sourceTs, receivedMs, false); err != nil {
		return err
	}
	topic.lastSeq = seq
	return nil
}

func (f *feed) applyPayload(kind string, fr frame, sourceTs, receivedMs int64, snapshot bool) error {
	switch kind {
	case "perp_orderbook":
		return f.applyOrderbook(fr.Data, sourceTs, receivedMs)
	case "perp_trade":
		if snapshot {
			return f.applyTradeSnapshot(fr.Data)
		}
		return f.applyTrade(fr.Data)
	case "perp_stats":
		return f.applyStats(fr.Data, sourceTs, receivedMs)
	default:
		return fmt.Errorf("crossverse %s: unexpected payload kind %q", f.symbol, kind)
	}
}

type orderbookPayload struct {
	Symbol     string           `json:"symbol"`
	MarkPrice  json.Number      `json:"mark_price"`
	IndexPrice json.Number      `json:"index_price"`
	BasisBps   json.Number      `json:"basis_bps"`
	Asks       [][2]json.Number `json:"asks"`
	Bids       [][2]json.Number `json:"bids"`
}

func (f *feed) applyOrderbook(data []byte, sourceTs, receivedMs int64) error {
	var p orderbookPayload
	if err := decodeNumbers(data, &p); err != nil {
		return fmt.Errorf("crossverse %s: invalid orderbook payload: %w", f.symbol, err)
	}
	mark, err := ParsePriceCents(p.MarkPrice.String())
	if err != nil {
		return fmt.Errorf("crossverse %s: mark price: %w", f.symbol, err)
	}
	index, err := ParsePriceCents(p.IndexPrice.String())
	if err != nil {
		return fmt.Errorf("crossverse %s: index price: %w", f.symbol, err)
	}
	basis, err := ParseBpsToPpb(p.BasisBps.String())
	if err != nil {
		return fmt.Errorf("crossverse %s: basis bps: %w", f.symbol, err)
	}
	asks, err := parseLevels(p.Asks, true)
	if err != nil {
		return fmt.Errorf("crossverse %s: asks: %w", f.symbol, err)
	}
	bids, err := parseLevels(p.Bids, false)
	if err != nil {
		return fmt.Errorf("crossverse %s: bids: %w", f.symbol, err)
	}
	f.markPriceCents = mark
	f.indexPriceCents = index
	f.basisPpb = basis
	f.asks = asks
	f.bids = bids
	f.bookSourceTsMs = sourceTs
	f.bookReceivedTsMs = receivedMs
	return nil
}

func parseLevels(raw [][2]json.Number, ascending bool) ([]Level, error) {
	out := make([]Level, 0, len(raw))
	var prev int64
	for i, lv := range raw {
		price, err := ParseExactPriceCents(lv[0].String())
		if err != nil {
			return nil, err
		}
		contracts, err := ParseContracts(lv[1].String())
		if err != nil {
			return nil, err
		}
		if i > 0 {
			if ascending && price <= prev {
				return nil, fmt.Errorf("crossverse book levels are not ascending at index %d", i)
			}
			if !ascending && price >= prev {
				return nil, fmt.Errorf("crossverse book levels are not descending at index %d", i)
			}
		}
		prev = price
		out = append(out, Level{PriceCents: price, Contracts: contracts})
	}
	return out, nil
}

type tradePayload struct {
	Side        string      `json:"side"`
	Price       json.Number `json:"price"`
	Contracts   json.Number `json:"contracts"`
	Notional    json.Number `json:"notional"`
	Liquidation bool        `json:"liquidation"`
	TradeTime   json.Number `json:"trade_time"`
}

type tradeSnapshotPayload struct {
	Symbol string         `json:"symbol"`
	Trades []tradePayload `json:"trades"`
}

func (f *feed) applyTradeSnapshot(data []byte) error {
	var p tradeSnapshotPayload
	if err := decodeNumbers(data, &p); err != nil {
		return fmt.Errorf("crossverse %s: invalid trade snapshot payload: %w", f.symbol, err)
	}
	trades := make([]Trade, 0, len(p.Trades))
	for i := len(p.Trades) - 1; i >= 0; i-- {
		t, err := parseTrade(p.Trades[i])
		if err != nil {
			return fmt.Errorf("crossverse %s: trade snapshot: %w", f.symbol, err)
		}
		trades = append(trades, t)
	}
	if len(trades) > recentTradeCap {
		trades = trades[len(trades)-recentTradeCap:]
	}
	f.recentTrades = trades
	return nil
}

func (f *feed) applyTrade(data []byte) error {
	var p tradePayload
	if err := decodeNumbers(data, &p); err != nil {
		return fmt.Errorf("crossverse %s: invalid trade payload: %w", f.symbol, err)
	}
	t, err := parseTrade(p)
	if err != nil {
		return fmt.Errorf("crossverse %s: trade: %w", f.symbol, err)
	}
	f.recentTrades = append(f.recentTrades, t)
	if len(f.recentTrades) > recentTradeCap {
		f.recentTrades = f.recentTrades[len(f.recentTrades)-recentTradeCap:]
	}
	return nil
}

func parseTrade(p tradePayload) (Trade, error) {
	side := strings.ToUpper(strings.TrimSpace(p.Side))
	if side != "BUY" && side != "SELL" {
		return Trade{}, fmt.Errorf("trade side %q is invalid", p.Side)
	}
	price, err := ParsePriceCents(p.Price.String())
	if err != nil {
		return Trade{}, err
	}
	contracts, err := ParseContracts(p.Contracts.String())
	if err != nil {
		return Trade{}, err
	}
	notional, err := ParseMicroUSDX(p.Notional.String())
	if err != nil {
		return Trade{}, err
	}
	tradeTime, err := ParseTimestampMs(p.TradeTime.String())
	if err != nil {
		return Trade{}, err
	}
	return Trade{
		Side:              side,
		PriceCents:        price,
		Contracts:         contracts,
		NotionalMicroUSDX: notional,
		Liquidation:       p.Liquidation,
		TradeTimeMs:       tradeTime,
	}, nil
}

// statsPayload carries the authoritative perp_stats field set; the legacy*
// fields absorb the currently deployed services, which still emit the
// pre-protocol names. Authoritative names always win when both are present.
type statsPayload struct {
	Symbol            string      `json:"symbol"`
	Volume24h         json.Number `json:"volume_24h"`
	OpenInterest      json.Number `json:"open_interest"`
	EstFundingRate    json.Number `json:"est_funding_rate"`
	LastFundingRate   json.Number `json:"last_funding_rate"`
	LongShortRatio    json.Number `json:"long_short_ratio"`
	LiqVolume24h      json.Number `json:"liq_volume_24h"`
	LegacyFundingRate json.Number `json:"funding_rate"`
	LegacyOIUSD       json.Number `json:"oi_usd"`
	LegacyVolume24h   json.Number `json:"volume_24h_usd"`
}

func firstNumber(primary, fallback json.Number) json.Number {
	if primary.String() != "" {
		return primary
	}
	return fallback
}

func (f *feed) applyStats(data []byte, sourceTs, receivedMs int64) error {
	var p statsPayload
	if err := decodeNumbers(data, &p); err != nil {
		return fmt.Errorf("crossverse %s: invalid stats payload: %w", f.symbol, err)
	}
	est := firstNumber(p.EstFundingRate, p.LegacyFundingRate)
	if est.String() == "" {
		return fmt.Errorf("crossverse %s: stats payload has no est_funding_rate", f.symbol)
	}
	estPpb, err := ParseSignedPpb(est.String())
	if err != nil {
		return fmt.Errorf("crossverse %s: est funding rate: %w", f.symbol, err)
	}
	lastPpb := f.lastFundingPpb
	if p.LastFundingRate.String() != "" {
		lastPpb, err = ParseSignedPpb(p.LastFundingRate.String())
		if err != nil {
			return fmt.Errorf("crossverse %s: last funding rate: %w", f.symbol, err)
		}
	}
	oiUSD := f.externalOpenInterestMicroUSDX
	if raw := firstNumber(p.OpenInterest, p.LegacyOIUSD); raw.String() != "" {
		oiUSD, err = ParseMicroUSDX(raw.String())
		if err != nil {
			return fmt.Errorf("crossverse %s: open interest: %w", f.symbol, err)
		}
	}
	volume := f.volume24hMicroUSDX
	if raw := firstNumber(p.Volume24h, p.LegacyVolume24h); raw.String() != "" {
		volume, err = ParseMicroUSDX(raw.String())
		if err != nil {
			return fmt.Errorf("crossverse %s: volume 24h: %w", f.symbol, err)
		}
	}
	longShort := f.longShortRatioPpb
	if p.LongShortRatio.String() != "" {
		longShort, err = ParseSignedPpb(p.LongShortRatio.String())
		if err != nil {
			return fmt.Errorf("crossverse %s: long short ratio: %w", f.symbol, err)
		}
	}
	liqVolume := f.liquidationVolumeMicroUSDX
	if p.LiqVolume24h.String() != "" {
		liqVolume, err = ParseMicroUSDX(p.LiqVolume24h.String())
		if err != nil {
			return fmt.Errorf("crossverse %s: liquidation volume: %w", f.symbol, err)
		}
	}
	f.estimatedFundingPpb = estPpb
	f.lastFundingPpb = lastPpb
	f.externalOpenInterestMicroUSDX = oiUSD
	f.volume24hMicroUSDX = volume
	f.longShortRatioPpb = longShort
	f.liquidationVolumeMicroUSDX = liqVolume
	f.statsReceivedTsMs = receivedMs
	return nil
}

func (f *feed) applyRestStats(body []byte, receivedMs int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyStats(body, receivedMs, receivedMs)
}

func decodeNumbers(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(out)
}

func (f *feed) Snapshot(nowMs int64) NormalizedSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	bids := make([]Level, len(f.bids))
	copy(bids, f.bids)
	asks := make([]Level, len(f.asks))
	copy(asks, f.asks)
	var bestBid, bestAsk int64
	if len(bids) > 0 {
		bestBid = bids[0].PriceCents
	}
	if len(asks) > 0 {
		bestAsk = asks[0].PriceCents
	}
	return NormalizedSnapshot{
		Symbol:                        f.symbol,
		SnapshotID:                    f.snapshotID,
		OrderbookSeq:                  f.orderbook.lastSeq,
		TradeSeq:                      f.trade.lastSeq,
		StatsSeq:                      f.stats.lastSeq,
		SourceTimestampMs:             f.bookSourceTsMs,
		ReceivedTimestampMs:           f.bookReceivedTsMs,
		MarkPriceCents:                f.markPriceCents,
		IndexPriceCents:               f.indexPriceCents,
		BasisPpb:                      f.basisPpb,
		BestBidCents:                  bestBid,
		BestAskCents:                  bestAsk,
		Bids:                          bids,
		Asks:                          asks,
		EstimatedFundingPpb:           f.estimatedFundingPpb,
		LastFundingPpb:                f.lastFundingPpb,
		ExternalOpenInterestMicroUSDX: f.externalOpenInterestMicroUSDX,
		LongShortRatioPpb:             f.longShortRatioPpb,
		LiquidationVolumeMicroUSDX:    f.liquidationVolumeMicroUSDX,
		Volume24hMicroUSDX:            f.volume24hMicroUSDX,
		Health:                        f.healthLocked(nowMs),
	}
}

func (f *feed) Ref() SnapshotRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return SnapshotRef{
		SnapshotID:        f.snapshotID,
		OrderbookSeq:      f.orderbook.lastSeq,
		StatsSeq:          f.stats.lastSeq,
		SourceTimestampMs: f.bookSourceTsMs,
	}
}

func (f *feed) Health(nowMs int64) Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthLocked(nowMs)
}
