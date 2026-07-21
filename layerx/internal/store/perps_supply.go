package store

import (
	"context"
	"fmt"
)

// PerpBuckets is every perps-side term of the conservation identity:
// spendable balances + open holds + user perps margin (held reservations +
// open-position margin) + protocol liquidity + insurance + queued withdrawal
// accounting = circulating USDX, exactly in micro-USDX after every commit.
type PerpBuckets struct {
	SpendableMicroUSDX          int64
	OpenHoldsMicroUSDX          int64
	HeldReservationsMicroUSDX   int64
	OpenPositionMarginMicroUSDX int64
	LiquidityCapitalMicroUSDX   int64
	InsuranceCapitalMicroUSDX   int64
	QueuedWithdrawalsMicroUSDX  int64
}

// UserPerpsMarginMicroUSDX is the user-perps-margin term of the identity.
func (b PerpBuckets) UserPerpsMarginMicroUSDX() int64 {
	return b.HeldReservationsMicroUSDX + b.OpenPositionMarginMicroUSDX
}

// PerpConservationBuckets reads every bucket of the conservation identity in
// one statement, so reconciliation compares a single consistent snapshot.
func (s *Store) PerpConservationBuckets(ctx context.Context) (PerpBuckets, error) {
	var b PerpBuckets
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COALESCE(SUM(balance_usdx), 0) FROM accounts),
		       (SELECT COALESCE(SUM(amount_usdx), 0) FROM holds WHERE status = 'open'),
		       (SELECT COALESCE(SUM(amount_usdx), 0) FROM perp_margin_reservations WHERE status = 'held'),
		       (SELECT COALESCE(SUM(margin_usdx), 0) FROM perp_positions WHERE status IN ('OPEN', 'LIQUIDATING')),
		       (SELECT COALESCE(SUM(capital_usdx), 0) FROM perp_pools WHERE id = 'liquidity'),
		       (SELECT COALESCE(SUM(capital_usdx), 0) FROM perp_pools WHERE id = 'insurance'),
		       (SELECT COALESCE(SUM(amount_usdx), 0) FROM withdrawals WHERE status = 'queued')`).
		Scan(&b.SpendableMicroUSDX, &b.OpenHoldsMicroUSDX, &b.HeldReservationsMicroUSDX,
			&b.OpenPositionMarginMicroUSDX, &b.LiquidityCapitalMicroUSDX,
			&b.InsuranceCapitalMicroUSDX, &b.QueuedWithdrawalsMicroUSDX); err != nil {
		return PerpBuckets{}, fmt.Errorf("store: perp conservation buckets: %w", err)
	}
	return b, nil
}
