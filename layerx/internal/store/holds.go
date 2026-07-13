package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrHoldClosed is returned when a lifecycle op targets a hold that has already
// been captured (capture/release) or when a capture targets a released/expired
// hold with a different outcome than the recorded one.
var ErrHoldClosed = errors.New("store: hold is not open")

// ErrHoldExpired is returned when a capture arrives after the hold's expiry.
var ErrHoldExpired = errors.New("store: hold expired")

// ErrNotCaptor is returned when a capture is attempted by a DID other than the
// hold's payer-authorized captor_did.
var ErrNotCaptor = errors.New("store: caller is not the hold captor")

// ErrCaptureExceedsHold is returned when a capture amount exceeds the held amount.
var ErrCaptureExceedsHold = errors.New("store: capture exceeds held amount")

// Hold is one authorize->capture/release reservation locked inside the payer's
// own account (funds left balance_usdx at creation; they return on release or
// expiry, or flow to the payee as a standard transfer on capture).
type Hold struct {
	ID            string
	PayerDID      string
	PayeeDID      string
	CaptorDID     string
	AmountMicro   int64
	Ref           string
	ExpiresAt     time.Time
	Status        string
	CapturedMicro int64
	CaptureSeq    int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const holdSelect = `
	SELECT id::text, payer_did, payee_did, captor_did, amount_usdx, COALESCE(ref,''),
	       expires_at, status, COALESCE(captured_usdx,0), COALESCE(capture_seq,0),
	       created_at, updated_at
	FROM holds`

func scanHold(row pgx.Row) (Hold, error) {
	var h Hold
	err := row.Scan(&h.ID, &h.PayerDID, &h.PayeeDID, &h.CaptorDID, &h.AmountMicro, &h.Ref,
		&h.ExpiresAt, &h.Status, &h.CapturedMicro, &h.CaptureSeq, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Hold{}, ErrNotFound
		}
		return Hold{}, fmt.Errorf("store: scan hold: %w", err)
	}
	return h, nil
}

