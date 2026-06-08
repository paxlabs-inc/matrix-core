package discovery_test

import (
	"testing"

	"github.com/paxlabs-inc/deus/internal/discovery"
)

func TestExtractMaxPAXAndUptime(t *testing.T) {
	out := discovery.ExtractConstraints(
		"weather API with >99% uptime under 0.001 PAX per call",
		map[string]any{},
	)
	if out.Filters["max_price_wei"] == nil {
		t.Fatalf("expected max_price_wei, got %+v", out.Filters)
	}
	if out.Filters["min_uptime_bps"] != 9900 {
		t.Fatalf("uptime filter %+v", out.Filters["min_uptime_bps"])
	}
	if out.SemanticQuery == "" {
		t.Fatal("expected semantic remainder")
	}
}
