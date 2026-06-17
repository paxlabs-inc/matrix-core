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

// PendingBatch is a sealed-but-not-yet-anchored batch — the input to crash
// recovery (re-anchor the durable open window at-least-once).
type PendingBatch struct {
	ID            string
	RootHex       string
	WindowEnd     time.Time
	TransferCount int
	Status        string
}

// ListUnanchoredBatches returns batches that were sealed (and possibly
// submitted/failed) but never confirmed anchored — the set the settlement
// worker re-submits on startup to honor at-least-once. Oldest first.
func (s *Store) ListUnanchoredBatches(ctx context.Context) ([]PendingBatch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.root, b.window_end, b.status, COUNT(t.seq)
		FROM batches b
		LEFT JOIN transfers t ON t.batch_id = b.id
		WHERE b.status IN ('sealed', 'submitted', 'failed')
		GROUP BY b.id, b.root, b.window_end, b.status, b.created_at
		ORDER BY b.created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list unanchored batches: %w", err)
	}
	defer rows.Close()
	var out []PendingBatch
	for rows.Next() {
		var b PendingBatch
		if err := rows.Scan(&b.ID, &b.RootHex, &b.WindowEnd, &b.Status, &b.TransferCount); err != nil {
			return nil, fmt.Errorf("store: scan unanchored batch: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
