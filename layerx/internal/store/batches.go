package store

import (
	"context"
	"fmt"
	"time"
)

// SealBatch creates a sealed batch over the given transfer seqs (committing the
// Merkle root) and assigns those transfers to it, in one DB transaction. The
// settlement worker then submits + anchors the batch on Paxeer.
func (s *Store) SealBatch(ctx context.Context, rootHex string, seqs []int64, windowStart, windowEnd time.Time) (string, error) {
	if len(seqs) == 0 {
		return "", fmt.Errorf("store: cannot seal an empty batch")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin seal tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO batches (root, window_start, window_end, status)
		VALUES ($1, $2, $3, 'sealed')
		RETURNING id`, rootHex, windowStart, windowEnd).Scan(&id); err != nil {
		return "", fmt.Errorf("store: insert batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE transfers SET batch_id = $1 WHERE seq = ANY($2)`, id, seqs); err != nil {
		return "", fmt.Errorf("store: assign transfers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit seal: %w", err)
	}
	return id, nil
}

// MarkSubmitted records that a batch's settlement tx was broadcast.
func (s *Store) MarkSubmitted(ctx context.Context, batchID, anchorTx string) error {
	_, err := s.pool.Exec(ctx, `UPDATE batches SET status = 'submitted', anchor_tx = $2 WHERE id = $1`, batchID, anchorTx)
	if err != nil {
		return fmt.Errorf("store: mark submitted: %w", err)
	}
	return nil
}

// MarkAnchored records that a batch's settlement tx confirmed on Paxeer — the
// point at which its receipts become fully verifiable (invariant i9).
func (s *Store) MarkAnchored(ctx context.Context, batchID, anchorTx string) error {
	_, err := s.pool.Exec(ctx, `UPDATE batches SET status = 'anchored', anchor_tx = $2 WHERE id = $1`, batchID, anchorTx)
	if err != nil {
		return fmt.Errorf("store: mark anchored: %w", err)
	}
	return nil
}

// MarkBatchFailed records a settlement failure with the error text (honest
// failure surfacing, invariant i9).
func (s *Store) MarkBatchFailed(ctx context.Context, batchID, errText string) error {
	_, err := s.pool.Exec(ctx, `UPDATE batches SET status = 'failed', last_error = $2 WHERE id = $1`, batchID, errText)
	if err != nil {
		return fmt.Errorf("store: mark failed: %w", err)
	}
	return nil
}
