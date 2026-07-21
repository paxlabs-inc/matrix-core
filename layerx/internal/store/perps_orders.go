package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrOrderTerminal is returned when a transition targets an order already in a
// terminal state (no transition leaves a terminal state).
var ErrOrderTerminal = errors.New("store: order is terminal")

// ErrIdempotencyConflict is returned when (owner_did, idempotency_key) already
// exists for a different request.
var ErrIdempotencyConflict = errors.New("store: idempotency key conflicts with a different request")

// ErrIdempotencyInFlight is returned when a retry arrives while the original
// request is still executing.
var ErrIdempotencyInFlight = errors.New("store: original request is still in flight")

var perpOrderTerminal = map[string]bool{
	"SHADOW_RECORDED": true, "REJECTED": true, "FILLED": true, "CANCELLED": true, "EXPIRED": true,
}

// PerpOrder is one order row.
type PerpOrder struct {
	ID                string
	OwnerDID          string
	ActingDID         string
	MarketSymbol      string
	Side              string
	OrderType         string
	Contracts         int64
	FilledContracts   int64
	LimitPriceCents   int64
	StopPriceCents    int64
	TimeInForce       string
	ReduceOnly        bool
	ClientOrderID     string
	IdempotencyKey    string
	Status            string
	SnapshotID        string
	OrderbookSeq      int64
	StatsSeq          int64
	SourceTimestampMs int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const perpOrderSelect = `
	SELECT id::text, owner_did, acting_did, market_symbol, side, order_type,
	       contracts, filled_contracts, COALESCE(limit_price_cents, 0), COALESCE(stop_price_cents, 0),
	       time_in_force, reduce_only, COALESCE(client_order_id, ''), idempotency_key, status,
	       COALESCE(snapshot_id, ''), COALESCE(orderbook_seq, 0), COALESCE(stats_seq, 0),
	       COALESCE(source_timestamp_ms, 0), created_at, updated_at
	FROM perp_orders`

func scanPerpOrder(row pgx.Row) (PerpOrder, error) {
	var o PerpOrder
	err := row.Scan(&o.ID, &o.OwnerDID, &o.ActingDID, &o.MarketSymbol, &o.Side, &o.OrderType,
		&o.Contracts, &o.FilledContracts, &o.LimitPriceCents, &o.StopPriceCents,
		&o.TimeInForce, &o.ReduceOnly, &o.ClientOrderID, &o.IdempotencyKey, &o.Status,
		&o.SnapshotID, &o.OrderbookSeq, &o.StatsSeq, &o.SourceTimestampMs, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpOrder{}, ErrNotFound
		}
		return PerpOrder{}, fmt.Errorf("store: scan perp order: %w", err)
	}
	return o, nil
}

