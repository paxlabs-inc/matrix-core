// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package rates

import (
	"strings"
	"testing"
)

func TestLookupKnownAndUnknown(t *testing.T) {
	if _, ok := Lookup(ModelGPTOSS120B); !ok {
		t.Fatalf("expected gpt-oss-120b in rate table")
	}
	if _, ok := Lookup("nope/missing"); ok {
		t.Fatalf("unknown model should not resolve")
	}
}

func TestCostBasic(t *testing.T) {
	// gpt-oss-20b (v3 rate card, 1 PAX = $11.43):
	//   in  = $0.20/Mtoken → 0.017497813 PAX/Mtoken
	//   out = $0.40/Mtoken → 0.034995626 PAX/Mtoken
	// 1M in + 1M out → exactly (in_rate + out_rate)
	//   = 0.017497813 + 0.034995626 = 0.052493439 PAX (≈ $0.60).
	cost, err := Cost(ModelGPTOSS20B, 1_000_000, 1_000_000)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if cost != "0.052493439000" {
		t.Fatalf("expected 0.052493439000, got %q", cost)
	}
}

func TestCostNegativeRejected(t *testing.T) {
	if _, err := Cost(ModelGPTOSS20B, -1, 0); err == nil {
		t.Fatalf("expected error on negative tokens")
	}
}

func TestCostUnknownModelRejected(t *testing.T) {
	if _, err := Cost("does/not-exist", 100, 100); err == nil {
		t.Fatalf("expected error for unknown model")
	}
}

func TestCostSmallTokensPrecision(t *testing.T) {
	// deepseek-v4-flash (v3 rate card, 1 PAX = $11.43):
	//   in  = $0.30/Mtoken → 0.026246719 PAX/Mtoken
	//   out = $0.90/Mtoken → 0.078740157 PAX/Mtoken
	// 100 prompt + 50 completion →
	//   (100*0.026246719 + 50*0.078740157) / 1e6
	//   = (2.6246719 + 3.93700785) / 1e6
	//   = 6.56167975e-6 PAX.
	cost, err := Cost(ModelDeepSeekV4Flash, 100, 50)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// Formatted at 12 decimals (NUMERIC(20,12)): 0.000006561680.
	if cost != "0.000006561680" {
		t.Fatalf("expected 0.000006561680, got %q", cost)
	}
}

func TestAddPaxAndSubPaxRoundTrip(t *testing.T) {
	sum, err := AddPax("0.000001000000", "0.000002500000")
	if err != nil {
		t.Fatalf("AddPax: %v", err)
	}
	if sum != "0.000003500000" {
		t.Fatalf("expected 0.000003500000, got %q", sum)
	}
	rem, err := SubPax("10", sum)
	if err != nil {
		t.Fatalf("SubPax: %v", err)
	}
	if !strings.HasPrefix(rem, "9.999996500000") {
		t.Fatalf("expected 9.99999650…, got %q", rem)
	}
}

func TestSubPaxClampsAtZero(t *testing.T) {
	v, err := SubPax("1", "5")
	if err != nil {
		t.Fatalf("SubPax: %v", err)
	}
	if v != "0" {
		t.Fatalf("expected clamp to 0, got %q", v)
	}
}

func TestCmpPax(t *testing.T) {
	c, err := CmpPax("3.5", "3.5")
	if err != nil {
		t.Fatalf("CmpPax equal: %v", err)
	}
	if c != 0 {
		t.Fatalf("expected 0, got %d", c)
	}
	c, err = CmpPax("3.6", "3.5")
	if err != nil {
		t.Fatalf("CmpPax gt: %v", err)
	}
	if c <= 0 {
		t.Fatalf("expected positive, got %d", c)
	}
}

func TestRateTableVersionStable(t *testing.T) {
	if RateTableVersion <= 0 {
		t.Fatalf("RateTableVersion must be positive; got %d", RateTableVersion)
	}
}

func TestFreeTierWhitelistMembersOnTable(t *testing.T) {
	// Every model on the free-tier whitelist MUST resolve in the
	// rate table, otherwise the gateway would route a metered call
	// to an unknown-priced model.
	for slot, models := range FreeTierWhitelist() {
		for _, m := range models {
			if _, ok := Lookup(m); !ok {
				t.Fatalf("free-tier model %q (slot=%s) not on rate card", m, slot)
			}
		}
	}
}

func TestV1LaunchModelsOnRateCardAndWhitelist(t *testing.T) {
	// v1 launch (2026-06-01) added deepseek-v4-pro (planner upgrade +
	// compiler low-confidence escalation target) and kimi-k2.6 (executor
	// upgrade). Lock the rate-card membership + per-slot whitelist so a
	// future edit can't silently drop a launch model and 403 the fleet.
	if RateTableVersion < 2 {
		t.Fatalf("RateTableVersion must be >= 2 after the v1 launch additions; got %d", RateTableVersion)
	}
	for _, m := range []string{ModelDeepSeekV4Pro, ModelKimiK26} {
		if _, ok := Lookup(m); !ok {
			t.Fatalf("v1 launch model %q missing from rate card", m)
		}
	}
	wl := FreeTierWhitelist()
	mustContain := func(slot, model string) {
		t.Helper()
		for _, m := range wl[slot] {
			if m == model {
				return
			}
		}
		t.Fatalf("slot %q whitelist missing %q (have %v)", slot, model, wl[slot])
	}
	// New v1 pins: compiler escalates to v4-pro, planner is v4-pro,
	// executor default is kimi-k2.6.
	mustContain("compiler", ModelDeepSeekV4Pro)
	mustContain("planner", ModelDeepSeekV4Pro)
	mustContain("executor", ModelKimiK26)
	// Back-compat: the prior slot pins stay whitelisted.
	mustContain("compiler", ModelCompilerFreeTier)
	mustContain("executor", ModelExecutorFreeTier)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
