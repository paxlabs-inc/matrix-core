package crossverse

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const testBaseTs = int64(1_752_950_000_000)

func marshalFrame(t *testing.T, topic, snapshotID string, seq int64, snapshot bool, ts int64, data any) []byte {
	t.Helper()
	env := map[string]any{
		"topic":      topic,
		"ts":         ts,
		"snapshotId": snapshotID,
		"seq":        seq,
		"data":       data,
	}
	if snapshot {
		env["snapshot"] = true
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func orderbookData(mark, index, basis string, asks, bids [][]json.RawMessage) map[string]any {
	return map[string]any{
		"symbol":      "BTC",
		"mark_price":  json.RawMessage(mark),
		"index_price": json.RawMessage(index),
		"basis_bps":   json.RawMessage(basis),
		"asks":        asks,
		"bids":        bids,
	}
}

func defaultBook() map[string]any {
	return orderbookData("77182.436", "77173.0", "1.21",
		[][]json.RawMessage{
			{json.RawMessage("77185"), json.RawMessage("25")},
			{json.RawMessage("77190.50"), json.RawMessage("130")},
		},
		[][]json.RawMessage{
			{json.RawMessage("77180"), json.RawMessage("40")},
			{json.RawMessage("77175.25"), json.RawMessage("90")},
		})
}

func statsData() map[string]any {
	return map[string]any{
		"symbol":            "BTC",
		"volume_24h":        json.RawMessage("12345678.912345678"),
		"open_interest":     json.RawMessage("987650.5"),
		"est_funding_rate":  json.RawMessage("0.00012"),
		"last_funding_rate": json.RawMessage("0.00009"),
		"mark_price":        json.RawMessage("77182.4"),
		"index_price":       json.RawMessage("77173.0"),
		"basis_bps":         json.RawMessage("1.21"),
		"long_short_ratio":  json.RawMessage("1.03"),
		"liq_volume_24h":    json.RawMessage("4567.89"),
	}
}

func tradeData(price string, contracts, tradeTime int64) map[string]any {
	return map[string]any{
		"symbol":      "BTC",
		"side":        "BUY",
		"price":       json.RawMessage(price),
		"contracts":   contracts,
		"notional":    json.RawMessage("250"),
		"liquidation": false,
		"trade_time":  tradeTime,
	}
}

func mustHandle(t *testing.T, f *feed, raw []byte, receivedMs int64) {
	t.Helper()
	if err := f.handleMessage(raw, receivedMs); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
}

func bootFeed(t *testing.T, snapshotID string, nowMs int64) *feed {
	t.Helper()
	f := newFeed("BTC", 75)
	f.setPhase(HealthConnecting)
	mustHandle(t, f, []byte(`{"event":"welcome","ts":1,"snapshotId":"`+snapshotID+`"}`), nowMs)
	f.setPhase(HealthAwaitingSnapshot)
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", snapshotID, 100, true, testBaseTs, defaultBook()), nowMs)
	trades := map[string]any{"symbol": "BTC", "trades": []any{
		tradeData("77182.4", 25, testBaseTs-1000),
		tradeData("77181.9", 10, testBaseTs-2000),
	}}
	mustHandle(t, f, marshalFrame(t, "BTC@perp_trade", snapshotID, 50, true, testBaseTs, trades), nowMs)
	mustHandle(t, f, marshalFrame(t, "BTC@perp_stats", snapshotID, 10, true, testBaseTs, statsData()), nowMs)
	return f
}

func TestFeedReachesHealthyAndNormalizes(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)
	if got := f.Health(now); got != HealthHealthy {
		t.Fatalf("health = %s, want HEALTHY", got)
	}
	s := f.Snapshot(now)
	if s.SnapshotID != "proc-1" || s.OrderbookSeq != 100 || s.TradeSeq != 50 || s.StatsSeq != 10 {
		t.Fatalf("snapshot identity = %q %d %d %d", s.SnapshotID, s.OrderbookSeq, s.TradeSeq, s.StatsSeq)
	}
	if s.MarkPriceCents != 7718244 || s.IndexPriceCents != 7717300 || s.BasisPpb != 121_000 {
		t.Fatalf("refs = %d %d %d", s.MarkPriceCents, s.IndexPriceCents, s.BasisPpb)
	}
	if s.BestAskCents != 7718500 || s.BestBidCents != 7718000 {
		t.Fatalf("best = bid %d ask %d", s.BestBidCents, s.BestAskCents)
	}
	if len(s.Asks) != 2 || s.Asks[1].PriceCents != 7719050 || s.Asks[1].Contracts != 130 {
		t.Fatalf("asks = %+v", s.Asks)
	}
	if len(s.Bids) != 2 || s.Bids[1].PriceCents != 7717525 || s.Bids[1].Contracts != 90 {
		t.Fatalf("bids = %+v", s.Bids)
	}
	if s.EstimatedFundingPpb != 120_000 || s.LastFundingPpb != 90_000 {
		t.Fatalf("funding = %d %d", s.EstimatedFundingPpb, s.LastFundingPpb)
	}
	if s.ExternalOpenInterestMicroUSDX != 987_650_500_000 {
		t.Fatalf("oi = %d", s.ExternalOpenInterestMicroUSDX)
	}
	if s.Volume24hMicroUSDX != 12_345_678_912_345 || s.LiquidationVolumeMicroUSDX != 4_567_890_000 {
		t.Fatalf("volume = %d %d", s.Volume24hMicroUSDX, s.LiquidationVolumeMicroUSDX)
	}
	if s.LongShortRatioPpb != 1_030_000_000 {
		t.Fatalf("long short = %d", s.LongShortRatioPpb)
	}
	if s.SourceTimestampMs != testBaseTs || s.ReceivedTimestampMs != now {
		t.Fatalf("timestamps = %d %d", s.SourceTimestampMs, s.ReceivedTimestampMs)
	}
	f.mu.Lock()
	trades := append([]Trade(nil), f.recentTrades...)
	f.mu.Unlock()
	if len(trades) != 2 || trades[0].TradeTimeMs != testBaseTs-2000 || trades[1].TradeTimeMs != testBaseTs-1000 {
		t.Fatalf("trades = %+v", trades)
	}
	if trades[1].PriceCents != 7718240 || trades[1].Contracts != 25 || trades[1].NotionalMicroUSDX != 250_000_000 {
		t.Fatalf("trade fields = %+v", trades[1])
	}
}

func TestDeltaSequenceAndDiscards(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)

	stale := orderbookData("90000.0", "77173.0", "1.21",
		[][]json.RawMessage{{json.RawMessage("77185"), json.RawMessage("1")}},
		[][]json.RawMessage{{json.RawMessage("77180"), json.RawMessage("1")}})
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 100, false, testBaseTs+10, stale), now+10)
	if s := f.Snapshot(now); s.MarkPriceCents != 7718244 {
		t.Fatalf("old-seq delta must be discarded, mark = %d", s.MarkPriceCents)
	}

	next := orderbookData("77183.0", "77173.5", "1.23",
		[][]json.RawMessage{{json.RawMessage("77186"), json.RawMessage("30")}},
		[][]json.RawMessage{{json.RawMessage("77181"), json.RawMessage("45")}})
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 101, false, testBaseTs+20, next), now+20)
	s := f.Snapshot(now + 20)
	if s.OrderbookSeq != 101 || s.MarkPriceCents != 7718300 || s.BestAskCents != 7718600 {
		t.Fatalf("dense delta not applied: %+v", s)
	}

	fresh := newFeed("BTC", 75)
	fresh.setPhase(HealthAwaitingSnapshot)
	mustHandle(t, fresh, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 5, false, testBaseTs, defaultBook()), now)
	if s := fresh.Snapshot(now); s.OrderbookSeq != 0 || len(s.Asks) != 0 {
		t.Fatalf("pre-snapshot delta must be discarded: %+v", s)
	}
	if got := fresh.Health(now); got != HealthAwaitingSnapshot {
		t.Fatalf("health = %s, want AWAITING_SNAPSHOT", got)
	}
}

