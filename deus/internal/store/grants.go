package store

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// SpendGrant is a cached spend policy row.
type SpendGrant struct {
	MaxPerCallWei string
	MaxTotalWei   string
	SpentWei      string
	ExpiresAt     time.Time
}

// ActiveGrantForCaller returns the best matching grant or nil.
func (s *Store) ActiveGrantForCaller(ctx context.Context, callerDID, serviceID string) (*SpendGrant, error) {
	var g SpendGrant
	err := s.pool.QueryRow(ctx, `
		SELECT max_per_call_wei, max_total_wei, spent_wei, expires_at
		FROM spend_grants
		WHERE caller_did = $1
		  AND (service_id IS NULL OR service_id = $2::uuid)
		  AND expires_at > now()
		ORDER BY service_id NULLS LAST
		LIMIT 1`, callerDID, serviceID,
	).Scan(&g.MaxPerCallWei, &g.MaxTotalWei, &g.SpentWei, &g.ExpiresAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("store: grant: %w", err)
	}
	return &g, nil
}

// GrantAllows checks whether quote wei fits grant caps.
func GrantAllows(g *SpendGrant, quoteWei string) (allowed bool, reason string) {
	if g == nil {
		return true, ""
	}
	quote, ok := new(big.Int).SetString(quoteWei, 10)
	if !ok {
		return false, "invalid quote wei"
	}
	perCall, ok := new(big.Int).SetString(g.MaxPerCallWei, 10)
	if ok && quote.Cmp(perCall) > 0 {
		return false, "per-call cap exceeded"
	}
	spent, _ := new(big.Int).SetString(g.SpentWei, 10)
	total, ok := new(big.Int).SetString(g.MaxTotalWei, 10)
	if ok {
		remaining := new(big.Int).Sub(total, spent)
		if quote.Cmp(remaining) > 0 {
			return false, "total cap exceeded"
		}
	}
	return true, ""
}
