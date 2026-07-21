package risk

import (
	"errors"
	"testing"

	"github.com/paxlabs-inc/layerx/internal/perps/market"
	pmath "github.com/paxlabs-inc/layerx/internal/perps/math"
)

func TestNotionalAndMargins(t *testing.T) {
	n, err := Notional(-7)
	if err != nil || n != 70_000_000 {
		t.Fatalf("notional = %d %v", n, err)
	}
	if n, _ := Notional(0); n != 0 {
		t.Fatalf("zero notional = %d", n)
	}
	im, err := InitialMargin(70_000_000, 3334)
	if err != nil || im != 23_338_000 {
		t.Fatalf("im = %d %v", im, err)
	}
	im, err = InitialMargin(1, 3334)
	if err != nil || im != 1 {
		t.Fatalf("im adverse round up = %d %v", im, err)
	}
	mm, err := MaintenanceMargin(70_000_000, 1250)
	if err != nil || mm != 8_750_000 {
		t.Fatalf("mm = %d %v", mm, err)
	}
	fee, err := Fee(10_000_001, 5)
	if err != nil || fee != 5_001 {
		t.Fatalf("fee = %d %v, want ceil(50000.005)=5001", fee, err)
	}
	if _, err := Notional(-1 << 63); !errors.Is(err, pmath.ErrOverflow) {
		t.Fatalf("notional overflow = %v", err)
	}
}

func TestUnrealizedPnLTruncation(t *testing.T) {
	up, err := UnrealizedPnL(SideLong, 100_000_000, 10_037, 10_000)
	if err != nil || up != 370_000 {
		t.Fatalf("long upnl = %d %v", up, err)
	}
	up, err = UnrealizedPnL(SideShort, 100_000_000, 10_037, 10_000)
	if err != nil || up != -370_000 {
		t.Fatalf("short upnl = %d %v", up, err)
	}
	up, err = UnrealizedPnL(SideLong, 100_000_000, 10_001, 30_000)
	if err != nil || up != -66_663_333 {
		t.Fatalf("trunc toward zero = %d %v, want -66663333", up, err)
	}
	up, err = UnrealizedPnL(SideShort, 100_000_000, 10_001, 30_000)
	if err != nil || up != 66_663_333 {
		t.Fatalf("short gain trunc = %d %v", up, err)
	}
	if _, err := UnrealizedPnL(SideLong, 1, 1, 0); err == nil {
		t.Fatal("zero entry must be rejected")
	}
	if _, err := UnrealizedPnL(2, 1, 1, 1); err == nil {
		t.Fatal("bad side sign must be rejected")
	}
}

func TestEquityAndWithdrawal(t *testing.T) {
	eq, err := Equity(10_000_000, -2_000_000, 300_000, 100_000)
	if err != nil || eq != 7_800_000 {
		t.Fatalf("equity = %d %v", eq, err)
	}
	w, err := AvailableWithdrawal(50_000_000, 10_000_000, 20_000_000, 15_000_000)
	if err != nil || w != 35_000_000 {
		t.Fatalf("withdrawal = %d %v", w, err)
	}
	w, err = AvailableWithdrawal(50_000_000, 10_000_000, 20_000_000, 25_000_000)
	if err != nil || w != 40_000_000 {
		t.Fatalf("withdrawal no shortfall = %d %v", w, err)
	}
	w, err = AvailableWithdrawal(5_000_000, 10_000_000, 0, 0)
	if err != nil || w != 0 {
		t.Fatalf("withdrawal clamps at zero = %d %v", w, err)
	}
}