// CreateHold atomically debits the payer's spendable balance into an open hold
// row. Held funds are unspendable by pay and withdraw because they have left
// balance_usdx; escrow_usdx is untouched (it stays the net-reserve audit
// counter). The reserve proof stays exact because Supply counts open holds as
// circulating.
func (s *Store) CreateHold(ctx context.Context, payerDID, payeeDID, captorDID string,
	amountMicro int64, ref string, ttl time.Duration) (Hold, error) {

	if amountMicro <= 0 {
		return Hold{}, fmt.Errorf("store: hold amount must be positive")
	}
	if payerDID == payeeDID {
		return Hold{}, fmt.Errorf("store: cannot hold to self")
	}
	if ttl <= 0 {
		return Hold{}, fmt.Errorf("store: hold ttl must be positive")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Hold{}, fmt.Errorf("store: begin hold tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bal int64
	err = tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, payerDID).Scan(&bal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Hold{}, ErrNotFound
		}
		return Hold{}, fmt.Errorf("store: lock payer: %w", err)
	}
	if bal < amountMicro {
		return Hold{}, ErrInsufficientFunds
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, payerDID, amountMicro); err != nil {
		return Hold{}, fmt.Errorf("store: debit payer: %w", err)
	}
	h, err := scanHold(tx.QueryRow(ctx, `
		INSERT INTO holds (payer_did, payee_did, captor_did, amount_usdx, ref, expires_at)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), now() + $6)
		RETURNING id::text, payer_did, payee_did, captor_did, amount_usdx, COALESCE(ref,''),
		          expires_at, status, COALESCE(captured_usdx,0), COALESCE(capture_seq,0),
		          created_at, updated_at`,
		payerDID, payeeDID, captorDID, amountMicro, ref, ttl))
	if err != nil {
		return Hold{}, fmt.Errorf("store: insert hold: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Hold{}, fmt.Errorf("store: commit hold: %w", err)
	}
	return h, nil
}

// GetHold returns a hold by id, or ErrNotFound.
func (s *Store) GetHold(ctx context.Context, id string) (Hold, error) {
	return scanHold(s.pool.QueryRow(ctx, holdSelect+` WHERE id = $1`, id))
}

// CaptureHold consumes an open hold: it credits the payee amountMicro by
// emitting a STANDARD transfer (monotonic seq, Merkle leaf, sequencer signature
// via finalize — the same commitment path as Pay), returns any remainder to the
// payer's balance, and closes the hold — all in ONE transaction. Only the
// hold's payer-authorized captor may capture, bounded by amount and expiry; the
// payee is fixed at hold creation. Idempotent: re-capturing an already-captured
// hold with the same amount returns the recorded transfer without a second
// value movement.
func (s *Store) CaptureHold(ctx context.Context, id, captorDID string, amountMicro int64, tier string,
	finalize func(seq int64, ts time.Time) (leafHex, sigHex string)) (PayResult, Hold, error) {

	if amountMicro <= 0 {
		return PayResult{}, Hold{}, fmt.Errorf("store: capture amount must be positive")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PayResult{}, Hold{}, fmt.Errorf("store: begin capture tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	h, err := scanHold(tx.QueryRow(ctx, holdSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return PayResult{}, Hold{}, err
	}
	if h.CaptorDID != captorDID {
		return PayResult{}, Hold{}, ErrNotCaptor
	}
	if h.Status == "captured" {
		if h.CapturedMicro == amountMicro {
			res, rerr := s.captureResult(ctx, tx, h)
			return res, h, rerr
		}
		return PayResult{}, Hold{}, ErrHoldClosed
	}
	if h.Status != "open" {
		return PayResult{}, Hold{}, ErrHoldClosed
	}
	if time.Now().After(h.ExpiresAt) {
		return PayResult{}, Hold{}, ErrHoldExpired
	}
	if amountMicro > h.AmountMicro {
		return PayResult{}, Hold{}, ErrCaptureExceedsHold
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO accounts (did, balance_usdx, escrow_usdx) VALUES ($1, 0, 0)
		ON CONFLICT (did) DO NOTHING`, h.PayeeDID); err != nil {
		return PayResult{}, Hold{}, fmt.Errorf("store: ensure payee: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, h.PayeeDID, amountMicro); err != nil {
		return PayResult{}, Hold{}, fmt.Errorf("store: credit payee: %w", err)
	}
	if remainder := h.AmountMicro - amountMicro; remainder > 0 {
		if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, h.PayerDID, remainder); err != nil {
			return PayResult{}, Hold{}, fmt.Errorf("store: return remainder: %w", err)
		}
	}

	var seq int64
	if err := tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('transfers', 'seq'))`).Scan(&seq); err != nil {
		return PayResult{}, Hold{}, fmt.Errorf("store: allocate seq: %w", err)
	}
	ts := time.Now().UTC()
	leafHex, sigHex := finalize(seq, ts)
	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (seq, from_did, to_did, amount_usdx, tier, leaf_hash, sig, ref, ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8,''), $9)`,
		seq, h.PayerDID, h.PayeeDID, amountMicro, tier, leafHex, sigHex, h.Ref, ts); err != nil {
		return PayResult{}, Hold{}, fmt.Errorf("store: insert capture transfer: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE holds SET status = 'captured', captured_usdx = $2, capture_seq = $3, updated_at = now()
		WHERE id = $1`, id, amountMicro, seq); err != nil {
		return PayResult{}, Hold{}, fmt.Errorf("store: close hold: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return PayResult{}, Hold{}, fmt.Errorf("store: commit capture: %w", err)
	}
	h.Status = "captured"
	h.CapturedMicro = amountMicro
	h.CaptureSeq = seq
	return PayResult{Seq: seq, TS: ts, LeafHex: leafHex, SigHex: sigHex, Tier: tier}, h, nil
}

// captureResult rebuilds the PayResult of an already-recorded capture (the
// idempotent replay path) from the transfer row the capture emitted.
func (s *Store) captureResult(ctx context.Context, tx pgx.Tx, h Hold) (PayResult, error) {
	var res PayResult
	err := tx.QueryRow(ctx, `
		SELECT seq, ts, COALESCE(leaf_hash,''), COALESCE(sig,''), tier
		FROM transfers WHERE seq = $1`, h.CaptureSeq).
		Scan(&res.Seq, &res.TS, &res.LeafHex, &res.SigHex, &res.Tier)
	if err != nil {
		return PayResult{}, fmt.Errorf("store: read capture transfer: %w", err)
	}
	return res, nil
}

// ReleaseHold returns the full held amount to the payer and closes the hold.
// Authorized for the hold's captor or payer (checked here so the ledger-level
// bound holds regardless of transport). Idempotent: releasing an
// already-released/expired hold succeeds without moving value; releasing a
// captured hold returns ErrHoldClosed.
func (s *Store) ReleaseHold(ctx context.Context, id, callerDID string) (Hold, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Hold{}, fmt.Errorf("store: begin release tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	h, err := scanHold(tx.QueryRow(ctx, holdSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return Hold{}, err
	}
	if callerDID != h.CaptorDID && callerDID != h.PayerDID {
		return Hold{}, ErrNotCaptor
	}
	switch h.Status {
	case "released", "expired":
		return h, nil
	case "captured":
		return Hold{}, ErrHoldClosed
	}

	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, h.PayerDID, h.AmountMicro); err != nil {
		return Hold{}, fmt.Errorf("store: return held funds: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE holds SET status = 'released', updated_at = now() WHERE id = $1`, id); err != nil {
		return Hold{}, fmt.Errorf("store: release hold: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Hold{}, fmt.Errorf("store: commit release: %w", err)
	}
	h.Status = "released"
	return h, nil
}

// SweepExpiredHolds returns the funds of every open, past-expiry hold to its
// payer and marks the hold expired — one atomic statement per sweep, idempotent
// (an expired hold is no longer open, so a re-run is a no-op). Returns the
// number of holds expired.
func (s *Store) SweepExpiredHolds(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		WITH expired AS (
			UPDATE holds SET status = 'expired', updated_at = now()
			WHERE status = 'open' AND expires_at < now()
			RETURNING payer_did, amount_usdx
		), refunds AS (
			SELECT payer_did, SUM(amount_usdx) AS total FROM expired GROUP BY payer_did
		), credited AS (
			UPDATE accounts a SET balance_usdx = a.balance_usdx + r.total, updated_at = now()
			FROM refunds r WHERE a.did = r.payer_did
			RETURNING a.did
		)
		SELECT COUNT(*) FROM expired`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: sweep expired holds: %w", err)
	}
	return n, nil
}

// OpenHeldMicroUSDX sums the currently-open held amounts — USDX claims that
// have left balances but not yet settled, counted as circulating by Supply.
func (s *Store) OpenHeldMicroUSDX(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_usdx), 0) FROM holds WHERE status = 'open'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: open held sum: %w", err)
	}
	return n, nil
}
