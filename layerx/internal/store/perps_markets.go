package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/paxlabs-inc/layerx/internal/perps/market"
)

// ErrMarketPauseHeld is returned when a mode change would leave PAUSED without
// the caller explicitly clearing the recorded pause cause (the emergency PAUSED
// transition is monotonic until an operator clears it).
var ErrMarketPauseHeld = errors.New("store: market is paused; clearing requires an explicit cause reset")

// PerpMarketRow is the durable market registry row plus its runtime mode.
type PerpMarketRow struct {
	market.Market
	Mode        string
	PausedCause string
	UpdatedAt   time.Time
}

// SyncPerpMarkets upserts the source-locked registry parameters for every
// market, preserving each row's runtime mode. Boot calls this so the DB always
// mirrors the compiled registry (the single source of truth for parameters).
func (s *Store) SyncPerpMarkets(ctx context.Context, markets []market.Market) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin market sync: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, m := range markets {
		if _, err := tx.Exec(ctx, `
			INSERT INTO perp_markets (
				symbol, class, tick_price_units, min_order_contracts, min_position_contracts,
				initial_margin_bps, maintenance_margin_bps, max_leverage_x,
				max_position_usdx, max_protocol_oi_usdx, maker_fee_bps, taker_fee_bps,
				liquidation_fee_bps, base_spread_bps, max_skew_impact_bps,
				divergence_limit_bps, stress_loss_bps, session)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			ON CONFLICT (symbol) DO UPDATE SET
				class = EXCLUDED.class,
				tick_price_units = EXCLUDED.tick_price_units,
				min_order_contracts = EXCLUDED.min_order_contracts,
				min_position_contracts = EXCLUDED.min_position_contracts,
				initial_margin_bps = EXCLUDED.initial_margin_bps,
				maintenance_margin_bps = EXCLUDED.maintenance_margin_bps,
				max_leverage_x = EXCLUDED.max_leverage_x,
				max_position_usdx = EXCLUDED.max_position_usdx,
				max_protocol_oi_usdx = EXCLUDED.max_protocol_oi_usdx,
				maker_fee_bps = EXCLUDED.maker_fee_bps,
				taker_fee_bps = EXCLUDED.taker_fee_bps,
				liquidation_fee_bps = EXCLUDED.liquidation_fee_bps,
				base_spread_bps = EXCLUDED.base_spread_bps,
				max_skew_impact_bps = EXCLUDED.max_skew_impact_bps,
				divergence_limit_bps = EXCLUDED.divergence_limit_bps,
				stress_loss_bps = EXCLUDED.stress_loss_bps,
				session = EXCLUDED.session,
				updated_at = now()`,
			m.Symbol, m.Class, m.TickPriceUnits, m.MinOrderContracts, m.MinPositionContracts,
			m.InitialMarginBps, m.MaintenanceMarginBps, m.MaxLeverageX,
			m.MaxPositionMicroUSDX, m.MaxProtocolOIMicroUSDX, m.MakerFeeBps, m.TakerFeeBps,
			m.LiquidationFeeBps, m.BaseSpreadBps, m.MaxSkewImpactBps,
			m.DivergenceLimitBps, m.StressLossBps, string(m.Session)); err != nil {
			return fmt.Errorf("store: sync perp market %s: %w", m.Symbol, err)
		}
	}
	return tx.Commit(ctx)
}

const perpMarketSelect = `
	SELECT symbol, class, tick_price_units, min_order_contracts, min_position_contracts,
	       initial_margin_bps, maintenance_margin_bps, max_leverage_x,
	       max_position_usdx, max_protocol_oi_usdx, maker_fee_bps, taker_fee_bps,
	       liquidation_fee_bps, base_spread_bps, max_skew_impact_bps,
	       divergence_limit_bps, stress_loss_bps, session, mode, COALESCE(paused_cause,''), updated_at
	FROM perp_markets`

func scanPerpMarket(row pgx.Row) (PerpMarketRow, error) {
	var r PerpMarketRow
	var session string
	err := row.Scan(&r.Symbol, &r.Class, &r.TickPriceUnits, &r.MinOrderContracts, &r.MinPositionContracts,
		&r.InitialMarginBps, &r.MaintenanceMarginBps, &r.MaxLeverageX,
		&r.MaxPositionMicroUSDX, &r.MaxProtocolOIMicroUSDX, &r.MakerFeeBps, &r.TakerFeeBps,
		&r.LiquidationFeeBps, &r.BaseSpreadBps, &r.MaxSkewImpactBps,
		&r.DivergenceLimitBps, &r.StressLossBps, &session, &r.Mode, &r.PausedCause, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpMarketRow{}, ErrNotFound
		}
		return PerpMarketRow{}, fmt.Errorf("store: scan perp market: %w", err)
	}
	r.Session = market.Session(session)
	return r, nil
}

