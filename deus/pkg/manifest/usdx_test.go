package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func usdxManifest() *Manifest {
	return &Manifest{
		SchemaVersion: "1",
		Slug:          "usdx-test",
		Kind:          "data",
		DisplayName:   "USDX Test",
		Summary:       "usdx pricing test",
		Owner:         "0x00000000000000000000000000000000000000aa",
		PayoutAddress: "0x00000000000000000000000000000000000000aa",
		PayeeDID:      "did:matrix:usdx-test:00112233aabbccdd",
		Mode:          "proxy",
		Operations: []Operation{{
			Name:         "run",
			Method:       "POST",
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
		}},
		Pricing: []Pricing{{
			Operation:     "run",
			Model:         "per_call",
			Unit:          "call",
			UnitPriceUSDX: "0.031500",
			MinChargeUSDX: "0.031500",
		}},
		Endpoint: &Endpoint{ProxyURL: "https://example.com/run"},
	}
}

func TestValidateUSDXOnlyManifest(t *testing.T) {
	m := usdxManifest()
	if err := Validate(m); err != nil {
		t.Fatalf("usdx-only manifest invalid: %v", err)
	}
	if err := ValidateUSDXOnly(m); err != nil {
		t.Fatalf("ValidateUSDXOnly rejected a pure-usdx manifest: %v", err)
	}
}

func TestValidateDenominationPairs(t *testing.T) {
	// Half a usdx pair is rejected.
	m := usdxManifest()
	m.Pricing[0].MinChargeUSDX = ""
	if err := Validate(m); err == nil {
		t.Fatal("half usdx pair accepted")
	}
	// No denomination at all is rejected.
	m = usdxManifest()
	m.Pricing[0].UnitPriceUSDX = ""
	m.Pricing[0].MinChargeUSDX = ""
	if err := Validate(m); err == nil {
		t.Fatal("denomination-less pricing accepted")
	}
	// Zero usdx pricing is rejected.
	m = usdxManifest()
	m.Pricing[0].UnitPriceUSDX = "0"
	m.Pricing[0].MinChargeUSDX = "0.000000"
	if err := Validate(m); err == nil {
		t.Fatal("zero usdx pricing accepted")
	}
	// Excess precision is rejected by the schema.
	m = usdxManifest()
	m.Pricing[0].UnitPriceUSDX = "0.0000001"
	if err := Validate(m); err == nil {
		t.Fatal("7dp usdx price accepted")
	}
	// Wei + usdx together stay valid under the default rules (migration state).
	m = usdxManifest()
	m.Pricing[0].PriceWei = "200000000000000"
	m.Pricing[0].MinChargeWei = "200000000000000"
	if err := Validate(m); err != nil {
		t.Fatalf("dual-denominated manifest invalid: %v", err)
	}
	// ...but ValidateUSDXOnly rejects any wei presence.
	if err := ValidateUSDXOnly(m); err == nil {
		t.Fatal("ValidateUSDXOnly accepted wei fields")
	}
	// And a wei-only manifest fails USDX-only validation.
	m = usdxManifest()
	m.Pricing[0].UnitPriceUSDX = ""
	m.Pricing[0].MinChargeUSDX = ""
	m.Pricing[0].PriceWei = "200000000000000"
	m.Pricing[0].MinChargeWei = "200000000000000"
	if err := Validate(m); err != nil {
		t.Fatalf("legacy wei manifest invalid under default rules: %v", err)
	}
	if err := ValidateUSDXOnly(m); err == nil {
		t.Fatal("ValidateUSDXOnly accepted a wei-only manifest")
	}
}

func TestValidatePayeeDID(t *testing.T) {
	// A priced service without payee_did is fine under default (migration)
	// rules but rejected once the LayerX rail is the only rail.
	m := usdxManifest()
	m.PayeeDID = ""
	if err := Validate(m); err != nil {
		t.Fatalf("payee-less manifest invalid under default rules: %v", err)
	}
	if err := ValidateUSDXOnly(m); err == nil {
		t.Fatal("ValidateUSDXOnly accepted a priced service without payee_did")
	}
	// Malformed DID shapes are rejected by the schema.
	m = usdxManifest()
	m.PayeeDID = "did:matrix:short"
	if err := Validate(m); err == nil {
		t.Fatal("malformed payee_did accepted")
	}
}

func TestValidateSettlementMode(t *testing.T) {
	m := usdxManifest()
	m.SettlementMode = "hold"
	m.HoldTTLS = 60
	if err := ValidateUSDXOnly(m); err != nil {
		t.Fatalf("hold-mode manifest invalid: %v", err)
	}
	// hold_ttl_s without hold mode is rejected.
	m = usdxManifest()
	m.HoldTTLS = 60
	if err := Validate(m); err == nil {
		t.Fatal("hold_ttl_s without settlement_mode=hold accepted")
	}
	// Unknown settlement modes are rejected by the schema.
	m = usdxManifest()
	m.SettlementMode = "escrow"
	if err := Validate(m); err == nil {
		t.Fatal("unknown settlement_mode accepted")
	}
}

func TestFixturesCarryUSDXPricing(t *testing.T) {
	dir := filepath.Join("..", "..", "test", "fixtures")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		m, err := ValidateBytes(data)
		if err != nil {
			t.Fatalf("fixture %s invalid: %v", e.Name(), err)
		}
		for _, p := range m.Pricing {
			if p.UnitPriceUSDX == "" || p.MinChargeUSDX == "" {
				t.Fatalf("fixture %s pricing %q has no usdx denomination", e.Name(), p.Operation)
			}
		}
	}
}
