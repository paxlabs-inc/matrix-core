package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrDelegationTerminal is returned when a lifecycle op targets an EXPIRED or
// REVOKED delegation (terminal grants never reactivate).
var ErrDelegationTerminal = errors.New("store: delegation is terminal")

// ErrNotOwner is returned when a delegation mutation is attempted by a DID
// other than the granting owner (a delegate can never modify its own grant).
var ErrNotOwner = errors.New("store: caller is not the delegation owner")

// ErrDelegationRequired is returned when a delegated action has no ACTIVE,
// unexpired delegation from the owner to the acting DID.
var ErrDelegationRequired = errors.New("store: no active delegation authorizes this actor")

// ErrDelegationLimit is returned when a delegated risk increase exceeds a
// bound of the owner-signed grant (market, order type, notional, leverage, or
// daily window).
var ErrDelegationLimit = errors.New("store: delegated action exceeds the grant bounds")

// ErrMembershipRequired is returned when the owner's current membership tier
// no longer satisfies the grant's tier (downgrade blocks risk increase only).
var ErrMembershipRequired = errors.New("store: owner membership tier does not permit delegated risk increase")

// PerpDelegation is one owner-signed bounded grant.
type PerpDelegation struct {
	ID                        string
	OwnerDID                  string
	DelegateDID               string
	MembershipTier            string
	AllowedMarkets            []string
	AllowedOrderTypes         []string
	MaxOrderNotionalMicro     int64
	MaxPositionNotionalMicro  int64
	MaxLeverageX              int64
	MaxDailyNotionalMicro     int64
	MaxDailyRealizedLossMicro int64
	GrantSignature            string
	PublicKey                 string
	Status                    string
	ExpiresAt                 time.Time
	RevokedAt                 time.Time
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

const perpDelegationSelect = `
	SELECT id::text, owner_did, delegate_did, membership_tier, allowed_markets, allowed_order_types,
	       max_order_notional_usdx, max_position_notional_usdx, max_leverage_x,
	       max_daily_notional_usdx, max_daily_realized_loss_usdx, grant_signature, public_key,
	       status, expires_at, COALESCE(revoked_at, 'epoch'::timestamptz), created_at, updated_at
	FROM perp_delegations`

func scanPerpDelegation(row pgx.Row) (PerpDelegation, error) {
	var d PerpDelegation
	err := row.Scan(&d.ID, &d.OwnerDID, &d.DelegateDID, &d.MembershipTier, &d.AllowedMarkets, &d.AllowedOrderTypes,
		&d.MaxOrderNotionalMicro, &d.MaxPositionNotionalMicro, &d.MaxLeverageX,
		&d.MaxDailyNotionalMicro, &d.MaxDailyRealizedLossMicro, &d.GrantSignature, &d.PublicKey,
		&d.Status, &d.ExpiresAt, &d.RevokedAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerpDelegation{}, ErrNotFound
		}
		return PerpDelegation{}, fmt.Errorf("store: scan perp delegation: %w", err)
	}
	return d, nil
}

