package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrPositionClosed is returned when a mutation targets a CLOSED position
// (closed rows are immutable history).
var ErrPositionClosed = errors.New("store: position is closed")

// ErrMarginInsufficient is returned when a movement would drive position margin
// negative.
var ErrMarginInsufficient = errors.New("store: position margin is insufficient")

// PerpSnapshotRef is the immutable Crossverse reference stored with each
// snapshot-bound row.
type PerpSnapshotRef struct {
	SnapshotID        string
	OrderbookSeq      int64
	StatsSeq          int64
	SourceTimestampMs int64
}

// PerpPosition is one position row.
type PerpPosition struct {
	ID                    string
	OwnerDID              string
	MarketSymbol          string
	Side                  string
	Contracts             int64
	EntryPriceCents       int64
	MarginMicro           int64
	RealizedPnLMicro      int64
	UnsettledFundingMicro int64
	Status                string
	OpenedAt              time.Time
	UpdatedAt             time.Time
}

const perpPositionSelect = `
	SELECT id::text, owner_did, market_symbol, side, contracts, entry_price_cents,
	       margin_usdx, realized_pnl_usdx, unsettled_funding_usdx, status, opened_at, updated_at
	FROM perp_positions`

func scanPerpPosition(row pgx.Row) (PerpPosition, error) {
	var p PerpPosition
	err := row.Scan(&p.ID, &p.OwnerDID, &p.MarketSymbol, &p.Side, &p.Contracts, &p.EntryPriceCents,
		&p.MarginMicro, &p.RealizedPnLMicro, &p.UnsettledFundingMicro, &p.Status, &p.OpenedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpPosition{}, ErrNotFound
		}
		return PerpPosition{}, fmt.Errorf("store: scan perp position: %w", err)
	}
	return p, nil
}

// GetPerpPosition returns one position by id, or ErrNotFound.
func (s *Store) GetPerpPosition(ctx context.Context, id string) (PerpPosition, error) {
	return scanPerpPosition(s.pool.QueryRow(ctx, perpPositionSelect+` WHERE id = $1`, id))
}

// GetOpenPerpPosition returns the owner's single open position on a market, or
// ErrNotFound.
func (s *Store) GetOpenPerpPosition(ctx context.Context, ownerDID, symbol string) (PerpPosition, error) {
	return scanPerpPosition(s.pool.QueryRow(ctx, perpPositionSelect+`
		WHERE owner_did = $1 AND market_symbol = $2 AND status IN ('OPEN','LIQUIDATING')`, ownerDID, symbol))
}

