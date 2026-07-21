package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PerpLiquidationPlan is the engine-computed close of an eligible position.
// The store executes it under a SKIP LOCKED claim so concurrent liquidators
// produce exactly one outcome, and applies the margin -> insurance ->
// liquidity waterfall against the ACTUAL locked balances.
type PerpLiquidationPlan struct {
	PositionID       string
	ExpectContracts  int64
	CloseContracts   int64
	FullClose        bool
	Fill             PerpIntentFill
	RealizedPnLMicro int64
	ActingDID        string
	IdempotencyKey   string
	Ref              PerpSnapshotRef
}

// PerpLiquidationResult reports the executed waterfall.
type PerpLiquidationResult struct {
	LiquidationID       string
	OrderID             string
	FillID              string
	MarginAbsorbedMicro int64
	InsurancePaidMicro  int64
	PoolPaidMicro       int64
	DeficitMicro        int64
	Paused              bool
}

// LiquidatePerpPosition executes one liquidation atomically. A deficit that
// margin and insurance cannot absorb writes NO user credit; it is recorded on
// the liquidation row, every market is set PAUSED, and perps.insolvency is
// journaled with the exact unmet amount.
func (s *Store) LiquidatePerpPosition(ctx context.Context, plan PerpLiquidationPlan) (PerpLiquidationResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: begin liquidation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := scanPerpPosition(tx.QueryRow(ctx, perpPositionSelect+`
		WHERE id = $1 AND status IN ('OPEN','LIQUIDATING') FOR UPDATE SKIP LOCKED`, plan.PositionID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return PerpLiquidationResult{}, ErrOrderClaimed
		}
		return PerpLiquidationResult{}, err
	}
	if p.Contracts != plan.ExpectContracts || plan.CloseContracts <= 0 || plan.CloseContracts > p.Contracts {
		return PerpLiquidationResult{}, ErrPlanStale
	}

	var insurance, liquidity int64
	if err := tx.QueryRow(ctx, `SELECT capital_usdx FROM perp_pools WHERE id = 'insurance' FOR UPDATE`).Scan(&insurance); err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: lock insurance: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT capital_usdx FROM perp_pools WHERE id = 'liquidity' FOR UPDATE`).Scan(&liquidity); err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: lock liquidity: %w", err)
	}

	order := PerpOrder{
		OwnerDID: p.OwnerDID, ActingDID: plan.ActingDID, MarketSymbol: p.MarketSymbol,
		Side: closingSide(p.Side), OrderType: "MARKET", Contracts: plan.CloseContracts,
		TimeInForce: "IOC", ReduceOnly: true, IdempotencyKey: plan.IdempotencyKey,
		Status:     "FILLED",
		SnapshotID: plan.Ref.SnapshotID, OrderbookSeq: plan.Ref.OrderbookSeq,
		StatsSeq: plan.Ref.StatsSeq, SourceTimestampMs: plan.Ref.SourceTimestampMs,
	}
	order, _, err = s.insertPerpOrderTx(ctx, tx, order)
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return PerpLiquidationResult{}, ErrOrderClaimed
		}
		return PerpLiquidationResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_orders SET filled_contracts = $2, updated_at = now() WHERE id = $1`, order.ID, plan.CloseContracts); err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: record liquidation fill count: %w", err)
	}

	res := PerpLiquidationResult{OrderID: order.ID}
	margin := p.MarginMicro

	fee := plan.Fill.FeeMicro
	if fee > margin {
		fee = margin
	}
	margin -= fee
	liquidity += fee

	if plan.RealizedPnLMicro >= 0 {
		pay := plan.RealizedPnLMicro
		if pay > liquidity {
			res.DeficitMicro = pay - liquidity
			pay = liquidity
		}
		liquidity -= pay
		margin += pay
		res.PoolPaidMicro = pay
	} else {
		loss := -plan.RealizedPnLMicro
		fromMargin := loss
		if fromMargin > margin {
			fromMargin = margin
		}
		margin -= fromMargin
		liquidity += fromMargin
		res.MarginAbsorbedMicro = fromMargin
		remaining := loss - fromMargin
		fromInsurance := remaining
		if fromInsurance > insurance {
			fromInsurance = insurance
		}
		insurance -= fromInsurance
		liquidity += fromInsurance
		res.InsurancePaidMicro = fromInsurance
		res.DeficitMicro = remaining - fromInsurance
	}

	if _, err := tx.Exec(ctx, `UPDATE perp_pools SET capital_usdx = $1, updated_at = now() WHERE id = 'insurance'`, insurance); err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: settle insurance: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_pools SET capital_usdx = $1, updated_at = now() WHERE id = 'liquidity'`, liquidity); err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: settle liquidity: %w", err)
	}

	if plan.FullClose {
		if margin > 0 {
			if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, p.OwnerDID, margin); err != nil {
				return PerpLiquidationResult{}, fmt.Errorf("store: return liquidation margin: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE perp_positions SET contracts = 0, margin_usdx = 0, status = 'CLOSED',
			       realized_pnl_usdx = realized_pnl_usdx + $2, closed_at = now(), updated_at = now()
			WHERE id = $1`, p.ID, plan.RealizedPnLMicro); err != nil {
			return PerpLiquidationResult{}, fmt.Errorf("store: close liquidated position: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE perp_positions SET contracts = contracts - $2, margin_usdx = $3, status = 'OPEN',
			       realized_pnl_usdx = realized_pnl_usdx + $4, updated_at = now()
			WHERE id = $1`, p.ID, plan.CloseContracts, margin, plan.RealizedPnLMicro); err != nil {
			return PerpLiquidationResult{}, fmt.Errorf("store: reduce liquidated position: %w", err)
		}
	}

	fillID, _, err := s.insertPerpFillTx(ctx, tx, order, p.ID, plan.Fill, plan.RealizedPnLMicro)
	if err != nil {
		return PerpLiquidationResult{}, err
	}
	res.FillID = fillID

	if err := tx.QueryRow(ctx, `
		INSERT INTO perp_liquidations (
			position_id, owner_did, market_symbol, closed_contracts, price_cents, fee_usdx,
			margin_absorbed_usdx, insurance_paid_usdx, pool_paid_usdx, deficit_usdx,
			snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id::text`,
		p.ID, p.OwnerDID, p.MarketSymbol, plan.CloseContracts, plan.Fill.PriceCents, fee,
		res.MarginAbsorbedMicro, res.InsurancePaidMicro, res.PoolPaidMicro, res.DeficitMicro,
		plan.Ref.SnapshotID, plan.Ref.OrderbookSeq, plan.Ref.StatsSeq, plan.Ref.SourceTimestampMs).
		Scan(&res.LiquidationID); err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: insert liquidation row: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, p.OwnerDID, plan.ActingDID, "position.liquidated", map[string]any{
		"liquidation_id": res.LiquidationID, "position_id": p.ID, "closed_contracts": plan.CloseContracts,
		"deficit_usdx": res.DeficitMicro, "closed": plan.FullClose,
	}); err != nil {
		return PerpLiquidationResult{}, err
	}

	if res.DeficitMicro > 0 {
		if err := s.pauseAllPerpMarketsTx(ctx, tx, plan.ActingDID, "perps.insolvency", res.DeficitMicro); err != nil {
			return PerpLiquidationResult{}, err
		}
		res.Paused = true
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpLiquidationResult{}, fmt.Errorf("store: commit liquidation: %w", err)
	}
	return res, nil
}

func closingSide(positionSide string) string {
	if positionSide == "LONG" {
		return "SELL"
	}
	return "BUY"
}

func (s *Store) pauseAllPerpMarketsTx(ctx context.Context, tx pgx.Tx, actingDID, cause string, deficitMicro int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE perp_markets SET mode = 'PAUSED', paused_cause = $1, updated_at = now()
		WHERE mode <> 'PAUSED'`, cause); err != nil {
		return fmt.Errorf("store: pause all markets: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, "", actingDID, "perps.insolvency", map[string]any{
		"cause": cause, "deficit_usdx": deficitMicro,
	}); err != nil {
		return err
	}
	return nil
}

// PauseAllPerpMarkets pauses every market with a recorded cause (the
// reconciliation worker's fail-closed response to an identity breach).
func (s *Store) PauseAllPerpMarkets(ctx context.Context, actingDID, cause string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin pause all: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.pauseAllPerpMarketsTx(ctx, tx, actingDID, cause, 0); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListOpenPerpPositions returns every OPEN/LIQUIDATING position, oldest first
// (the funding and liquidation workers' scan set).
func (s *Store) ListOpenPerpPositions(ctx context.Context, limit int) ([]PerpPosition, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1_000
	}
	rows, err := s.pool.Query(ctx, perpPositionSelect+`
		WHERE status IN ('OPEN','LIQUIDATING') ORDER BY opened_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list open positions: %w", err)
	}
	defer rows.Close()
	var out []PerpPosition
	for rows.Next() {
		p, err := scanPerpPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListRestingPerpOrders returns RESTING orders for the trigger worker, oldest
// first. Claiming happens inside ExecutePerpIntent via FOR UPDATE SKIP LOCKED.
func (s *Store) ListRestingPerpOrders(ctx context.Context, symbol string, limit int) ([]PerpOrder, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1_000
	}
	rows, err := s.pool.Query(ctx, perpOrderSelect+`
		WHERE status = 'RESTING' AND ($1 = '' OR market_symbol = $1)
		ORDER BY created_at ASC LIMIT $2`, symbol, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list resting orders: %w", err)
	}
	defer rows.Close()
	var out []PerpOrder
	for rows.Next() {
		o, err := scanPerpOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
