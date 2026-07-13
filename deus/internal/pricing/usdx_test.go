package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/pricingmath"
)

func testURI() string {
	if v := os.Getenv("DEUS_POSTGRES_URI"); v != "" {
		return v
	}
	return "postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable"
}

// TestQuoteUSDXFromMigratedFixture drives the real store: the (usdx-converted)
// proxy-weather fixture manifest is registered and quoted through QuoteUSDX,
// proving migrated fixtures quote correctly in micro-USDX.
func TestQuoteUSDXFromMigratedFixture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.New(ctx, testURI())
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "proxy-weather.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The slug is unique per run so repeated runs on a persistent test DB never
	// collide on the services unique index.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	slug := fmt.Sprintf("weather-usdx-%d", time.Now().UnixNano())
	doc["slug"] = slug
	manifest, _ := json.Marshal(doc)

	devID, err := st.UpsertDeveloperByWallet(ctx, "0x00000000000000000000000000000000000000ab", "0x00000000000000000000000000000000000000ab", "usdx-quote-dev")
	if err != nil {
		t.Fatalf("UpsertDeveloperByWallet: %v", err)
	}
	svcID, err := st.InsertDraftService(ctx, store.ServiceRow{
		DeveloperID:  devID,
		Slug:         slug,
		Kind:         "data",
		Mode:         "proxy",
		DisplayName:  "Weather USDX",
		Summary:      "usdx quote test",
		Manifest:     manifest,
		ManifestHash: "0xdead",
		Status:       "active",
	})
	if err != nil {
		t.Fatalf("InsertDraftService: %v", err)
	}

	svc := New(st)
	plan, chargeMicro, err := svc.QuoteUSDX(ctx, svcID, "forecast", "1")
	if err != nil {
		t.Fatalf("QuoteUSDX: %v", err)
	}
	if !plan.HasUSDX() {
		t.Fatalf("plan has no usdx denomination: %+v", plan)
	}
	if got := pricingmath.FormatUSDX(chargeMicro); got != "0.000200" {
		t.Fatalf("fixture quote = %s USDX, want 0.000200", got)
	}

	// A wei-only manifest quoted through QuoteUSDX is a typed error, not a
	// silent zero charge.
	for i := range doc["pricing"].([]any) {
		p := doc["pricing"].([]any)[i].(map[string]any)
		delete(p, "unit_price_usdx")
		delete(p, "min_charge_usdx")
	}
	slug2 := slug + "-wei"
	doc["slug"] = slug2
	manifest2, _ := json.Marshal(doc)
	svcID2, err := st.InsertDraftService(ctx, store.ServiceRow{
		DeveloperID:  devID,
		Slug:         slug2,
		Kind:         "data",
		Mode:         "proxy",
		DisplayName:  "Weather Wei",
		Summary:      "wei quote test",
		Manifest:     manifest2,
		ManifestHash: "0xdead",
		Status:       "active",
	})
	if err != nil {
		t.Fatalf("InsertDraftService wei: %v", err)
	}
	if _, _, err := svc.QuoteUSDX(ctx, svcID2, "forecast", "1"); err == nil {
		t.Fatal("QuoteUSDX accepted a wei-only plan")
	}
}
