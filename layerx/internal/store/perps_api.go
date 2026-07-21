package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PerpAccount is the authenticated account view: every owner-scoped balance
// term plus the owner's durable private-event cursor.
type PerpAccount struct {
	OwnerDID              string
	SpendableMicro        int64
	PositionMarginMicro   int64
	OpenOrderMarginMicro  int64
	RealizedPnLMicro      int64
	UnsettledFundingMicro int64
	LastOwnerEventID      int64
}

// GetPerpAccount reads the owner's full perps account view in one consistent
// statement. An unfunded owner returns zeros, not ErrNotFound.
func (s *Store) GetPerpAccount(ctx context.Context, ownerDID string) (PerpAccount, error) {
	a := PerpAccount{OwnerDID: ownerDID}
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COALESCE(SUM(balance_usdx), 0) FROM accounts WHERE did = $1),
		       (SELECT COALESCE(SUM(margin_usdx), 0) FROM perp_positions WHERE owner_did = $1 AND status IN ('OPEN','LIQUIDATING')),
		       (SELECT COALESCE(SUM(amount_usdx), 0) FROM perp_margin_reservations WHERE owner_did = $1 AND status = 'held'),
		       (SELECT COALESCE(SUM(realized_pnl_usdx), 0) FROM perp_positions WHERE owner_did = $1),
		       (SELECT COALESCE(SUM(unsettled_funding_usdx), 0) FROM perp_positions WHERE owner_did = $1 AND status IN ('OPEN','LIQUIDATING')),
		       (SELECT COALESCE(MAX(owner_event_id), 0) FROM perp_events WHERE owner_did = $1)`, ownerDID).
		Scan(&a.SpendableMicro, &a.PositionMarginMicro, &a.OpenOrderMarginMicro,
			&a.RealizedPnLMicro, &a.UnsettledFundingMicro, &a.LastOwnerEventID); err != nil {
		return PerpAccount{}, fmt.Errorf("store: perp account: %w", err)
	}
	return a, nil
}

// ListPerpPositionsByOwner returns the owner's positions newest first. status
// "" returns open (OPEN/LIQUIDATING) positions only; an explicit status
// filters exactly.
func (s *Store) ListPerpPositionsByOwner(ctx context.Context, ownerDID, status string, limit int) ([]PerpPosition, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, perpPositionSelect+`
		WHERE owner_did = $1
		  AND (($2 = '' AND status IN ('OPEN','LIQUIDATING')) OR status = $2)
		ORDER BY opened_at DESC LIMIT $3`, ownerDID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list owner positions: %w", err)
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

// PerpMarketOpenInterestMicro is the market's internal open interest in
// micro-USDX notional, derived solely from committed LayerX positions
// (Crossverse OI is reference-only).
func (s *Store) PerpMarketOpenInterestMicro(ctx context.Context, symbol string) (int64, error) {
	var contracts int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(contracts), 0) FROM perp_positions
		WHERE market_symbol = $1 AND status IN ('OPEN','LIQUIDATING')`, symbol).Scan(&contracts); err != nil {
		return 0, fmt.Errorf("store: market open interest: %w", err)
	}
	return contracts * 10_000_000, nil
}

// HasPerpFundingEntry reports whether a funding interval is already settled
// for a position. The worker skips settled intervals BEFORE recomputing from
// the live rate: exactly-once means the first settlement wins, and the store's
// value-mismatch rejection stays reserved for genuine double-settle defects.
func (s *Store) HasPerpFundingEntry(ctx context.Context, positionID string, intervalStartMs int64) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `
		SELECT 1 FROM perp_funding_entries
		WHERE position_id = $1 AND interval_start_ms = $2`, positionID, intervalStartMs).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check funding entry: %w", err)
	}
	return true, nil
}

// GetPerpFill returns one fill by id, or ErrNotFound.
func (s *Store) GetPerpFill(ctx context.Context, id string) (PerpFill, error) {
	return scanPerpFill(s.pool.QueryRow(ctx, perpFillSelect+` WHERE id = $1`, id))
}