// CreatePerpDelegation persists an owner-signed grant as ACTIVE and journals
// delegation.updated. Signature verification against the canonical grant is
// the caller's (API-layer) responsibility; the store enforces the structural
// bounds and the one-active-grant-per-pair law.
func (s *Store) CreatePerpDelegation(ctx context.Context, d PerpDelegation) (PerpDelegation, error) {
	if d.ExpiresAt.Before(time.Now()) {
		return PerpDelegation{}, fmt.Errorf("store: delegation expiry is in the past")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpDelegation{}, fmt.Errorf("store: begin delegation create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO perp_delegations (
			owner_did, delegate_did, membership_tier, allowed_markets, allowed_order_types,
			max_order_notional_usdx, max_position_notional_usdx, max_leverage_x,
			max_daily_notional_usdx, max_daily_realized_loss_usdx, grant_signature, public_key,
			status, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'ACTIVE',$13)
		RETURNING id::text, created_at, updated_at`,
		d.OwnerDID, d.DelegateDID, d.MembershipTier, d.AllowedMarkets, d.AllowedOrderTypes,
		d.MaxOrderNotionalMicro, d.MaxPositionNotionalMicro, d.MaxLeverageX,
		d.MaxDailyNotionalMicro, d.MaxDailyRealizedLossMicro, d.GrantSignature, d.PublicKey,
		d.ExpiresAt)
	if err := row.Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return PerpDelegation{}, fmt.Errorf("store: insert perp delegation: %w", err)
	}
	d.Status = "ACTIVE"
	if _, err := appendPerpEvent(ctx, tx, d.OwnerDID, d.OwnerDID, "delegation.updated", map[string]any{
		"delegation_id": d.ID, "delegate_did": d.DelegateDID, "status": "ACTIVE",
	}); err != nil {
		return PerpDelegation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpDelegation{}, fmt.Errorf("store: commit delegation create: %w", err)
	}
	return d, nil
}

// GetActivePerpDelegation returns the single ACTIVE grant from owner to
// delegate, or ErrNotFound.
func (s *Store) GetActivePerpDelegation(ctx context.Context, ownerDID, delegateDID string) (PerpDelegation, error) {
	return scanPerpDelegation(s.pool.QueryRow(ctx, perpDelegationSelect+`
		WHERE owner_did = $1 AND delegate_did = $2 AND status = 'ACTIVE'`, ownerDID, delegateDID))
}

// GetPerpOwnerTimezone returns the owner's configured IANA timezone ('UTC'
// when unset) — the anchor for delegation daily-counter windows.
func (s *Store) GetPerpOwnerTimezone(ctx context.Context, ownerDID string) (string, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT timezone FROM perp_owner_settings WHERE owner_did = $1`, ownerDID).Scan(&tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return "UTC", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: owner timezone: %w", err)
	}
	return tz, nil
}

