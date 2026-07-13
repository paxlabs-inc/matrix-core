package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func newUSDXTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	s, err := New(ctx, testURI())
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx, testMigrationsDir()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s, ctx
}

func insertTestService(t *testing.T, s *Store, ctx context.Context, slug string, manifest []byte) string {
	t.Helper()
	devID, err := s.UpsertDeveloperByWallet(ctx, "0x00000000000000000000000000000000000000aa", "0x00000000000000000000000000000000000000aa", "usdx-test-dev")
	if err != nil {
		t.Fatalf("UpsertDeveloperByWallet: %v", err)
	}
	id, err := s.InsertDraftService(ctx, ServiceRow{
		DeveloperID:  devID,
		Slug:         slug,
		Kind:         "data",
		Mode:         "proxy",
		DisplayName:  "USDX Test",
		Summary:      "usdx pricing test",
		Manifest:     json.RawMessage(manifest),
		ManifestHash: "0xdead",
		Status:       "active",
	})
	if err != nil {
		t.Fatalf("InsertDraftService: %v", err)
	}
	return id
}

// TestWeiRowConversionToUSDX replays migration 006's guarded conversion UPDATE
// over a freshly-inserted wei-only pricing row (simulating pre-006 data; the
// statement is idempotent by its IS NULL guard) and proves the 1 PAX = 1 USDX
// conversion lands: 2e14 wei -> 200 micro -> "0.000200".
func TestWeiRowConversionToUSDX(t *testing.T) {
	s, ctx := newUSDXTestStore(t)
	slug := fmt.Sprintf("usdx-conv-%d", time.Now().UnixNano())
	manifest := []byte(`{"slug":"` + slug + `"}`)
	svcID := insertTestService(t, s, ctx, slug, manifest)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO pricing_plans (service_id, model, unit, price_wei, min_charge_wei, version)
		VALUES ($1, 'per_call', 'call', '200000000000000', '1', 1)`, svcID); err != nil {
		t.Fatalf("insert wei row: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE pricing_plans SET
			price_usdx = (GREATEST(CEIL(price_wei::numeric / 1000000000000),
			                       CASE WHEN price_wei::numeric > 0 THEN 1 ELSE 0 END)
			              / 1000000.0)::numeric(20,6)::text,
			min_charge_usdx = (GREATEST(CEIL(min_charge_wei::numeric / 1000000000000),
			                            CASE WHEN min_charge_wei::numeric > 0 THEN 1 ELSE 0 END)
			                   / 1000000.0)::numeric(20,6)::text
		WHERE price_usdx IS NULL AND price_wei IS NOT NULL`); err != nil {
		t.Fatalf("replay conversion: %v", err)
	}

	plans, err := s.PricingByService(ctx, svcID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("PricingByService: %v (%d plans)", err, len(plans))
	}
	if plans[0].PriceUSDX != "0.000200" {
		t.Fatalf("converted price_usdx = %q, want 0.000200", plans[0].PriceUSDX)
	}
	// A sub-micro positive wei price floors at 1 micro, never free.
	if plans[0].MinChargeUSDX != "0.000001" {
		t.Fatalf("converted min_charge_usdx = %q, want 0.000001", plans[0].MinChargeUSDX)
	}
}

// TestUSDXPricingRowRoundTrip proves usdx-denominated plans persist and read
// back through InsertPricingPlans/PricingByService with no wei fields.
func TestUSDXPricingRowRoundTrip(t *testing.T) {
	s, ctx := newUSDXTestStore(t)
	slug := fmt.Sprintf("usdx-rt-%d", time.Now().UnixNano())
	svcID := insertTestService(t, s, ctx, slug, []byte(`{"slug":"`+slug+`"}`))

	if err := s.InsertPricingPlans(ctx, svcID, []PricingRow{{
		Model: "per_call", Unit: "call", PriceUSDX: "0.031500", MinChargeUSDX: "0.031500", Version: 1,
	}}); err != nil {
		t.Fatalf("InsertPricingPlans: %v", err)
	}
	plans, err := s.PricingByService(ctx, svcID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("PricingByService: %v (%d plans)", err, len(plans))
	}
	p := plans[0]
	if p.PriceUSDX != "0.031500" || p.MinChargeUSDX != "0.031500" || p.PriceWei != "" || p.MinChargeWei != "" {
		t.Fatalf("bad usdx plan round trip: %+v", p)
	}
}
