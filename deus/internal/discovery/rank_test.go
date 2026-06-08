package discovery_test

import (
	"testing"

	"github.com/paxlabs-inc/deus/internal/discovery"
	"github.com/paxlabs-inc/deus/internal/store"
)

func TestBlendScorePrefersQuality(t *testing.T) {
	w := discovery.DefaultRankingWeights()
	qs := "0.9"
	high := store.DiscoverCandidate{
		ServiceRow:  store.ServiceRow{QualityScore: &qs},
		SemanticSim: 0.5,
	}
	lowQ := "0.2"
	low := store.DiscoverCandidate{
		ServiceRow:  store.ServiceRow{QualityScore: &lowQ},
		SemanticSim: 0.5,
	}
	if discovery.BlendScoreWithPrice(w, high, "", "") <= discovery.BlendScoreWithPrice(w, low, "", "") {
		t.Fatal("expected higher quality to score higher")
	}
}
