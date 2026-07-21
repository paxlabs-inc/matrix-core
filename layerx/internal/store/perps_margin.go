package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrReservationClosed is returned when a lifecycle op targets a margin
// reservation that already left 'held'.
var ErrReservationClosed = errors.New("store: margin reservation is not held")

// PerpMarginReservation is user initial margin held out of the spendable
// balance for an in-flight/resting order.
type PerpMarginReservation struct {
	ID          string
	OwnerDID    string
	OrderID     string
	AmountMicro int64
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const perpReservationSelect = `
	SELECT id::text, owner_did, order_id::text, amount_usdx, status, created_at, updated_at
	FROM perp_margin_reservations`

func scanPerpReservation(row pgx.Row) (PerpMarginReservation, error) {
	var r PerpMarginReservation
	err := row.Scan(&r.ID, &r.OwnerDID, &r.OrderID, &r.AmountMicro, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpMarginReservation{}, ErrNotFound
		}
		return PerpMarginReservation{}, fmt.Errorf("store: scan margin reservation: %w", err)
	}
	return r, nil
}

// ReservePerpMargin moves amountMicro from the owner's spendable balance into
// a held reservation for orderID — the value stays circulating in the
// user-perps-margin bucket. Fails closed on insufficient spendable balance.
func (s *Store) ReservePerpMargin(ctx context.Context, ownerDID, orderID string, amountMicro int64) (PerpMarginReservation, error) {
	if amountMicro <= 0 {
		return PerpMarginReservation{}, fmt.Errorf("store: reservation amount must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: begin margin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bal int64
	err = tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, ownerDID).Scan(&bal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpMarginReservation{}, ErrNotFound
		}
		return PerpMarginReservation{}, fmt.Errorf("store: lock margin owner: %w", err)
	}
	if bal < amountMicro {
		return PerpMarginReservation{}, ErrInsufficientFunds
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, ownerDID, amountMicro); err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: debit margin owner: %w", err)
	}
	r, err := scanPerpReservation(tx.QueryRow(ctx, `
		INSERT INTO perp_margin_reservations (owner_did, order_id, amount_usdx)
		VALUES ($1, $2, $3)
		RETURNING id::text, owner_did, order_id::text, amount_usdx, status, created_at, updated_at`,
		ownerDID, orderID, amountMicro))
	if err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: insert margin reservation: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, ownerDID, ownerDID, "balance.updated", map[string]any{
		"reason": "margin.reserved", "reservation_id": r.ID, "order_id": orderID, "amount_usdx": amountMicro,
	}); err != nil {
		return PerpMarginReservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: commit margin reserve: %w", err)
	}
	return r, nil
}

// HeldReservationForOrder returns the order's held reservation, or ErrNotFound.
func (s *Store) HeldReservationForOrder(ctx context.Context, orderID string) (PerpMarginReservation, error) {
	return scanPerpReservation(s.pool.QueryRow(ctx,
		perpReservationSelect+` WHERE order_id = $1 AND status = 'held'`, orderID))
}

// GetPerpMarginReservation returns one reservation, or ErrNotFound.
func (s *Store) GetPerpMarginReservation(ctx context.Context, id string) (PerpMarginReservation, error) {
	return scanPerpReservation(s.pool.QueryRow(ctx, perpReservationSelect+` WHERE id = $1`, id))
}

// ReleasePerpMarginReservation returns a held reservation to the owner's
// spendable balance. Idempotent for already-released rows; an applied
// reservation is ErrReservationClosed.
func (s *Store) ReleasePerpMarginReservation(ctx context.Context, id string) (PerpMarginReservation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: begin margin release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r, err := scanPerpReservation(tx.QueryRow(ctx, perpReservationSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return PerpMarginReservation{}, err
	}
	switch r.Status {
	case "released":
		return r, nil
	case "applied":
		return PerpMarginReservation{}, ErrReservationClosed
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, r.OwnerDID, r.AmountMicro); err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: return reserved margin: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_margin_reservations SET status = 'released', updated_at = now() WHERE id = $1`, id); err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: release margin reservation: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, r.OwnerDID, r.OwnerDID, "balance.updated", map[string]any{
		"reason": "margin.released", "reservation_id": r.ID, "amount_usdx": r.AmountMicro,
	}); err != nil {
		return PerpMarginReservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpMarginReservation{}, fmt.Errorf("store: commit margin release: %w", err)
	}
	r.Status = "released"
	return r, nil
}
