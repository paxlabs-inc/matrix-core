// Package quality records PoFQ delivery samples and folds service scores (docs/04-onchain.md §4.3).
package quality

import (
	"context"
	"math/big"

	"github.com/paxlabs-inc/deus/internal/store"
)

// Service updates quality scores from invocation outcomes.
type Service struct {
	store *store.Store
}

// New returns a quality service.
func New(st *store.Store) *Service {
	return &Service{store: st}
}

// Sample records one delivery sample and updates rolling quality_score.
func (s *Service) Sample(ctx context.Context, serviceID, outcome string, latencyMS int) error {
	row, err := s.store.GetServiceByID(ctx, serviceID)
	if err != nil {
		return err
	}
	score := sampleScore(outcome, latencyMS)
	current := big.NewInt(900000000000000000) // 0.9 * 1e18 default
	if row.QualityScore != nil {
		if v, ok := new(big.Int).SetString(*row.QualityScore, 10); ok {
			current = v
		}
	}
	// Exponential moving average: new = 0.9*old + 0.1*sample
	newScore := new(big.Int).Mul(current, big.NewInt(9))
	newScore.Add(newScore, new(big.Int).Mul(score, big.NewInt(1)))
	newScore.Div(newScore, big.NewInt(10))
	return s.store.SetQualityScore(ctx, serviceID, newScore.Text(10))
}

func sampleScore(outcome string, latencyMS int) *big.Int {
	perfect := big.NewInt(1_000_000_000_000_000_000) // 1e18
	switch outcome {
	case "ok":
		if latencyMS > 2000 {
			return big.NewInt(800_000_000_000_000_000)
		}
		return perfect
	case "voided", "error":
		return big.NewInt(100_000_000_000_000_000)
	default:
		return big.NewInt(500_000_000_000_000_000)
	}
}
