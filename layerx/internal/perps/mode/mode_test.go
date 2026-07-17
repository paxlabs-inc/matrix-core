package mode

import (
	"reflect"
	"testing"
)

func TestParseMarketModes(t *testing.T) {
	got, err := ParseMarketModes("btc=shadow, ETH = CANARY")
	if err != nil {
		t.Fatalf("ParseMarketModes: %v", err)
	}
	want := map[string]Mode{"BTC": Shadow, "ETH": Canary}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("market modes = %#v, want %#v", got, want)
	}

	for _, raw := range []string{
		"BTC", "UNKNOWN=ACTIVE", "BTC=INVALID", "BTC=OFF,BTC=ACTIVE",
	} {
		if _, err := ParseMarketModes(raw); err == nil {
			t.Errorf("ParseMarketModes(%q) succeeded, want error", raw)
		}
	}
}

func TestPermissions(t *testing.T) {
	tests := []struct {
		mode Mode
		want Permissions
	}{
		{Off, Permissions{Withdraw: true}},
		{Shadow, Permissions{PublicReads: true, ShadowCalculate: true, Withdraw: true}},
		{Canary, Permissions{PublicReads: true, ShadowCalculate: true, Increase: true, Reduce: true, Cancel: true, Withdraw: true}},
		{Active, Permissions{PublicReads: true, ShadowCalculate: true, Increase: true, Reduce: true, Cancel: true, Withdraw: true}},
		{ReduceOnly, Permissions{PublicReads: true, ShadowCalculate: true, Reduce: true, Cancel: true, Withdraw: true}},
		{Paused, Permissions{PublicReads: true, Cancel: true, Withdraw: true}},
	}
	for _, tt := range tests {
		if got := tt.mode.Permissions(); got != tt.want {
			t.Errorf("%s permissions = %#v, want %#v", tt.mode, got, tt.want)
		}
	}
}

func TestRegistryFailsClosedUsesMostRestrictiveModeAndCopiesConfig(t *testing.T) {
	configured := map[string]Mode{
		"BTC": Canary,
		"ETH": ReduceOnly,
		"SOL": Paused,
	}
	r, err := NewRegistry(Active, configured)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	configured["BTC"] = Off
	tests := map[string]Mode{
		"BTC": Canary,
		"ETH": ReduceOnly,
		"SOL": Paused,
		"BNB": Off,
	}
	for symbol, want := range tests {
		if got := r.Effective(symbol); got != want {
			t.Errorf("Effective(%s) = %s, want %s", symbol, got, want)
		}
	}

	reloaded, err := NewRegistry(Shadow, map[string]Mode{"BTC": Canary})
	if err != nil {
		t.Fatalf("NewRegistry reload: %v", err)
	}
	if got := reloaded.Effective("BTC"); got != Shadow {
		t.Fatalf("global SHADOW + market CANARY = %s, want SHADOW", got)
	}
	disabled, err := NewRegistry(Shadow, map[string]Mode{"BTC": Off})
	if err != nil {
		t.Fatalf("NewRegistry disabled: %v", err)
	}
	if got := disabled.Effective("BTC"); got != Off {
		t.Fatalf("global SHADOW + market OFF = %s, want OFF", got)
	}
}

func TestRegistryContainsLockedMarketSet(t *testing.T) {
	want := []string{
		"ASTER", "AVAX", "BNB", "BTC", "ETH", "GOOGL", "HYPE", "LINK",
		"NAS100", "NVDA", "PAX", "SID", "SOL", "SPX500", "TRUMP", "TSLA",
		"XAU", "XRP",
	}
	if got := Symbols(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Symbols = %#v, want %#v", got, want)
	}
}
