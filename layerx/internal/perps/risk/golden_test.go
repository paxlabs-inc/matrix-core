package risk

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
	"github.com/paxlabs-inc/layerx/internal/perps/pricing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden vector file")

// TestGoldenVectors computes the full deterministic calculation battery for
// every locked market — pricing (L2 walk, spread, quadratic skew, adverse tick
// rounding), margins/fees, unrealized and partial-close realized PnL on both
// sides, liquidation trigger prices, funding (skew, clamp, full and partial
// session), and the pool-capacity gate — and requires the output to be
// byte-identical to testdata/golden_vectors.txt. Regenerate deliberately with
// `go test -run TestGoldenVectors -update ./internal/perps/risk`.
func TestGoldenVectors(t *testing.T) {
	var b strings.Builder
	for idx, m := range market.All() {
		entry := int64(100_000 + 7_919*idx)
		writeMarketVectors(t, &b, m, entry)
	}
	got := b.String()

	path := filepath.Join("testdata", "golden_vectors.txt")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run with -update once to create): %v", err)
	}
	if got != string(want) {
		gotLines := strings.Split(got, "\n")
		wantLines := strings.Split(string(want), "\n")
		for i := range gotLines {
			if i >= len(wantLines) || gotLines[i] != wantLines[i] {
				t.Fatalf("golden mismatch at line %d:\n got: %s\nwant: %s", i+1,
					gotLines[i], safeLine(wantLines, i))
			}
		}
		t.Fatalf("golden file has %d lines, output has %d", len(wantLines), len(gotLines))
	}
}

func safeLine(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<missing>"
}

func writeMarketVectors(t *testing.T, b *strings.Builder, m market.Market, entry int64) {
	t.Helper()
	line := func(field string, v int64) {
		fmt.Fprintf(b, "%s %s=%d\n", m.Symbol, field, v)
	}
	must := func(v int64, err error) int64 {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", m.Symbol, err)
		}
		return v
	}

	const contracts = int64(12)
	notional := must(Notional(contracts))
	line("notional", notional)
	line("initial_margin", must(InitialMargin(notional, m.InitialMarginBps)))
	line("maintenance_margin", must(MaintenanceMargin(notional, m.MaintenanceMarginBps)))
	line("maker_fee", must(Fee(notional, m.MakerFeeBps)))
	line("taker_fee", must(Fee(notional, m.TakerFeeBps)))
	line("liquidation_fee", must(LiquidationFee(notional, m.LiquidationFeeBps)))

	asks := []crossverse.Level{
		{PriceCents: entry + 1, Contracts: 5},
		{PriceCents: entry + 3, Contracts: 10},
		{PriceCents: entry + 7, Contracts: 50},
	}
	bids := []crossverse.Level{
		{PriceCents: entry - 1, Contracts: 5},
		{PriceCents: entry - 3, Contracts: 10},
		{PriceCents: entry - 7, Contracts: 50},
	}
	buyVWAP := must(pricing.BookVWAP(asks, contracts, pricing.Buy))
	sellVWAP := must(pricing.BookVWAP(bids, contracts, pricing.Sell))
	line("buy_vwap", buyVWAP)
	line("sell_vwap", sellVWAP)

	projected := must(pricing.UtilizationBps(m.MaxProtocolOIMicroUSDX/100*37, m.MaxProtocolOIMicroUSDX))
	line("utilization_bps", projected)
	skew := must(pricing.SkewImpactBps(m.MaxSkewImpactBps, projected))
	line("skew_impact_bps", skew)
	impact := must(pricing.TotalImpactBps(m.BaseSpreadBps, skew))
	line("total_impact_bps", impact)
	line("buy_execution", must(pricing.ExecutionPriceCents(buyVWAP, impact, m.TickPriceUnits, pricing.Buy)))
	line("sell_execution", must(pricing.ExecutionPriceCents(sellVWAP, impact, m.TickPriceUnits, pricing.Sell)))

	mark := entry + entry*37/10_000
	line("upnl_long", must(UnrealizedPnL(SideLong, notional, mark, entry)))
	line("upnl_short", must(UnrealizedPnL(SideShort, notional, mark, entry)))
	closeNotional := must(Notional(5))
	line("partial_close_realized_long", must(UnrealizedPnL(SideLong, closeNotional, mark, entry)))
	line("partial_close_realized_short", must(UnrealizedPnL(SideShort, closeNotional, mark, entry)))

	im := must(InitialMargin(notional, m.InitialMarginBps))
	mm := must(MaintenanceMargin(notional, m.MaintenanceMarginBps))
	liqFee := must(LiquidationFee(notional, m.LiquidationFeeBps))
	line("liq_price_long", must(LiquidationPriceCents(SideLong, entry, notional, mm, liqFee, 1_234, 567, im, m.TickPriceUnits)))
	line("liq_price_short", must(LiquidationPriceCents(SideShort, entry, notional, mm, liqFee, 1_234, 567, im, m.TickPriceUnits)))

	skewPpb := must(SkewFundingPpb(-m.MaxProtocolOIMicroUSDX/100*13, m.MaxProtocolOIMicroUSDX))
	line("skew_funding_ppb", skewPpb)
	applied := must(AppliedFundingPpb(823_456, skewPpb))
	line("applied_funding_ppb", applied)
	line("funding_full_interval", must(FundingTransfer(notional, applied, 28_800_000, 28_800_000)))
	line("funding_partial_interval", must(FundingTransfer(notional, applied, 3_600_000, 28_800_000)))

	c, err := ComputeCapacity(m.MaxPositionMicroUSDX, m.MaxPositionMicroUSDX/10, m.MaxPositionMicroUSDX/20,
		0, 0, m.MaxProtocolOIMicroUSDX, m.StressLossBps, m.LiquidationFeeBps)
	if err != nil {
		t.Fatalf("%s: capacity: %v", m.Symbol, err)
	}
	line("usable_capital", c.UsableCapitalMicro)
	line("required_capital", c.RequiredCapitalMicro)
	line("capacity_oi", c.CapacityOIMicro)
	line("effective_oi_cap", c.EffectiveOICapMicro)
	if c.ActivationAllowed {
		line("activation_allowed", 1)
	} else {
		line("activation_allowed", 0)
	}
}
