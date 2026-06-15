package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/paxlabs-inc/layerx/pkg/types"
)

// GetAccount returns the account for did, or ErrNotFound.
func (s *Store) GetAccount(ctx context.Context, did string) (types.Account, error) {
	var a types.Account
	err := s.pool.QueryRow(ctx, `
		SELECT did, COALESCE(evm_address,''), balance_usdx, escrow_usdx, created_at, updated_at
		FROM accounts WHERE did = $1`, did).
		Scan(&a.DID, &a.EVMAddress, &a.BalanceUSDX, &a.EscrowUSDX, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.Account{}, ErrNotFound
		}
		return types.Account{}, fmt.Errorf("store: get account: %w", err)
	}
	return a, nil
}

// SetEVMAddress records/updates the DID's mapped Paxeer payout address.
func (s *Store) SetEVMAddress(ctx context.Context, did, evm string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO accounts (did, evm_address) VALUES ($1, $2)
		ON CONFLICT (did) DO UPDATE SET evm_address = EXCLUDED.evm_address, updated_at = now()`, did, evm)
	if err != nil {
		return fmt.Errorf("store: set evm address: %w", err)
	}
	return nil
}

// CreditDeposit credits a confirmed on-chain deposit: balance += amount AND
// escrow += amount (the deposit IS the escrow). Idempotent on deposit_tx.
func (s *Store) CreditDeposit(ctx context.Context, did, evm, depositTx string, amountMicro int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin deposit tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency: skip if this deposit_tx was already credited.
	if depositTx != "" {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(1) FROM deposits WHERE deposit_tx = $1`, depositTx).Scan(&n); err != nil {
			return fmt.Errorf("store: deposit idempotency check: %w", err)
		}
		if n > 0 {
			return nil
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO accounts (did, evm_address, balance_usdx, escrow_usdx)
		VALUES ($1, NULLIF($2,''), $3, $3)
		ON CONFLICT (did) DO UPDATE SET
			balance_usdx = accounts.balance_usdx + EXCLUDED.balance_usdx,
			escrow_usdx  = accounts.escrow_usdx + EXCLUDED.escrow_usdx,
			evm_address  = COALESCE(NULLIF(EXCLUDED.evm_address,''), accounts.evm_address),
			updated_at   = now()`, did, evm, amountMicro); err != nil {
		return fmt.Errorf("store: credit deposit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO deposits (did, evm_address, amount_usdx, deposit_tx)
		VALUES ($1, NULLIF($2,''), $3, NULLIF($4,''))`,
		did, evm, amountMicro, depositTx); err != nil {
		return fmt.Errorf("store: record deposit: %w", err)
	}
	return tx.Commit(ctx)
}

// PayResult is what a successful Pay produced.
type PayResult struct {
	Seq     int64
	TS      time.Time
	LeafHex string
	SigHex  string
	Tier    string
}

// Pay atomically: locks the sender, enforces the escrow bound, debits sender /
// credits recipient, inserts the ordered transfer (assigning the monotonic seq +
// ts), then calls finalize(seq, ts) to compute the Merkle leaf + sequencer sig
// and writes them back — all in ONE DB transaction (layerx.frozen.kvx [ledger]
// atomicity). The recipient account is created on first receipt.
func (s *Store) Pay(ctx context.Context, fromDID, toDID string, amountMicro int64, tier string,
	finalize func(seq int64, ts time.Time) (leafHex, sigHex string)) (PayResult, error) {

	if amountMicro <= 0 {
		return PayResult{}, fmt.Errorf("store: pay amount must be positive")
	}
	if fromDID == toDID {
		return PayResult{}, fmt.Errorf("store: cannot pay self")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PayResult{}, fmt.Errorf("store: begin pay tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bal int64
	err = tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, fromDID).Scan(&bal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PayResult{}, ErrNotFound
		}
		return PayResult{}, fmt.Errorf("store: lock sender: %w", err)
	}
	if bal < amountMicro {
		return PayResult{}, ErrInsufficientFunds
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO accounts (did, balance_usdx, escrow_usdx) VALUES ($1, 0, 0)
		ON CONFLICT (did) DO NOTHING`, toDID); err != nil {
		return PayResult{}, fmt.Errorf("store: ensure recipient: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx - $2, updated_at = now() WHERE did = $1`, fromDID, amountMicro); err != nil {
		return PayResult{}, fmt.Errorf("store: debit sender: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_usdx = balance_usdx + $2, updated_at = now() WHERE did = $1`, toDID, amountMicro); err != nil {
		return PayResult{}, fmt.Errorf("store: credit recipient: %w", err)
	}

	var seq int64
	var ts time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO transfers (from_did, to_did, amount_usdx, tier)
		VALUES ($1, $2, $3, $4)
		RETURNING seq, ts`, fromDID, toDID, amountMicro, tier).Scan(&seq, &ts); err != nil {
		return PayResult{}, fmt.Errorf("store: insert transfer: %w", err)
	}

	leafHex, sigHex := finalize(seq, ts)
	if _, err := tx.Exec(ctx, `UPDATE transfers SET leaf_hash = $1, sig = $2 WHERE seq = $3`, leafHex, sigHex, seq); err != nil {
		return PayResult{}, fmt.Errorf("store: write leaf/sig: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return PayResult{}, fmt.Errorf("store: commit pay: %w", err)
	}
	return PayResult{Seq: seq, TS: ts, LeafHex: leafHex, SigHex: sigHex, Tier: tier}, nil
}

// QueueWithdrawal debits balance + escrow and records a withdrawal row to be
// paid out at settlement. Escrow-bounded (cannot withdraw more than spendable).
func (s *Store) QueueWithdrawal(ctx context.Context, did string, amountMicro int64, swapOut, tier string) (string, error) {
	if amountMicro <= 0 {
		return "", fmt.Errorf("store: withdraw amount must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin withdraw tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bal int64
	err = tx.QueryRow(ctx, `SELECT balance_usdx FROM accounts WHERE did = $1 FOR UPDATE`, did).Scan(&bal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: lock account: %w", err)
	}
	if bal < amountMicro {
		return "", ErrInsufficientFunds
	}
	// Spendable balance is the authoritative bound. escrow_usdx is the audit
	// counter of this account's net on-chain reserve; it is clamped at 0 because
	// a recipient may hold balance it received via internal pays (which never
	// moved escrow), so escrow can legitimately be below balance.
	if _, err := tx.Exec(ctx, `
		UPDATE accounts
		SET balance_usdx = balance_usdx - $2,
		    escrow_usdx  = GREATEST(escrow_usdx - $2, 0),
		    updated_at   = now()
		WHERE did = $1`, did, amountMicro); err != nil {
		return "", fmt.Errorf("store: debit withdrawal: %w", err)
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO withdrawals (did, amount_usdx, swap_out, tier, status)
		VALUES ($1, $2, NULLIF($3,''), $4, 'queued')
		RETURNING id`, did, amountMicro, swapOut, tier).Scan(&id); err != nil {
		return "", fmt.Errorf("store: record withdrawal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit withdrawal: %w", err)
	}
	return id, nil
}