// SetPerpOwnerTimezone stores the owner's IANA timezone.
func (s *Store) SetPerpOwnerTimezone(ctx context.Context, ownerDID, tz string) error {
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("store: timezone %q is invalid: %w", tz, err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO perp_owner_settings (owner_did, timezone) VALUES ($1, $2)
		ON CONFLICT (owner_did) DO UPDATE SET timezone = EXCLUDED.timezone, updated_at = now()`,
		ownerDID, tz); err != nil {
		return fmt.Errorf("store: set owner timezone: %w", err)
	}
	return nil
}

// ExpirePerpDelegations transitions every ACTIVE grant past its expiry to
// EXPIRED and journals the change. Worker-driven (reconciliation tick);
// enforcement itself rejects expired grants regardless of this sweep.
func (s *Store) ExpirePerpDelegations(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin delegation expiry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		UPDATE perp_delegations SET status = 'EXPIRED', updated_at = now()
		WHERE status = 'ACTIVE' AND expires_at <= now()
		RETURNING id::text, owner_did, delegate_did`)
	if err != nil {
		return 0, fmt.Errorf("store: expire delegations: %w", err)
	}
	type expired struct{ id, owner, delegate string }
	var out []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.owner, &e.delegate); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan expired delegation: %w", err)
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate expired delegations: %w", err)
	}
	for _, e := range out {
		if _, err := appendPerpEvent(ctx, tx, e.owner, e.delegate, "delegation.updated", map[string]any{
			"delegation_id": e.id, "delegate_did": e.delegate, "status": "EXPIRED",
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit delegation expiry: %w", err)
	}
	return len(out), nil
}

// GetPerpDelegation returns one delegation by id, or ErrNotFound.
func (s *Store) GetPerpDelegation(ctx context.Context, id string) (PerpDelegation, error) {
	return scanPerpDelegation(s.pool.QueryRow(ctx, perpDelegationSelect+` WHERE id = $1`, id))
}

// ListPerpDelegations returns the owner's delegations newest first.
func (s *Store) ListPerpDelegations(ctx context.Context, ownerDID string, limit int) ([]PerpDelegation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, perpDelegationSelect+`
		WHERE owner_did = $1 ORDER BY created_at DESC LIMIT $2`, ownerDID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list perp delegations: %w", err)
	}
	defer rows.Close()
	var out []PerpDelegation
	for rows.Next() {
		d, err := scanPerpDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokePerpDelegation marks the grant REVOKED and, in the SAME transaction,
// cancels every open order the delegate placed for this owner and releases
// their held margin reservations back to the owner's spendable balance.
// Only the granting owner may revoke; a delegate cannot revoke (or otherwise
// modify) its own grant. Idempotent for an already-revoked grant.
func (s *Store) RevokePerpDelegation(ctx context.Context, id, callerDID string) (PerpDelegation, []string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpDelegation{}, nil, fmt.Errorf("store: begin revoke: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Canonical lock order (account -> delegation -> ...): the caller-named
	// owner's account row is locked BEFORE the grant so a concurrent engine
	// transaction (which locks the account first) can never deadlock with a
	// revocation.
	var lockedBal int64
	if err := tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, callerDID).Scan(&lockedBal); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PerpDelegation{}, nil, fmt.Errorf("store: lock revoking owner: %w", err)
	}

	d, err := scanPerpDelegation(tx.QueryRow(ctx, perpDelegationSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return PerpDelegation{}, nil, err
	}
	if callerDID != d.OwnerDID {
		return PerpDelegation{}, nil, ErrNotOwner
	}
	if d.Status == "REVOKED" {
		return d, nil, nil
	}
	if d.Status == "EXPIRED" {
		return PerpDelegation{}, nil, ErrDelegationTerminal
	}
	if _, err := tx.Exec(ctx, `
		UPDATE perp_delegations SET status = 'REVOKED', revoked_at = now(), updated_at = now()
		WHERE id = $1`, id); err != nil {
		return PerpDelegation{}, nil, fmt.Errorf("store: revoke delegation: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT id::text FROM perp_orders
		WHERE owner_did = $1 AND acting_did = $2
		  AND status IN ('RECEIVED', 'ACCEPTED', 'RESTING', 'PARTIALLY_FILLED')
		FOR UPDATE`, d.OwnerDID, d.DelegateDID)
	if err != nil {
		return PerpDelegation{}, nil, fmt.Errorf("store: list delegate orders: %w", err)
	}
	var cancelled []string
	for rows.Next() {
		var orderID string
		if err := rows.Scan(&orderID); err != nil {
			rows.Close()
			return PerpDelegation{}, nil, fmt.Errorf("store: scan delegate order: %w", err)
		}
		cancelled = append(cancelled, orderID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PerpDelegation{}, nil, fmt.Errorf("store: iterate delegate orders: %w", err)
	}

	for _, orderID := range cancelled {
		if _, err := tx.Exec(ctx, `UPDATE perp_orders SET status = 'CANCELLED', updated_at = now() WHERE id = $1`, orderID); err != nil {
			return PerpDelegation{}, nil, fmt.Errorf("store: cancel delegate order: %w", err)
		}
		var resTotal int64
		if err := tx.QueryRow(ctx, `
			WITH released AS (
				UPDATE perp_margin_reservations SET status = 'released', updated_at = now()
				WHERE order_id = $1 AND status = 'held'
				RETURNING amount_usdx
			)
			SELECT COALESCE(SUM(amount_usdx), 0) FROM released`, orderID).Scan(&resTotal); err != nil {
			return PerpDelegation{}, nil, fmt.Errorf("store: release delegate reservations: %w", err)
		}
		if resTotal > 0 {
			if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, d.OwnerDID, resTotal); err != nil {
				return PerpDelegation{}, nil, fmt.Errorf("store: return delegate margin: %w", err)
			}
		}
		if _, err := appendPerpEvent(ctx, tx, d.OwnerDID, d.DelegateDID, "order.cancelled", map[string]any{
			"order_id": orderID, "reason": "delegation.revoked", "released_usdx": resTotal,
		}); err != nil {
			return PerpDelegation{}, nil, err
		}
	}
	if _, err := appendPerpEvent(ctx, tx, d.OwnerDID, d.OwnerDID, "delegation.updated", map[string]any{
		"delegation_id": id, "delegate_did": d.DelegateDID, "status": "REVOKED",
		"cancelled_orders": len(cancelled),
	}); err != nil {
		return PerpDelegation{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpDelegation{}, nil, fmt.Errorf("store: commit revoke: %w", err)
	}
	d.Status = "REVOKED"
	return d, cancelled, nil
}
