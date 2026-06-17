package store

import (
	"context"
	"fmt"
)

// QueuedWithdrawal is a withdrawal awaiting (or undergoing) on-chain payout,
// joined with its account's mapped EVM payout address. EVMAddress is empty when
// the DID has no payout binding yet — such a withdrawal cannot be paid out and
// is left queued (skipped, not sealed) until a binding exists.
type QueuedWithdrawal struct {
	ID          string
	DID         string
	AmountMicro int64
	SwapOut     string
	EVMAddress  string
	PayoutRoot  string
}

// SealedWithdrawal pins a withdrawal's id to the recipient EVM address it is
// being paid out to — the input to SealWithdrawals. Freezing the recipient at
// seal time (rather than re-reading the account at recovery) keeps the payout
// root reproducible even if the account's mapped address later changes.
type SealedWithdrawal struct {
	ID         string
	EVMAddress string
}

// ListQueuedWithdrawals returns withdrawals still in the 'queued' state (oldest
// first), each joined to its account's mapped EVM payout address. These are the
// candidates for the next withdrawal-payout pass.
func (s *Store) ListQueuedWithdrawals(ctx context.Context, limit int) ([]QueuedWithdrawal, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.did, w.amount_usdx, COALESCE(w.swap_out,''), COALESCE(a.evm_address,'')
		FROM withdrawals w
		LEFT JOIN accounts a ON a.did = w.did
		WHERE w.status = 'queued'
		ORDER BY w.created_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list queued withdrawals: %w", err)
	}
	defer rows.Close()
	var out []QueuedWithdrawal
	for rows.Next() {
		var q QueuedWithdrawal
		if err := rows.Scan(&q.ID, &q.DID, &q.AmountMicro, &q.SwapOut, &q.EVMAddress); err != nil {
			return nil, fmt.Errorf("store: scan queued withdrawal: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ListSubmittedWithdrawals returns withdrawals sealed into a payout batch but not
// yet confirmed settled (the crash-recovery input). Grouped/ordered by
// payout_root then id so a recovery pass rebuilds each group's commitment in the
// same deterministic leaf order it was sealed with.
func (s *Store) ListSubmittedWithdrawals(ctx context.Context) ([]QueuedWithdrawal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, did, amount_usdx, COALESCE(swap_out,''), COALESCE(payout_evm,''), COALESCE(payout_root,'')
		FROM withdrawals
		WHERE status = 'submitted'
		ORDER BY payout_root ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list submitted withdrawals: %w", err)
	}
	defer rows.Close()
	var out []QueuedWithdrawal
	for rows.Next() {
		var q QueuedWithdrawal
		if err := rows.Scan(&q.ID, &q.DID, &q.AmountMicro, &q.SwapOut, &q.EVMAddress, &q.PayoutRoot); err != nil {
			return nil, fmt.Errorf("store: scan submitted withdrawal: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// SealWithdrawals transitions the given withdrawals queued -> submitted and
// stamps the deterministic payout_root over them, in one transaction. Once
// sealed the set is frozen: recovery re-derives the SAME root from these rows,
// so a retried on-chain submit maps to the same idempotent batch id and never
// double-pays. Only rows still 'queued' are moved (a concurrent settle cannot
// double-seal).
func (s *Store) SealWithdrawals(ctx context.Context, payoutRoot string, items []SealedWithdrawal) error {
	if payoutRoot == "" {
		return fmt.Errorf("store: seal withdrawals: empty payout root")
	}
	if len(items) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin seal withdrawals: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, it := range items {
		ct, err := tx.Exec(ctx, `
			UPDATE withdrawals
			SET status = 'submitted', payout_root = $1, payout_evm = $2, last_error = NULL
			WHERE id = $3 AND status = 'queued'`, payoutRoot, it.EVMAddress, it.ID)
		if err != nil {
			return fmt.Errorf("store: seal withdrawal %s: %w", it.ID, err)
		}
		if ct.RowsAffected() != 1 {
			return fmt.Errorf("store: seal withdrawal %s: not in 'queued' state", it.ID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit seal withdrawals: %w", err)
	}
	return nil
}

// MarkWithdrawalSettled records a confirmed on-chain payout (submitted ->
// settled) with its tx hash. Idempotent: re-marking an already-settled row is a
// no-op.
func (s *Store) MarkWithdrawalSettled(ctx context.Context, id, payoutTx string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE withdrawals
		SET status = 'settled', payout_tx = $2, last_error = NULL
		WHERE id = $1 AND status = 'submitted'`, id, payoutTx)
	if err != nil {
		return fmt.Errorf("store: mark withdrawal settled: %w", err)
	}
	return nil
}

// RecordWithdrawalError surfaces a submit failure on every submitted row of a
// payout group WITHOUT changing status — the group stays 'submitted' so the
// recovery pass retries it at-least-once (idempotent on the frozen payout_root).
func (s *Store) RecordWithdrawalError(ctx context.Context, payoutRoot, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE withdrawals SET last_error = $2
		WHERE payout_root = $1 AND status = 'submitted'`, payoutRoot, errText)
	if err != nil {
		return fmt.Errorf("store: record withdrawal error: %w", err)
	}
	return nil
}