// OpenPerpPositionFromReservation converts a held margin reservation into a
// new OPEN position whose margin is the reserved amount — the value moves
// between perps buckets without touching spendable balances. The partial
// unique index enforces at most one open position per (owner, market).
func (s *Store) OpenPerpPositionFromReservation(ctx context.Context, reservationID, symbol, side string,
	contracts, entryPriceCents int64, ref PerpSnapshotRef) (PerpPosition, error) {

	if contracts <= 0 || entryPriceCents <= 0 {
		return PerpPosition{}, fmt.Errorf("store: position contracts and entry price must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpPosition{}, fmt.Errorf("store: begin position open: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r, err := scanPerpReservation(tx.QueryRow(ctx, perpReservationSelect+` WHERE id = $1 FOR UPDATE`, reservationID))
	if err != nil {
		return PerpPosition{}, err
	}
	if r.Status != "held" {
		return PerpPosition{}, ErrReservationClosed
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_margin_reservations SET status = 'applied', updated_at = now() WHERE id = $1`, reservationID); err != nil {
		return PerpPosition{}, fmt.Errorf("store: apply margin reservation: %w", err)
	}
	p, err := scanPerpPosition(tx.QueryRow(ctx, `
		INSERT INTO perp_positions (
			owner_did, market_symbol, side, contracts, entry_price_cents, margin_usdx,
			snapshot_id, orderbook_seq, stats_seq, source_timestamp_ms)
		VALUES ($1,$2,$3,$4,$5,$6, NULLIF($7,''), NULLIF($8::bigint,0), NULLIF($9::bigint,0), NULLIF($10::bigint,0))
		RETURNING id::text, owner_did, market_symbol, side, contracts, entry_price_cents,
		          margin_usdx, realized_pnl_usdx, unsettled_funding_usdx, status, opened_at, updated_at`,
		r.OwnerDID, symbol, side, contracts, entryPriceCents, r.AmountMicro,
		ref.SnapshotID, ref.OrderbookSeq, ref.StatsSeq, ref.SourceTimestampMs))
	if err != nil {
		return PerpPosition{}, fmt.Errorf("store: insert perp position: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, r.OwnerDID, r.OwnerDID, "position.updated", map[string]any{
		"reason": "opened", "position_id": p.ID, "symbol": symbol, "side": side,
		"contracts": contracts, "margin_usdx": r.AmountMicro,
	}); err != nil {
		return PerpPosition{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpPosition{}, fmt.Errorf("store: commit position open: %w", err)
	}
	return p, nil
}

// lockOpenPosition loads a non-CLOSED position under FOR UPDATE.
func lockOpenPosition(ctx context.Context, tx pgx.Tx, id string) (PerpPosition, error) {
	p, err := scanPerpPosition(tx.QueryRow(ctx, perpPositionSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return PerpPosition{}, err
	}
	if p.Status == "CLOSED" {
		return PerpPosition{}, ErrPositionClosed
	}
	return p, nil
}

// RealizePerpPnL settles one signed conserved transfer between the position's
// margin and protocol liquidity capital: positive deltaMicro pays the user from
// the pool, negative moves user margin into the pool. Both buckets are bounded
// at zero — the pool paying out more than it holds is ErrPoolInsufficient (an
// insolvency signal, never an unbacked credit).
func (s *Store) RealizePerpPnL(ctx context.Context, positionID string, deltaMicro int64) (PerpPosition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpPosition{}, fmt.Errorf("store: begin pnl: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := s.transferMarginPool(ctx, tx, positionID, deltaMicro, "pnl.realized")
	if err != nil {
		return PerpPosition{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE perp_positions SET realized_pnl_usdx = realized_pnl_usdx + $2, updated_at = now()
		WHERE id = $1`, positionID, deltaMicro); err != nil {
		return PerpPosition{}, fmt.Errorf("store: record realized pnl: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpPosition{}, fmt.Errorf("store: commit pnl: %w", err)
	}
	p.RealizedPnLMicro += deltaMicro
	p.MarginMicro += deltaMicro
	return p, nil
}

// transferMarginPool moves one signed integer between position margin and the
// liquidity pool inside the caller's transaction, journaling the movement.
func (s *Store) transferMarginPool(ctx context.Context, tx pgx.Tx, positionID string, deltaMicro int64, reason string) (PerpPosition, error) {
	p, err := lockOpenPosition(ctx, tx, positionID)
	if err != nil {
		return PerpPosition{}, err
	}
	if deltaMicro == 0 {
		return p, nil
	}
	var poolCapital int64
	if err := tx.QueryRow(ctx, `SELECT capital_usdx FROM perp_pools WHERE id = 'liquidity' FOR UPDATE`).Scan(&poolCapital); err != nil {
		return PerpPosition{}, fmt.Errorf("store: lock liquidity pool: %w", err)
	}
	if deltaMicro > 0 && poolCapital < deltaMicro {
		return PerpPosition{}, ErrPoolInsufficient
	}
	if deltaMicro < 0 && p.MarginMicro < -deltaMicro {
		return PerpPosition{}, ErrMarginInsufficient
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_pools SET capital_usdx = capital_usdx - $1, updated_at = now() WHERE id = 'liquidity'`, deltaMicro); err != nil {
		return PerpPosition{}, fmt.Errorf("store: move pool capital: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_positions SET margin_usdx = margin_usdx + $2, updated_at = now() WHERE id = $1`, positionID, deltaMicro); err != nil {
		return PerpPosition{}, fmt.Errorf("store: move position margin: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, p.OwnerDID, p.OwnerDID, "position.updated", map[string]any{
		"reason": reason, "position_id": positionID, "transfer_usdx": deltaMicro,
	}); err != nil {
		return PerpPosition{}, err
	}
	return p, nil
}

// ApplyPerpFunding settles one funding interval for a position as a single
// signed conserved transfer (positive credits the position from the pool) and
// records the entry. Exactly-once per (position, interval): a replay with the
// same values is a no-op; a replay with different values is rejected.
func (s *Store) ApplyPerpFunding(ctx context.Context, positionID string, intervalStartMs, intervalEndMs,
	appliedPpb, transferMicro int64, ref PerpSnapshotRef) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin funding: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existingTransfer, existingPpb int64
	err = tx.QueryRow(ctx, `
		SELECT transfer_usdx, applied_ppb FROM perp_funding_entries
		WHERE position_id = $1 AND interval_start_ms = $2`, positionID, intervalStartMs).
		Scan(&existingTransfer, &existingPpb)
	if err == nil {
		if existingTransfer == transferMicro && existingPpb == appliedPpb {
			return nil
		}
		return fmt.Errorf("store: funding interval already settled with different values")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: check funding entry: %w", err)
	}

	p, err := s.transferMarginPool(ctx, tx, positionID, transferMicro, "funding.applied")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO perp_funding_entries (
			position_id, owner_did, market_symbol, interval_start_ms, interval_end_ms,
			applied_ppb, transfer_usdx, snapshot_id, stats_seq, source_timestamp_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7, NULLIF($8,''), NULLIF($9::bigint,0), NULLIF($10::bigint,0))`,
		positionID, p.OwnerDID, p.MarketSymbol, intervalStartMs, intervalEndMs,
		appliedPpb, transferMicro, ref.SnapshotID, ref.StatsSeq, ref.SourceTimestampMs); err != nil {
		return fmt.Errorf("store: insert funding entry: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit funding: %w", err)
	}
	return nil
}

// ReducePerpPosition closes closeContracts of an open position and returns
// marginReturnMicro from position margin to the owner's spendable balance (the
// engine computes the return; the store enforces the bucket bounds). Closing
// the full quantity closes the position and returns ALL remaining margin.
func (s *Store) ReducePerpPosition(ctx context.Context, positionID string, closeContracts, marginReturnMicro int64) (PerpPosition, error) {
	if closeContracts <= 0 {
		return PerpPosition{}, fmt.Errorf("store: close contracts must be positive")
	}
	if marginReturnMicro < 0 {
		return PerpPosition{}, fmt.Errorf("store: margin return must not be negative")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpPosition{}, fmt.Errorf("store: begin reduce: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := lockOpenPosition(ctx, tx, positionID)
	if err != nil {
		return PerpPosition{}, err
	}
	if closeContracts > p.Contracts {
		return PerpPosition{}, fmt.Errorf("store: close %d exceeds position %d contracts", closeContracts, p.Contracts)
	}
	full := closeContracts == p.Contracts
	returnMicro := marginReturnMicro
	if full {
		returnMicro = p.MarginMicro
	} else if marginReturnMicro > p.MarginMicro {
		return PerpPosition{}, ErrMarginInsufficient
	}
	if returnMicro > 0 {
		if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, p.OwnerDID, returnMicro); err != nil {
			return PerpPosition{}, fmt.Errorf("store: return position margin: %w", err)
		}
	}
	if full {
		if _, err := tx.Exec(ctx, `
			UPDATE perp_positions SET contracts = 0, margin_usdx = 0, status = 'CLOSED',
			       closed_at = now(), updated_at = now() WHERE id = $1`, positionID); err != nil {
			return PerpPosition{}, fmt.Errorf("store: close position: %w", err)
		}
		p.Status = "CLOSED"
		p.Contracts = 0
		p.MarginMicro = 0
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE perp_positions SET contracts = contracts - $2, margin_usdx = margin_usdx - $3,
			       updated_at = now() WHERE id = $1`, positionID, closeContracts, returnMicro); err != nil {
			return PerpPosition{}, fmt.Errorf("store: reduce position: %w", err)
		}
		p.Contracts -= closeContracts
		p.MarginMicro -= returnMicro
	}
	if _, err := appendPerpEvent(ctx, tx, p.OwnerDID, p.OwnerDID, "position.updated", map[string]any{
		"reason": "reduced", "position_id": positionID, "closed_contracts": closeContracts,
		"margin_returned_usdx": returnMicro, "closed": full,
	}); err != nil {
		return PerpPosition{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpPosition{}, fmt.Errorf("store: commit reduce: %w", err)
	}
	return p, nil
}

// AddPerpPositionMargin moves value from the owner's spendable balance into an
// open position's margin.
func (s *Store) AddPerpPositionMargin(ctx context.Context, positionID string, amountMicro int64) (PerpPosition, error) {
	if amountMicro <= 0 {
		return PerpPosition{}, fmt.Errorf("store: margin add must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpPosition{}, fmt.Errorf("store: begin margin add: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := lockOpenPosition(ctx, tx, positionID)
	if err != nil {
		return PerpPosition{}, err
	}
	var bal int64
	err = tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, p.OwnerDID).Scan(&bal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpPosition{}, ErrNotFound
		}
		return PerpPosition{}, fmt.Errorf("store: lock margin adder: %w", err)
	}
	if bal < amountMicro {
		return PerpPosition{}, ErrInsufficientFunds
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, p.OwnerDID, amountMicro); err != nil {
		return PerpPosition{}, fmt.Errorf("store: debit margin add: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE perp_positions SET margin_usdx = margin_usdx + $2, updated_at = now() WHERE id = $1`, positionID, amountMicro); err != nil {
		return PerpPosition{}, fmt.Errorf("store: credit position margin: %w", err)
	}
	if _, err := appendPerpEvent(ctx, tx, p.OwnerDID, p.OwnerDID, "position.updated", map[string]any{
		"reason": "margin.added", "position_id": positionID, "amount_usdx": amountMicro,
	}); err != nil {
		return PerpPosition{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpPosition{}, fmt.Errorf("store: commit margin add: %w", err)
	}
	p.MarginMicro += amountMicro
	return p, nil
}
