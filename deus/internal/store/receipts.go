package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ReceiptRow is a persisted EIP-712 receipt.
type ReceiptRow struct {
	InvocationID string
	Digest       string
	GatewaySig   string
	RunnerSig    *string
}

// InsertReceipt stores a signed receipt.
func (s *Store) InsertReceipt(ctx context.Context, r ReceiptRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO receipts (invocation_id, eip712_digest, gateway_sig, runner_sig)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (invocation_id) DO NOTHING`,
		r.InvocationID, r.Digest, r.GatewaySig, r.RunnerSig,
	)
	if err != nil {
		return fmt.Errorf("store: insert receipt: %w", err)
	}
	return nil
}

// GetReceipt loads receipt by invocation id.
func (s *Store) GetReceipt(ctx context.Context, invocationID string) (ReceiptRow, error) {
	var r ReceiptRow
	err := s.pool.QueryRow(ctx, `
		SELECT invocation_id::text, eip712_digest, gateway_sig, runner_sig
		FROM receipts WHERE invocation_id = $1`, invocationID,
	).Scan(&r.InvocationID, &r.Digest, &r.GatewaySig, &r.RunnerSig)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ReceiptRow{}, fmt.Errorf("store: receipt not found")
		}
		return ReceiptRow{}, fmt.Errorf("store: get receipt: %w", err)
	}
	return r, nil
}
