package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PerpFill is one immutable execution record with its exact snapshot reference.
type PerpFill struct {
	ID               string
	OrderID          string
	PositionID       string
	OwnerDID         string
	ActingDID        string
	MarketSymbol     string
	Side             string
	Contracts        int64
	PriceCents       int64
	NotionalMicro    int64
	FeeMicro         int64
	RealizedPnLMicro int64
	Maker            bool
	Liquidation      bool
	Ref              PerpSnapshotRef
	CreatedAt        time.Time
}

const perpFillSelect = `
	SELECT id::text, order_id::text, position_id::text, owner_did, acting_did, market_symbol,
	       side, contracts, price_cents, notional_usdx, fee_usdx, realized_pnl_usdx,
	       maker, liquidation, snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms, created_at
	FROM perp_fills`

func scanPerpFill(row pgx.Row) (PerpFill, error) {
	var f PerpFill
	err := row.Scan(&f.ID, &f.OrderID, &f.PositionID, &f.OwnerDID, &f.ActingDID, &f.MarketSymbol,
		&f.Side, &f.Contracts, &f.PriceCents, &f.NotionalMicro, &f.FeeMicro, &f.RealizedPnLMicro,
		&f.Maker, &f.Liquidation, &f.Ref.SnapshotID, &f.Ref.OrderbookSeq, &f.Ref.StatsSeq,
		&f.Ref.SourceTimestampMs, &f.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpFill{}, ErrNotFound
		}
		return PerpFill{}, fmt.Errorf("store: scan perp fill: %w", err)
	}
	return f, nil
}

// InsertPerpFill persists one fill and journals fill.created. The snapshot
// reference is mandatory — a fill without its exact market-data identity is
// rejected.
func (s *Store) InsertPerpFill(ctx context.Context, f PerpFill) (PerpFill, error) {
	if f.Ref.SnapshotID == "" || f.Ref.OrderbookSeq <= 0 || f.Ref.StatsSeq <= 0 || f.Ref.SourceTimestampMs <= 0 {
		return PerpFill{}, fmt.Errorf("store: fill requires a complete snapshot reference")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpFill{}, fmt.Errorf("store: begin fill insert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO perp_fills (
			order_id, position_id, owner_did, acting_did, market_symbol, side, contracts,
			price_cents, notional_usdx, fee_usdx, realized_pnl_usdx, maker, liquidation,
			snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING id::text, created_at`,
		f.OrderID, f.PositionID, f.OwnerDID, f.ActingDID, f.MarketSymbol, f.Side, f.Contracts,
		f.PriceCents, f.NotionalMicro, f.FeeMicro, f.RealizedPnLMicro, f.Maker, f.Liquidation,
		f.Ref.SnapshotID, f.Ref.OrderbookSeq, f.Ref.StatsSeq, f.Ref.SourceTimestampMs)
	if err := row.Scan(&f.ID, &f.CreatedAt); err != nil {
		return PerpFill{}, fmt.Errorf("store: insert perp fill: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, f.OwnerDID, f.ActingDID, "fill.created", map[string]any{
		"fill_id": f.ID, "order_id": f.OrderID, "position_id": f.PositionID,
		"symbol": f.MarketSymbol, "side": f.Side, "contracts": f.Contracts,
		"price_cents": f.PriceCents, "notional_usdx": f.NotionalMicro, "fee_usdx": f.FeeMicro,
		"snapshot_id": f.Ref.SnapshotID, "orderbook_seq": f.Ref.OrderbookSeq,
	}); err != nil {
		return PerpFill{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpFill{}, fmt.Errorf("store: commit fill insert: %w", err)
	}
	return f, nil
}

// ListPerpFills returns the owner's fills newest first, optionally filtered by
// market symbol.
func (s *Store) ListPerpFills(ctx context.Context, ownerDID, symbol string, limit int) ([]PerpFill, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, perpFillSelect+`
		WHERE owner_did = $1 AND ($2 = '' OR market_symbol = $2)
		ORDER BY created_at DESC LIMIT $3`, ownerDID, symbol, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list perp fills: %w", err)
	}
	defer rows.Close()
	var out []PerpFill
	for rows.Next() {
		f, err := scanPerpFill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