// LookupPerpIdempotency reads an exactly-once claim without creating one — the
// API's direct-signature retry path (verify signature, look up the key, and
// only a NEW key consumes the nonce).
func (s *Store) LookupPerpIdempotency(ctx context.Context, ownerDID, key string) (operation, requestHash, status string, response []byte, found bool, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT operation, request_hash, status, COALESCE(response, 'null'::jsonb)
		FROM perp_idempotency WHERE owner_did = $1 AND idempotency_key = $2`, ownerDID, key).
		Scan(&operation, &requestHash, &status, &response)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", nil, false, nil
	}
	if err != nil {
		return "", "", "", nil, false, fmt.Errorf("store: lookup idempotency: %w", err)
	}
	return operation, requestHash, status, response, true, nil
}

// claimPerpIdemTx claims (owner, key) inside the caller's transaction. A retry
// of the same request returns its stored response; a different request under
// the same key is ErrIdempotencyConflict; a still-executing original is
// ErrIdempotencyInFlight.
func claimPerpIdemTx(ctx context.Context, tx pgx.Tx, ownerDID, key, operation, requestHash string) (stored []byte, claimed bool, err error) {
	tag, err := tx.Exec(ctx, `
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
	err = tx.QueryRow(ctx, `
		SELECT operation, request_hash, status, COALESCE(response, 'null'::jsonb)
		FROM perp_idempotency WHERE owner_did = $1 AND idempotency_key = $2`, ownerDID, key).
		Scan(&storedOp, &storedHash, &status, &stored)
	if err != nil {
		return nil, false, fmt.Errorf("store: read idempotency claim: %w", err)
	}
	if storedOp != operation || storedHash != requestHash {
		return nil, false, ErrIdempotencyConflict
	}
	if status != "done" {
		return nil, false, ErrIdempotencyInFlight
	}
	return stored, false, nil
}

func completePerpIdemTx(ctx context.Context, tx pgx.Tx, ownerDID, key string, response []byte) error {
	if _, err := tx.Exec(ctx, `
		UPDATE perp_idempotency SET status = 'done', response = $3, updated_at = now()
		WHERE owner_did = $1 AND idempotency_key = $2`, ownerDID, key, response); err != nil {
		return fmt.Errorf("store: complete idempotency: %w", err)
	}
	return nil
}

// PerpCancelResult is one executed (or replayed) cancel.
type PerpCancelResult struct {
	Order         PerpOrder
	ReleasedMicro int64
	EventSeqLo    int64
	EventSeqHi    int64
	Replayed      bool
}

type perpCancelResponse struct {
	OrderID       string `json:"order_id"`
	ReleasedMicro int64  `json:"released_usdx"`
	EventSeqLo    int64  `json:"event_seq_lo,omitempty"`
	EventSeqHi    int64  `json:"event_seq_hi,omitempty"`
}