func TestGapMarksStaleUntilResubscribeSnapshot(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)

	gap := marshalFrame(t, "BTC@perp_orderbook", "proc-1", 102, false, testBaseTs+10, defaultBook())
	err := f.handleMessage(gap, now+10)
	if !errors.Is(err, errResubscribe) {
		t.Fatalf("gap error = %v, want errResubscribe", err)
	}
	if got := f.Health(now + 10); got != HealthStaleGap {
		t.Fatalf("health = %s, want STALE_GAP", got)
	}

	delta := marshalFrame(t, "BTC@perp_stats", "proc-1", 11, false, testBaseTs+20, statsData())
	mustHandle(t, f, delta, now+20)
	if got := f.Health(now + 20); got != HealthStaleGap {
		t.Fatalf("health after post-gap delta = %s, want STALE_GAP", got)
	}

	f.disconnected()
	if got := f.Health(now + 30); got != HealthConnecting {
		t.Fatalf("health = %s, want CONNECTING", got)
	}
	f.setPhase(HealthAwaitingSnapshot)
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 200, true, testBaseTs+40, defaultBook()), now+40)
	if got := f.Health(now + 40); got != HealthAwaitingSnapshot {
		t.Fatalf("health = %s, want AWAITING_SNAPSHOT until all topics", got)
	}
	trades := map[string]any{"symbol": "BTC", "trades": []any{}}
	mustHandle(t, f, marshalFrame(t, "BTC@perp_trade", "proc-1", 90, true, testBaseTs+40, trades), now+40)
	mustHandle(t, f, marshalFrame(t, "BTC@perp_stats", "proc-1", 12, true, testBaseTs+40, statsData()), now+40)
	if got := f.Health(now + 40); got != HealthHealthy {
		t.Fatalf("health = %s, want HEALTHY after resubscribe snapshots", got)
	}
	if ref := f.Ref(); ref.OrderbookSeq != 200 {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestSnapshotIDChangeDiscardsAllTopicState(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)

	restarted := marshalFrame(t, "BTC@perp_orderbook", "proc-2", 3, false, testBaseTs+10, defaultBook())
	err := f.handleMessage(restarted, now+10)
	if !errors.Is(err, errResubscribe) {
		t.Fatalf("restart error = %v, want errResubscribe", err)
	}
	if got := f.Health(now + 10); got != HealthAwaitingSnapshot {
		t.Fatalf("health = %s, want AWAITING_SNAPSHOT", got)
	}
	ref := f.Ref()
	if ref.SnapshotID != "proc-2" || ref.OrderbookSeq != 0 || ref.StatsSeq != 0 {
		t.Fatalf("ref after restart = %+v", ref)
	}

	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-2", 3, true, testBaseTs+20, defaultBook()), now+20)
	trades := map[string]any{"symbol": "BTC", "trades": []any{}}
	mustHandle(t, f, marshalFrame(t, "BTC@perp_trade", "proc-2", 1, true, testBaseTs+20, trades), now+20)
	mustHandle(t, f, marshalFrame(t, "BTC@perp_stats", "proc-2", 1, true, testBaseTs+20, statsData()), now+20)
	if got := f.Health(now + 20); got != HealthHealthy {
		t.Fatalf("health = %s, want HEALTHY after restart snapshots", got)
	}
}