func TestLiquidationPriceRounding(t *testing.T) {
	long, err := LiquidationPriceCents(SideLong, 10_000, 100_000_000, 1_250_000, 50_000, 0, 0, 2_000_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if long != 9_930 {
		t.Fatalf("long liq = %d, want 10000*(1-0.007)=9930 exact", long)
	}
	longAdverse, err := LiquidationPriceCents(SideLong, 10_000, 100_000_000, 1_250_000, 50_000, 1, 0, 2_000_000, 1)
	if err != nil || longAdverse != 9_931 {
		t.Fatalf("long liq adverse round up = %d %v, want 9931", longAdverse, err)
	}
	short, err := LiquidationPriceCents(SideShort, 10_000, 100_000_000, 1_250_000, 50_000, 0, 0, 2_000_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if short != 10_070 {
		t.Fatalf("short liq = %d, want floor(10000*1.007)=10070 (round down, earlier)", short)
	}
	elig, err := LiquidationEligible(1_300_000, 1_250_000, 50_000)
	if err != nil || !elig {
		t.Fatalf("eligible at boundary = %v %v", elig, err)
	}
	elig, err = LiquidationEligible(1_300_001, 1_250_000, 50_000)
	if err != nil || elig {
		t.Fatalf("not eligible above boundary = %v %v", elig, err)
	}
}

func TestFunding(t *testing.T) {
	skew, err := SkewFundingPpb(-33_000_000, 100_000_000)
	if err != nil || skew != -82_500 {
		t.Fatalf("skew = %d %v, want trunc(-33e6*250000/1e8)", skew, err)
	}
	applied, err := AppliedFundingPpb(700_000, 100_000)
	if err != nil || applied != market.MaxFundingPpb {
		t.Fatalf("clamp high = %d %v", applied, err)
	}
	applied, err = AppliedFundingPpb(-800_000, -100_000)
	if err != nil || applied != -market.MaxFundingPpb {
		t.Fatalf("clamp low = %d %v", applied, err)
	}
	tr, err := FundingTransfer(100_000_000, 120_000, 28_800_000, 28_800_000)
	if err != nil || tr != 12_000 {
		t.Fatalf("full-interval transfer = %d %v", tr, err)
	}
	tr, err = FundingTransfer(100_000_000, 120_000, 3_600_000, 28_800_000)
	if err != nil || tr != 1_500 {
		t.Fatalf("partial-interval transfer = %d %v", tr, err)
	}
	tr, err = FundingTransfer(100_000_000, -120_001, 3_600_000, 28_800_000)
	if err != nil || tr != -1_500 {
		t.Fatalf("negative trunc toward zero = %d %v", tr, err)
	}
	if _, err := FundingTransfer(1, 1, 30_000_000, 28_800_000); err == nil {
		t.Fatal("elapsed duration beyond interval must be rejected")
	}
}

func TestCapacityGate(t *testing.T) {
	btc, _ := market.Lookup("BTC")
	required := int64(20_500_000_000)
	c, err := ComputeCapacity(20_000_000_000, 400_000_000, 0, 0, 0,
		btc.MaxProtocolOIMicroUSDX, btc.StressLossBps, btc.LiquidationFeeBps)
	if err != nil {
		t.Fatal(err)
	}
	if c.RequiredCapitalMicro != required {
		t.Fatalf("required = %d, want %d", c.RequiredCapitalMicro, required)
	}
	if c.ActivationAllowed {
		t.Fatal("activation must require usable >= required")
	}
	if c.CapacityOIMicro != 99_512_195_121 || c.EffectiveOICapMicro != c.CapacityOIMicro {
		t.Fatalf("effective cap = %d capacity = %d, want min(configured, floor(20.4e9*1e4/2050))", c.EffectiveOICapMicro, c.CapacityOIMicro)
	}
	c, err = ComputeCapacity(required, 500_000_000, 500_000_000, 0, 0,
		btc.MaxProtocolOIMicroUSDX, btc.StressLossBps, btc.LiquidationFeeBps)
	if err != nil || !c.ActivationAllowed {
		t.Fatalf("activation at boundary: %+v %v", c, err)
	}
	c, err = ComputeCapacity(1_025_000_000, 0, 0, 0, 0,
		btc.MaxProtocolOIMicroUSDX, btc.StressLossBps, btc.LiquidationFeeBps)
	if err != nil {
		t.Fatal(err)
	}
	if c.CapacityOIMicro != 5_000_000_000 || c.EffectiveOICapMicro != 5_000_000_000 {
		t.Fatalf("capacity = %+v, want 5000 USDX capacity cap", c)
	}
	c, err = ComputeCapacity(0, 0, 1, 0, 0, btc.MaxProtocolOIMicroUSDX, btc.StressLossBps, btc.LiquidationFeeBps)
	if err != nil || c.CapacityOIMicro != 0 || c.ActivationAllowed {
		t.Fatalf("negative usable = %+v %v", c, err)
	}
}