// CancelPerpOrder cancels one owner order atomically with exactly-once
// idempotency: the order row is locked, any held margin reservation is
// released back to the owner's spendable balance, and order.cancelled is
// journaled in the same transaction. Cancelling an already-CANCELLED order
// returns that terminal result; any other terminal state is ErrOrderTerminal.
// Cancel is never mode-gated: it stays available under every market mode.
func (s *Store) CancelPerpOrder(ctx context.Context, ownerDID, orderID, actingDID, idemKey, requestHash, reason string) (PerpCancelResult, error) {
	if reason == "" {
		reason = "user.cancel"
	}
	if ownerDID == "" || idemKey == "" {
		return PerpCancelResult{}, fmt.Errorf("store: cancel requires owner and idempotency key")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpCancelResult{}, fmt.Errorf("store: begin cancel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, claimed, err := claimPerpIdemTx(ctx, tx, ownerDID, idemKey, "perps.cancel", requestHash)
	if err != nil {
		return PerpCancelResult{}, err
	}
	if !claimed {
		var resp perpCancelResponse
		if err := json.Unmarshal(stored, &resp); err != nil {
			return PerpCancelResult{}, fmt.Errorf("store: decode cancel response: %w", err)
		}
		order, err := scanPerpOrder(tx.QueryRow(ctx, perpOrderSelect+` WHERE id = $1`, resp.OrderID))
		if err != nil {
			return PerpCancelResult{}, err
		}
		return PerpCancelResult{
			Order: order, ReleasedMicro: resp.ReleasedMicro,
			EventSeqLo: resp.EventSeqLo, EventSeqHi: resp.EventSeqHi, Replayed: true,
		}, nil
	}

	order, err := scanPerpOrder(tx.QueryRow(ctx,
		perpOrderSelect+` WHERE id = $1 AND owner_did = $2 FOR UPDATE`, orderID, ownerDID))
	if err != nil {
		return PerpCancelResult{}, err
	}
	result := PerpCancelResult{Order: order}
	if perpOrderTerminal[order.Status] {
		if order.Status != "CANCELLED" {
			return PerpCancelResult{}, ErrOrderTerminal
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE perp_orders SET status = 'CANCELLED', updated_at = now() WHERE id = $1`, orderID); err != nil {
			return PerpCancelResult{}, fmt.Errorf("store: cancel order: %w", err)
		}
		result.Order.Status = "CANCELLED"
		var released int64
		if err := tx.QueryRow(ctx, `
			WITH released AS (
				UPDATE perp_margin_reservations SET status = 'released', updated_at = now()
				WHERE order_id = $1 AND status = 'held'
				RETURNING amount_usdx
			)
			SELECT COALESCE(SUM(amount_usdx), 0) FROM released`, orderID).Scan(&released); err != nil {
			return PerpCancelResult{}, fmt.Errorf("store: release cancel reservations: %w", err)
		}
		if released > 0 {
			if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, ownerDID, released); err != nil {
				return PerpCancelResult{}, fmt.Errorf("store: return cancel margin: %w", err)
			}
		}
		result.ReleasedMicro = released
		seq, err := appendPerpEvent(ctx, tx, ownerDID, actingDID, "order.cancelled", map[string]any{
			"order_id": orderID, "reason": reason, "released_usdx": released,
		})
		if err != nil {
			return PerpCancelResult{}, err
		}
		result.EventSeqLo, result.EventSeqHi = seq, seq
	}

	resp, err := json.Marshal(perpCancelResponse{
		OrderID: orderID, ReleasedMicro: result.ReleasedMicro,
		EventSeqLo: result.EventSeqLo, EventSeqHi: result.EventSeqHi,
	})
	if err != nil {
		return PerpCancelResult{}, fmt.Errorf("store: encode cancel response: %w", err)
	}
	if err := completePerpIdemTx(ctx, tx, ownerDID, idemKey, resp); err != nil {
		return PerpCancelResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpCancelResult{}, fmt.Errorf("store: commit cancel: %w", err)
	}
	return result, nil
}

// PerpMarginAdjustResult is one executed (or replayed) margin adjustment.
type PerpMarginAdjustResult struct {
	Position   PerpPosition
	EventSeqLo int64
	EventSeqHi int64
	Replayed   bool
}

type perpMarginResponse struct {
	PositionID string `json:"position_id"`
	DeltaMicro int64  `json:"delta_usdx"`
	EventSeqLo int64  `json:"event_seq_lo,omitempty"`
	EventSeqHi int64  `json:"event_seq_hi,omitempty"`
}

// AdjustPerpPositionMargin moves margin between the owner's spendable balance
// and an open position atomically with exactly-once idempotency. Positive
// deltaMicro adds margin (bounded by the spendable balance); negative removes
// margin back to the balance and fails closed unless the remaining margin
// stays at or above minRemainingMicro (the engine-computed equity floor).
func (s *Store) AdjustPerpPositionMargin(ctx context.Context, ownerDID, positionID, actingDID string,
	deltaMicro, minRemainingMicro int64, idemKey, requestHash string) (PerpMarginAdjustResult, error) {

	if ownerDID == "" || idemKey == "" {
		return PerpMarginAdjustResult{}, fmt.Errorf("store: margin adjust requires owner and idempotency key")
	}
	if deltaMicro == 0 {
		return PerpMarginAdjustResult{}, fmt.Errorf("store: margin adjust amount must be nonzero")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpMarginAdjustResult{}, fmt.Errorf("store: begin margin adjust: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, claimed, err := claimPerpIdemTx(ctx, tx, ownerDID, idemKey, "perps.margin", requestHash)
	if err != nil {
		return PerpMarginAdjustResult{}, err
	}
	if !claimed {
		var resp perpMarginResponse
		if err := json.Unmarshal(stored, &resp); err != nil {
			return PerpMarginAdjustResult{}, fmt.Errorf("store: decode margin response: %w", err)
		}
		p, err := scanPerpPosition(tx.QueryRow(ctx, perpPositionSelect+` WHERE id = $1`, resp.PositionID))
		if err != nil {
			return PerpMarginAdjustResult{}, err
		}
		return PerpMarginAdjustResult{Position: p, EventSeqLo: resp.EventSeqLo, EventSeqHi: resp.EventSeqHi, Replayed: true}, nil
	}

	p, err := scanPerpPosition(tx.QueryRow(ctx,
		perpPositionSelect+` WHERE id = $1 AND owner_did = $2 FOR UPDATE`, positionID, ownerDID))
	if err != nil {
		return PerpMarginAdjustResult{}, err
	}
	if p.Status == "CLOSED" {
		return PerpMarginAdjustResult{}, ErrPositionClosed
	}
	var bal int64
	if err := tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, ownerDID).Scan(&bal); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpMarginAdjustResult{}, ErrNotFound
		}
		return PerpMarginAdjustResult{}, fmt.Errorf("store: lock margin account: %w", err)
	}
	reason := "margin.added"
	if deltaMicro > 0 {
		if bal < deltaMicro {
			return PerpMarginAdjustResult{}, ErrInsufficientFunds
		}
	} else {
		reason = "margin.removed"
		remove := -deltaMicro
		if p.MarginMicro-remove < minRemainingMicro || p.MarginMicro < remove {
			return PerpMarginAdjustResult{}, ErrMarginInsufficient
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, ownerDID, deltaMicro); err != nil {
		return PerpMarginAdjustResult{}, fmt.Errorf("store: move margin balance: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_positions SET margin_usdx = margin_usdx + $2, updated_at = now() WHERE id = $1`, positionID, deltaMicro); err != nil {
		return PerpMarginAdjustResult{}, fmt.Errorf("store: move position margin: %w", err)
	}
	seq, err := appendPerpEvent(ctx, tx, ownerDID, actingDID, "position.updated", map[string]any{
		"reason": reason, "position_id": positionID, "amount_usdx": deltaMicro,
	})
	if err != nil {
		return PerpMarginAdjustResult{}, err
	}
	resp, err := json.Marshal(perpMarginResponse{
		PositionID: positionID, DeltaMicro: deltaMicro, EventSeqLo: seq, EventSeqHi: seq,
	})
	if err != nil {
		return PerpMarginAdjustResult{}, fmt.Errorf("store: encode margin response: %w", err)
	}
	if err := completePerpIdemTx(ctx, tx, ownerDID, idemKey, resp); err != nil {
		return PerpMarginAdjustResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpMarginAdjustResult{}, fmt.Errorf("store: commit margin adjust: %w", err)
	}
	p.MarginMicro += deltaMicro
	return PerpMarginAdjustResult{Position: p, EventSeqLo: seq, EventSeqHi: seq}, nil
}