func TestWelcomeSnapshotIDChangeResets(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)
	mustHandle(t, f, []byte(`{"event":"welcome","ts":2,"snapshotId":"proc-2"}`), now+10)
	if got := f.Health(now + 10); got != HealthAwaitingSnapshot {
		t.Fatalf("health = %s, want AWAITING_SNAPSHOT", got)
	}
	if ref := f.Ref(); ref.SnapshotID != "proc-2" || ref.OrderbookSeq != 0 {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestFreshnessDegradesToStaleTime(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)
	if got := f.Health(now + bookFreshMs); got != HealthHealthy {
		t.Fatalf("health at book boundary = %s, want HEALTHY", got)
	}
	if got := f.Health(now + bookFreshMs + 1); got != HealthStaleTime {
		t.Fatalf("health past book boundary = %s, want STALE_TIME", got)
	}

	bookAt := now + 44_500
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 101, false, bookAt, defaultBook()), bookAt)
	if got := f.Health(now + statsFreshMs); got != HealthHealthy {
		t.Fatalf("health at stats boundary = %s, want HEALTHY", got)
	}
	if got := f.Health(now + statsFreshMs + 1); got != HealthStaleTime {
		t.Fatalf("health past stats boundary = %s, want STALE_TIME", got)
	}
}

func TestDivergenceBoundary(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)

	atLimit := orderbookData("100.75", "100.00", "0",
		[][]json.RawMessage{{json.RawMessage("100.80"), json.RawMessage("10")}},
		[][]json.RawMessage{{json.RawMessage("100.70"), json.RawMessage("10")}})
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 101, false, testBaseTs+10, atLimit), now+10)
	if got := f.Health(now + 10); got != HealthHealthy {
		t.Fatalf("health at divergence limit = %s, want HEALTHY", got)
	}

	overLimit := orderbookData("100.76", "100.00", "0",
		[][]json.RawMessage{{json.RawMessage("100.80"), json.RawMessage("10")}},
		[][]json.RawMessage{{json.RawMessage("100.70"), json.RawMessage("10")}})
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 102, false, testBaseTs+20, overLimit), now+20)
	if got := f.Health(now + 20); got != HealthStaleDivergence {
		t.Fatalf("health past divergence limit = %s, want STALE_DIVERGENCE", got)
	}
}