// GetPerpMarket returns one market row, or ErrNotFound.
func (s *Store) GetPerpMarket(ctx context.Context, symbol string) (PerpMarketRow, error) {
	return scanPerpMarket(s.pool.QueryRow(ctx, perpMarketSelect+` WHERE symbol = $1`, symbol))
}

// ListPerpMarkets returns every market row in symbol order.
func (s *Store) ListPerpMarkets(ctx context.Context) ([]PerpMarketRow, error) {
	rows, err := s.pool.Query(ctx, perpMarketSelect+` ORDER BY symbol ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list perp markets: %w", err)
	}
	defer rows.Close()
	var out []PerpMarketRow
	for rows.Next() {
		r, err := scanPerpMarket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetPerpMarketMode durably changes one market's runtime mode and appends the
// market.mode_changed journal event in the same transaction. An existing
// PAUSED mode is monotonic: leaving it requires clearPause=true, which also
// resets the recorded cause. Entering PAUSED requires a non-empty cause.
func (s *Store) SetPerpMarketMode(ctx context.Context, symbol, newMode, cause, actingDID string, clearPause bool) (PerpMarketRow, error) {
	if newMode == "PAUSED" && cause == "" {
		return PerpMarketRow{}, fmt.Errorf("store: pausing %s requires a cause", symbol)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpMarketRow{}, fmt.Errorf("store: begin mode change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := scanPerpMarket(tx.QueryRow(ctx, perpMarketSelect+` WHERE symbol = $1 FOR UPDATE`, symbol))
	if err != nil {
		return PerpMarketRow{}, err
	}
	if cur.Mode == "PAUSED" && newMode != "PAUSED" && !clearPause {
		return PerpMarketRow{}, ErrMarketPauseHeld
	}
	nextCause := cur.PausedCause
	if newMode == "PAUSED" {
		nextCause = cause
	} else if clearPause {
		nextCause = ""
	}
	if _, err := tx.Exec(ctx, `
		UPDATE perp_markets SET mode = $2, paused_cause = NULLIF($3,''), updated_at = now()
		WHERE symbol = $1`, symbol, newMode, nextCause); err != nil {
		return PerpMarketRow{}, fmt.Errorf("store: update perp market mode: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, "", actingDID, "market.mode_changed", map[string]any{
		"symbol": symbol, "from": cur.Mode, "to": newMode, "cause": cause,
	}); err != nil {
		return PerpMarketRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpMarketRow{}, fmt.Errorf("store: commit mode change: %w", err)
	}
	cur.Mode = newMode
	cur.PausedCause = nextCause
	return cur, nil
}

// SetAllPerpMarketModes applies a global emergency mode to every market in one
// transaction. Either all market rows and journal events change, or none do.
func (s *Store) SetAllPerpMarketModes(ctx context.Context, newMode, cause, actingDID string,
	clearPause bool) ([]PerpMarketRow, error) {
	if actingDID == "" {
		return nil, fmt.Errorf("store: global mode change requires an actor")
	}
	if newMode == "PAUSED" && cause == "" {
		return nil, fmt.Errorf("store: global pause requires a cause")
	}
	valid := map[string]bool{
		"OFF": true, "SHADOW": true, "CANARY": true,
		"ACTIVE": true, "REDUCE_ONLY": true, "PAUSED": true,
	}
	if !valid[newMode] {
		return nil, fmt.Errorf("store: perp mode %q is invalid", newMode)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin global mode change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, perpMarketSelect+` ORDER BY symbol FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("store: lock all perp markets: %w", err)
	}
	var current []PerpMarketRow
	for rows.Next() {
		row, err := scanPerpMarket(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if row.Mode == "PAUSED" && newMode != "PAUSED" && !clearPause {
			rows.Close()
			return nil, ErrMarketPauseHeld
		}
		current = append(current, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	nextCause := ""
	if newMode == "PAUSED" {
		nextCause = cause
	}
	if _, err := tx.Exec(ctx, `
		UPDATE perp_markets
		SET mode=$1, paused_cause=NULLIF($2,''), updated_at=now()`,
		newMode, nextCause); err != nil {
		return nil, fmt.Errorf("store: update all perp market modes: %w", err)
	}
	for i := range current {
		if _, err := appendPerpEvent(ctx, tx, "", actingDID, "market.mode_changed", map[string]any{
			"symbol": current[i].Symbol, "from": current[i].Mode, "to": newMode,
			"cause": cause, "global": true,
		}); err != nil {
			return nil, err
		}
		current[i].Mode = newMode
		current[i].PausedCause = nextCause
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit global mode change: %w", err)
	}
	return current, nil
}
