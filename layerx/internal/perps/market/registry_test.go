package market

import (
	"reflect"
	"testing"

	"github.com/paxlabs-inc/layerx/internal/perps/mode"
)

func TestRegistryMatchesLockedSpec(t *testing.T) {
	if Count() != 18 {
		t.Fatalf("market count = %d, want 18", Count())
	}
	if !reflect.DeepEqual(Symbols(), mode.Symbols()) {
		t.Fatalf("registry symbols %v differ from mode symbols %v", Symbols(), mode.Symbols())
	}

	btc, err := Lookup("btc")
	if err != nil {
		t.Fatal(err)
	}
	want := Market{
		Symbol: "BTC", Class: "crypto_major", TickPriceUnits: 1,
		MinOrderContracts: 1, MinPositionContracts: 1,
		InitialMarginBps: 2000, MaintenanceMarginBps: 1250, MaxLeverageX: 5,
		MaxPositionMicroUSDX: 25_000_000_000, MaxProtocolOIMicroUSDX: 100_000_000_000,
		MakerFeeBps: 2, TakerFeeBps: 5, LiquidationFeeBps: 50,
		BaseSpreadBps: 1, MaxSkewImpactBps: 20, DivergenceLimitBps: 75,
		StressLossBps: 2000, Session: Session24x7,
	}
	if btc != want {
		t.Fatalf("BTC = %+v, want %+v", btc, want)
	}

	trump, err := Lookup("TRUMP")
	if err != nil {
		t.Fatal(err)
	}
	if trump.InitialMarginBps != 10000 || trump.MaxLeverageX != 1 || trump.DivergenceLimitBps != 200 ||
		trump.StressLossBps != 7500 || trump.Class != "crypto_restricted" {
		t.Fatalf("TRUMP = %+v", trump)
	}

	xau, err := Lookup("XAU")
	if err != nil {
		t.Fatal(err)
	}
	if xau.Session != Session24x7 || xau.Class != "gold" {
		t.Fatalf("XAU = %+v", xau)
	}

	for _, m := range All() {
		if m.MaintenanceMarginBps >= m.InitialMarginBps {
			t.Fatalf("%s maintenance %d must be below initial %d", m.Symbol, m.MaintenanceMarginBps, m.InitialMarginBps)
		}
		if m.InitialMarginBps*m.MaxLeverageX < 10_000 {
			t.Fatalf("%s initial margin %d with leverage %d undercovers notional", m.Symbol, m.InitialMarginBps, m.MaxLeverageX)
		}
		if m.MakerFeeBps >= m.TakerFeeBps {
			t.Fatalf("%s maker fee %d must be below taker fee %d", m.Symbol, m.MakerFeeBps, m.TakerFeeBps)
		}
		if m.TickPriceUnits != 1 || m.MinOrderContracts != 1 || m.MinPositionContracts != 1 {
			t.Fatalf("%s tick/min values are not locked to 1", m.Symbol)
		}
		if m.DivergenceLimitBps <= 0 || m.StressLossBps <= 0 || m.MaxPositionMicroUSDX <= 0 || m.MaxProtocolOIMicroUSDX < m.MaxPositionMicroUSDX {
			t.Fatalf("%s limits are inconsistent: %+v", m.Symbol, m)
		}
		if m.Session != Session24x7 {
			t.Fatalf("%s session %q, want 24x7", m.Symbol, m.Session)
		}
	}

	if _, err := Lookup("DOGE"); err == nil {
		t.Fatal("unknown symbol must error")
	}
}