func TestBookValidationFailsClosed(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)

	unordered := orderbookData("77182.4", "77173.0", "1.21",
		[][]json.RawMessage{
			{json.RawMessage("77190"), json.RawMessage("10")},
			{json.RawMessage("77185"), json.RawMessage("10")},
		},
		[][]json.RawMessage{{json.RawMessage("77180"), json.RawMessage("10")}})
	if err := f.handleMessage(marshalFrame(t, "BTC@perp_orderbook", "proc-1", 101, false, testBaseTs+10, unordered), now+10); err == nil {
		t.Fatal("unordered asks must fail closed")
	}

	subCent := orderbookData("77182.4", "77173.0", "1.21",
		[][]json.RawMessage{{json.RawMessage("77185.123"), json.RawMessage("10")}},
		[][]json.RawMessage{{json.RawMessage("77180"), json.RawMessage("10")}})
	if err := f.handleMessage(marshalFrame(t, "BTC@perp_orderbook", "proc-1", 101, false, testBaseTs+10, subCent), now+10); err == nil {
		t.Fatal("sub-cent book price must fail closed")
	}

	fractional := orderbookData("77182.4", "77173.0", "1.21",
		[][]json.RawMessage{{json.RawMessage("77185"), json.RawMessage("10.5")}},
		[][]json.RawMessage{{json.RawMessage("77180"), json.RawMessage("10")}})
	if err := f.handleMessage(marshalFrame(t, "BTC@perp_orderbook", "proc-1", 101, false, testBaseTs+10, fractional), now+10); err == nil {
		t.Fatal("fractional contracts must fail closed")
	}
}

func TestRestStatsRecoveryRestoresStatsOnly(t *testing.T) {
	now := testBaseTs
	f := bootFeed(t, "proc-1", now)

	later := now + statsFreshMs + 1_000
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 101, false, later, defaultBook()), later)
	if got := f.Health(later); got != HealthStaleTime {
		t.Fatalf("health before recovery = %s, want STALE_TIME", got)
	}

	body, err := json.Marshal(statsData())
	if err != nil {
		t.Fatal(err)
	}
	refBefore := f.Ref()
	if err := f.applyRestStats(body, later); err != nil {
		t.Fatal(err)
	}
	if got := f.Health(later); got != HealthHealthy {
		t.Fatalf("health after recovery = %s, want HEALTHY", got)
	}
	refAfter := f.Ref()
	if refAfter != refBefore {
		t.Fatalf("rest recovery must not move refs: %+v vs %+v", refBefore, refAfter)
	}
}

func TestManagerRiskIncreaseAllowed(t *testing.T) {
	now := time.UnixMilli(testBaseTs)
	m, err := New(Config{
		BaseURL: "https://example.invalid",
		Symbols: []string{"BTC", "ETH"},
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f := m.feeds["BTC"]
	f.setPhase(HealthAwaitingSnapshot)
	mustHandle(t, f, marshalFrame(t, "BTC@perp_orderbook", "proc-1", 1, true, testBaseTs, defaultBook()), testBaseTs)
	trades := map[string]any{"symbol": "BTC", "trades": []any{}}
	mustHandle(t, f, marshalFrame(t, "BTC@perp_trade", "proc-1", 1, true, testBaseTs, trades), testBaseTs)
	mustHandle(t, f, marshalFrame(t, "BTC@perp_stats", "proc-1", 1, true, testBaseTs, statsData()), testBaseTs)

	allowed, err := m.RiskIncreaseAllowed("BTC")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("risk increase must fail closed without fresh aggregate")
	}

	m.aggregateReceivedMs.Store(testBaseTs - aggregateFreshMs)
	allowed, err = m.RiskIncreaseAllowed("BTC")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("risk increase must be allowed with healthy feed and fresh aggregate")
	}

	m.aggregateReceivedMs.Store(testBaseTs - aggregateFreshMs - 1)
	allowed, err = m.RiskIncreaseAllowed("BTC")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("risk increase must fail closed on stale aggregate")
	}

	m.aggregateReceivedMs.Store(testBaseTs)
	allowed, err = m.RiskIncreaseAllowed("ETH")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("risk increase must fail closed for a feed that never became healthy")
	}
	if _, err := m.RiskIncreaseAllowed("DOGE"); err == nil {
		t.Fatal("unknown symbol must error")
	}
}
