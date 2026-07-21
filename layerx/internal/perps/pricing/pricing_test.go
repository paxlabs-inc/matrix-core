package pricing

import (
	"errors"
	"testing"

	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
)

func levels(pairs ...[2]int64) []crossverse.Level {
	out := make([]crossverse.Level, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, crossverse.Level{PriceCents: p[0], Contracts: p[1]})
	}
	return out
}

func TestBookVWAP(t *testing.T) {
	asks := levels([2]int64{10000, 5}, [2]int64{10010, 10}, [2]int64{10050, 100})
	v, err := BookVWAP(asks, 12, Buy)
	if err != nil {
		t.Fatal(err)
	}
	if v != 10006 {
		t.Fatalf("buy vwap = %d, want 10006 (ceil of 120070/12)", v)
	}
	bids := levels([2]int64{10000, 5}, [2]int64{9990, 10}, [2]int64{9950, 100})
	v, err = BookVWAP(bids, 12, Sell)
	if err != nil {
		t.Fatal(err)
	}
	if v != 9994 {
		t.Fatalf("sell vwap = %d, want 9994 (floor of 119930/12)", v)
	}
	if _, err := BookVWAP(asks, 200, Buy); !errors.Is(err, ErrInsufficientBookDepth) {
		t.Fatalf("depth = %v, want ErrInsufficientBookDepth", err)
	}
	if _, err := BookVWAP(asks, 0, Buy); err == nil {
		t.Fatal("zero contracts must be rejected")
	}
	if v, _ := BookVWAP(asks, 5, Buy); v != 10000 {
		t.Fatalf("single level vwap = %d", v)
	}
}

func TestUtilizationAndSkew(t *testing.T) {
	u, err := UtilizationBps(37_000_000, 100_000_000)
	if err != nil || u != 3_700 {
		t.Fatalf("u = %d %v", u, err)
	}
	u, err = UtilizationBps(-37_000_001, 100_000_000)
	if err != nil || u != 3_701 {
		t.Fatalf("u ceil of abs = %d %v", u, err)
	}
	u, err = UtilizationBps(250_000_000, 100_000_000)
	if err != nil || u != 10_000 {
		t.Fatalf("u cap = %d %v", u, err)
	}
	if _, err := UtilizationBps(1, 0); err == nil {
		t.Fatal("zero cap must be rejected")
	}
	skew, err := SkewImpactBps(20, 3_700)
	if err != nil || skew != 3 {
		t.Fatalf("skew = %d %v, want ceil(20*13690000/1e8)=3", skew, err)
	}
	skew, err = SkewImpactBps(20, 10_000)
	if err != nil || skew != 20 {
		t.Fatalf("skew at full utilization = %d %v", skew, err)
	}
	skew, err = SkewImpactBps(20, 0)
	if err != nil || skew != 0 {
		t.Fatalf("skew at zero = %d %v", skew, err)
	}
}

func TestExecutionPriceAdverseRounding(t *testing.T) {
	buy, err := ExecutionPriceCents(10_001, 3, 1, Buy)
	if err != nil {
		t.Fatal(err)
	}
	if buy != 10_005 {
		t.Fatalf("buy = %d, want ceil(10001*10003/10000)=10005", buy)
	}
	sell, err := ExecutionPriceCents(10_001, 3, 1, Sell)
	if err != nil {
		t.Fatal(err)
	}
	if sell != 9_997 {
		t.Fatalf("sell = %d, want floor(10001*9997/10000)=9997", sell)
	}
	if buy <= 10_001 || sell >= 10_001 {
		t.Fatal("impact must worsen the taker's price on both sides")
	}
	buyTick, err := ExecutionPriceCents(10_001, 3, 25, Buy)
	if err != nil || buyTick != 10_025 {
		t.Fatalf("buy tick = %d %v", buyTick, err)
	}
	sellTick, err := ExecutionPriceCents(10_001, 3, 25, Sell)
	if err != nil || sellTick != 9_975 {
		t.Fatalf("sell tick = %d %v", sellTick, err)
	}
}
