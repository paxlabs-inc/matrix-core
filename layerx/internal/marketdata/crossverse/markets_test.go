package crossverse

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func marketsManager(t *testing.T, nowMs int64) *Manager {
	t.Helper()
	m, err := New(Config{
		BaseURL: "https://example.invalid",
		Symbols: []string{"BTC"},
		Now:     func() time.Time { return time.UnixMilli(nowMs) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func marketRecordData(symbol string) map[string]any {
	return map[string]any{
		"symbol":                    symbol,
		"symbol_full":               "Bitcoin",
		"price":                     json.RawMessage("77173.0"),
		"change_24h":                json.RawMessage("0.0123"),
		"volume_24h":                json.RawMessage("1234567.8"),
		"open_interest":             json.RawMessage("987654.3"),
		"funding_8h":                json.RawMessage("0.00009"),
		"est_funding_rate":          json.RawMessage("0.00012"),
		"perp_mark_price":           json.RawMessage("77182.4"),
		"perp_index_price":          json.RawMessage("77173.0"),
		"perp_basis_bps":            json.RawMessage("1.21"),
		"perp_funding_rate":         json.RawMessage("0.00009"),
		"perp_next_funding_ms":      json.RawMessage("1747555200000"),
		"perp_long_short_ratio":     json.RawMessage("1.03"),
		"perp_liq_volume_24h":       json.RawMessage("4567.89"),
		"perp_oi_contracts":         json.RawMessage("98765"),
		"perp_oi_usd":               json.RawMessage("987650.5"),
		"perp_volume_24h_contracts": json.RawMessage("1234567"),
		"perp_volume_24h_usd":       json.RawMessage("12345678.912345678"),
	}
}

func marketsFrame(t *testing.T, topic, snapshotID string, seq int64, snapshot bool, data any) []byte {
	t.Helper()
	return marshalFrame(t, topic, snapshotID, seq, snapshot, testBaseTs, data)
}

func TestMarketsAggregateLane(t *testing.T) {
	m := marketsManager(t, testBaseTs)
	list := map[string]any{"ts": testBaseTs, "markets": []any{marketRecordData("BTC")}}

	if err := m.handleAggregateMessage(marketsFrame(t, "markets@all", "agg-1", 7, true, list)); err != nil {
		t.Fatal(err)
	}
	if !m.AggregateFresh(testBaseTs) {
		t.Fatal("aggregate must be fresh after markets snapshot")
	}
	rec, ok := m.MarketsRecord("BTC")
	if !ok {
		t.Fatal("BTC markets record missing")
	}
	if rec.PriceCents != 7717300 || rec.PerpMarkPriceCents != 7718240 || rec.PerpIndexPriceCents != 7717300 {
		t.Fatalf("prices = %d %d %d", rec.PriceCents, rec.PerpMarkPriceCents, rec.PerpIndexPriceCents)
	}
	if rec.PerpNextFundingAtMs != 1_747_555_200_000 {
		t.Fatalf("next funding = %d", rec.PerpNextFundingAtMs)
	}
	if rec.PerpFundingPpb != 90_000 || rec.EstimatedFundingPpb != 120_000 {
		t.Fatalf("funding = %d %d", rec.PerpFundingPpb, rec.EstimatedFundingPpb)
	}
	if rec.PerpOIContracts != 98_765 || rec.PerpOIMicroUSDX != 987_650_500_000 {
		t.Fatalf("oi = %d %d", rec.PerpOIContracts, rec.PerpOIMicroUSDX)
	}
	if rec.PerpVolume24hContracts != 1_234_567 || rec.PerpVolume24hMicroUSDX != 12_345_678_912_345 {
		t.Fatalf("volume = %d %d", rec.PerpVolume24hContracts, rec.PerpVolume24hMicroUSDX)
	}
	if rec.PerpBasisPpb != 121_000 || rec.PerpLongShortRatioPpb != 1_030_000_000 {
		t.Fatalf("basis/ratio = %d %d", rec.PerpBasisPpb, rec.PerpLongShortRatioPpb)
	}

	if err := m.handleAggregateMessage(marketsFrame(t, "markets@all", "agg-1", 8, false, list)); err != nil {
		t.Fatal(err)
	}
	err := m.handleAggregateMessage(marketsFrame(t, "markets@all", "agg-1", 10, false, list))
	if !errors.Is(err, errResubscribe) {
		t.Fatalf("gap error = %v, want errResubscribe", err)
	}
	if err := m.handleAggregateMessage(marketsFrame(t, "markets@all", "agg-1", 11, false, list)); err != nil {
		t.Fatal(err)
	}
	m.markets.mu.Lock()
	accepted := m.markets.all.snapshotAccepted
	m.markets.mu.Unlock()
	if accepted {
		t.Fatal("post-gap delta must be discarded until a fresh snapshot")
	}

	err = m.handleAggregateMessage(marketsFrame(t, "markets@all", "agg-2", 3, false, list))
	if !errors.Is(err, errResubscribe) {
		t.Fatalf("restart error = %v, want errResubscribe", err)
	}
	if err := m.handleAggregateMessage(marketsFrame(t, "markets@all", "agg-2", 3, true, list)); err != nil {
		t.Fatal(err)
	}
	if err := m.handleAggregateMessage(marketsFrame(t, "markets@totals", "agg-2", 1, true, map[string]any{"ts": testBaseTs, "totals": map[string]any{"total_trades": 1}})); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.MarketsRecord("DOGE"); ok {
		t.Fatal("unknown symbol must have no record")
	}
}

func TestMarketsLiveDeployedShapes(t *testing.T) {
	m := marketsManager(t, testBaseTs)
	rel := marketRecordData("BTC")
	rel["perp_next_funding_ms"] = json.RawMessage("27700000")
	if err := m.handleAggregateMessage(marketsFrame(t, "markets@all", "agg-1", 1, true, []any{rel})); err != nil {
		t.Fatalf("bare-array markets@all data must be accepted: %v", err)
	}
	rec, ok := m.MarketsRecord("BTC")
	if !ok {
		t.Fatal("BTC markets record missing")
	}
	if rec.PerpNextFundingAtMs != testBaseTs+27_700_000 {
		t.Fatalf("relative next funding = %d, want %d", rec.PerpNextFundingAtMs, testBaseTs+27_700_000)
	}
}

func TestStatsLegacyDeployedFieldNames(t *testing.T) {
	f := newFeed("BTC", 75)
	legacy := []byte(`{"symbol":"BTC","mark_price":64703.93742670378,"index_price":64702.3,` +
		`"basis_bps":0.2531,"funding_rate":0.00075,"last_funding_rate":-0.00011105307866656878,` +
		`"next_funding_ms":16671289,"long_short_ratio":1,"liq_volume_24h":901710,` +
		`"oi_contracts":90221,"oi_usd":902210,"volume_24h_contracts":90171,"volume_24h_usd":901710}`)
	if err := f.applyRestStats(legacy, testBaseTs); err != nil {
		t.Fatalf("legacy live stats shape must parse: %v", err)
	}
	s := f.Snapshot(testBaseTs)
	if s.EstimatedFundingPpb != 750_000 {
		t.Fatalf("est funding = %d", s.EstimatedFundingPpb)
	}
	if s.LastFundingPpb != -111_053 {
		t.Fatalf("last funding = %d", s.LastFundingPpb)
	}
	if s.ExternalOpenInterestMicroUSDX != 902_210_000_000 || s.Volume24hMicroUSDX != 901_710_000_000 {
		t.Fatalf("oi/volume = %d %d", s.ExternalOpenInterestMicroUSDX, s.Volume24hMicroUSDX)
	}
}

func TestURLSchemeConfiguration(t *testing.T) {
	m := marketsManager(t, testBaseTs)
	if got := m.symbolRESTBase("BTC"); got != "https://example.invalid/api/btc" {
		t.Fatalf("symbol rest = %q", got)
	}
	if got := m.symbolWSBase("BTC"); got != "https://example.invalid/ws/btc" {
		t.Fatalf("symbol ws = %q", got)
	}
	if got := m.marketsRESTBase(); got != "https://example.invalid/api/markets" {
		t.Fatalf("markets rest = %q", got)
	}
	if got := m.marketsWSBase(); got != "https://example.invalid/ws/markets" {
		t.Fatalf("markets ws = %q", got)
	}

	direct, err := New(Config{
		SymbolRESTBase:  "http://{symbol}.svc.local:3001",
		SymbolWSBase:    "ws://{SYMBOL}.svc.local:3001",
		MarketsRESTBase: "http://markets.svc.local:3020",
		MarketsWSBase:   "ws://markets.svc.local:3020",
		Symbols:         []string{"BTC"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := direct.symbolRESTBase("BTC"); got != "http://btc.svc.local:3001" {
		t.Fatalf("direct symbol rest = %q", got)
	}
	if got := direct.symbolWSBase("BTC"); got != "ws://BTC.svc.local:3001" {
		t.Fatalf("direct symbol ws = %q", got)
	}
	if got := direct.marketsWSBase(); got != "ws://markets.svc.local:3020" {
		t.Fatalf("direct markets ws = %q", got)
	}
	if _, err := New(Config{Symbols: []string{"BTC"}}); err == nil {
		t.Fatal("missing base url without full overrides must be rejected")
	}
	ws, err := toWSURL("https://example.invalid/ws/btc")
	if err != nil || ws != "wss://example.invalid/ws/btc" {
		t.Fatalf("toWSURL = %q %v", ws, err)
	}
}