// InsertPerpOrder persists a new order row and appends its order.accepted
// event; the unique (owner_did, idempotency_key) constraint maps a duplicate
// intent to ErrIdempotencyConflict instead of a second order.
func (s *Store) InsertPerpOrder(ctx context.Context, o PerpOrder) (PerpOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpOrder{}, fmt.Errorf("store: begin order insert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inserted, _, err := s.insertPerpOrderTx(ctx, tx, o)
	if err != nil {
		return PerpOrder{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpOrder{}, fmt.Errorf("store: commit order insert: %w", err)
	}
	return inserted, nil
}

func (s *Store) insertPerpOrderTx(ctx context.Context, tx pgx.Tx, o PerpOrder) (PerpOrder, int64, error) {
	if o.Status == "" {
		o.Status = "RECEIVED"
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO perp_orders (
			owner_did, acting_did, market_symbol, side, order_type, contracts,
			limit_price_cents, stop_price_cents, time_in_force, reduce_only,
			client_order_id, idempotency_key, status,
			snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms)
		VALUES ($1,$2,$3,$4,$5,$6, NULLIF($7::bigint,0), NULLIF($8::bigint,0), $9,$10, NULLIF($11,''), $12, $13,
		        NULLIF($14,''), NULLIF($15::bigint,0), NULLIF($16::bigint,0), NULLIF($17::bigint,0))
		ON CONFLICT (owner_did, idempotency_key) DO NOTHING
		RETURNING id::text, created_at, updated_at`,
		o.OwnerDID, o.ActingDID, o.MarketSymbol, o.Side, o.OrderType, o.Contracts,
		o.LimitPriceCents, o.StopPriceCents, o.TimeInForce, o.ReduceOnly,
		o.ClientOrderID, o.IdempotencyKey, o.Status,
		o.SnapshotID, o.OrderbookSeq, o.StatsSeq, o.SourceTimestampMs)
	if err := row.Scan(&o.ID, &o.CreatedAt, &o.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpOrder{}, 0, ErrIdempotencyConflict
		}
		return PerpOrder{}, 0, fmt.Errorf("store: insert perp order: %w", err)
	}
	seq, err := appendPerpEvent(ctx, tx, o.OwnerDID, o.ActingDID, "order.accepted", map[string]any{
		"order_id": o.ID, "symbol": o.MarketSymbol, "side": o.Side, "type": o.OrderType,
		"contracts": o.Contracts, "status": o.Status,
	})
	if err != nil {
		return PerpOrder{}, 0, err
	}
	return o, seq, nil
}

// GetPerpOrder returns one order by id, or ErrNotFound.
func (s *Store) GetPerpOrder(ctx context.Context, id string) (PerpOrder, error) {
	return scanPerpOrder(s.pool.QueryRow(ctx, perpOrderSelect+` WHERE id = $1`, id))
}

// GetPerpOrderByIdempotency returns the order created under this exactly-once
// identity, or ErrNotFound.
func (s *Store) GetPerpOrderByIdempotency(ctx context.Context, ownerDID, key string) (PerpOrder, error) {
	return scanPerpOrder(s.pool.QueryRow(ctx,
		perpOrderSelect+` WHERE owner_did = $1 AND idempotency_key = $2`, ownerDID, key))
}

// ListPerpOrders returns the owner's orders newest first, optionally filtered
// by status and market.
func (s *Store) ListPerpOrders(ctx context.Context, ownerDID, status, symbol string, limit int) ([]PerpOrder, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, perpOrderSelect+`
		WHERE owner_did = $1
		  AND ($2 = '' OR status = $2)
		  AND ($3 = '' OR market_symbol = $3)
		ORDER BY created_at DESC LIMIT $4`, ownerDID, status, symbol, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list perp orders: %w", err)
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

// UpdatePerpOrderStatus transitions an order, rejecting any transition out of a
// terminal state, and journals order.updated.
func (s *Store) UpdatePerpOrderStatus(ctx context.Context, id, newStatus, actingDID string) (PerpOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpOrder{}, fmt.Errorf("store: begin order status: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o, err := scanPerpOrder(tx.QueryRow(ctx, perpOrderSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return PerpOrder{}, err
	}
	if perpOrderTerminal[o.Status] {
		return PerpOrder{}, ErrOrderTerminal
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_orders SET status = $2, updated_at = now() WHERE id = $1`, id, newStatus); err != nil {
		return PerpOrder{}, fmt.Errorf("store: update order status: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, o.OwnerDID, actingDID, "order.updated", map[string]any{
		"order_id": id, "from": o.Status, "to": newStatus,
	}); err != nil {
		return PerpOrder{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpOrder{}, fmt.Errorf("store: commit order status: %w", err)
	}
	o.Status = newStatus
	return o, nil
}

// ClaimPerpIdempotency claims (owner_did, idempotency_key) for one operation.
// The first caller gets claimed=true and must later call
// CompletePerpIdempotency. A retry of the SAME request (matching hash) returns
// the stored response, or ErrIdempotencyInFlight while the original is still
// executing. A different request under the same key is ErrIdempotencyConflict.
func (s *Store) ClaimPerpIdempotency(ctx context.Context, ownerDID, key, operation, requestHash string) (response []byte, claimed bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO perp_idempotency (owner_did, idempotency_key, operation, request_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner_did, idempotency_key) DO NOTHING`, ownerDID, key, operation, requestHash)
	if err != nil {
		return nil, false, fmt.Errorf("store: claim idempotency: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil, true, nil
	}
	var storedOp, storedHash, status string
	var stored []byte
	err = s.pool.QueryRow(ctx, `
		SELECT operation, request_hash, status, COALESCE(response, 'null'::jsonb)
		FROM perp_idempotency WHERE owner_did = $1 AND idempotency_key = $2`, ownerDID, key).
		Scan(&storedOp, &storedHash, &status, &stored)
	if err != nil {
		return nil, false, fmt.Errorf("store: read idempotency: %w", err)
	}
	if storedOp != operation || storedHash != requestHash {
		return nil, false, ErrIdempotencyConflict
	}
	if status != "done" {
		return nil, false, ErrIdempotencyInFlight
	}
	return stored, false, nil
}

// CompletePerpIdempotency records the original result for every future retry.
func (s *Store) CompletePerpIdempotency(ctx context.Context, ownerDID, key string, response []byte) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE perp_idempotency SET status = 'done', response = $3, updated_at = now()
		WHERE owner_did = $1 AND idempotency_key = $2`, ownerDID, key, response)
	if err != nil {
		return fmt.Errorf("store: complete idempotency: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
