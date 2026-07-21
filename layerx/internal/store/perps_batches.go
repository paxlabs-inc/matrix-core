package store

import (
	"context"
	"fmt"
	"time"
)

// SealPerpBatch commits a Merkle root over the journal range [lo, hi].
func (s *Store) SealPerpBatch(ctx context.Context, rootHex string, eventSeqLo, eventSeqHi int64) (string, error) {
	if eventSeqLo <= 0 || eventSeqHi < eventSeqLo {
		return "", fmt.Errorf("store: perp batch range [%d,%d] is invalid", eventSeqLo, eventSeqHi)
	}
	var id string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO perp_batches (root, event_seq_lo, event_seq_hi)
		VALUES ($1, $2, $3) RETURNING id::text`, rootHex, eventSeqLo, eventSeqHi).Scan(&id); err != nil {
		return "", fmt.Errorf("store: seal perp batch: %w", err)
	}
	return id, nil
}

// MarkPerpBatchAnchored records the confirmed anchor tx.
func (s *Store) MarkPerpBatchAnchored(ctx context.Context, batchID, anchorTx string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE perp_batches SET status = 'anchored', anchor_tx = $2 WHERE id = $1`, batchID, anchorTx); err != nil {
		return fmt.Errorf("store: mark perp batch anchored: %w", err)
	}
	return nil
}

// MarkPerpBatchFailed records an honest anchoring failure.
func (s *Store) MarkPerpBatchFailed(ctx context.Context, batchID, errText string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE perp_batches SET status = 'failed', last_error = $2 WHERE id = $1`, batchID, errText); err != nil {
		return fmt.Errorf("store: mark perp batch failed: %w", err)
	}
	return nil
}

// PerpBatch is one perps journal commitment.
type PerpBatch struct {
	ID         string
	RootHex    string
	EventSeqLo int64
	EventSeqHi int64
	Status     string
	AnchorTx   string
	CreatedAt  time.Time
}

// ListUnanchoredPerpBatches returns sealed/submitted/failed perps batches
// oldest first — the at-least-once recovery set.
func (s *Store) ListUnanchoredPerpBatches(ctx context.Context) ([]PerpBatch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, root, event_seq_lo, event_seq_hi, status, COALESCE(anchor_tx,''), created_at
		FROM perp_batches WHERE status IN ('sealed', 'submitted', 'failed')
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list unanchored perp batches: %w", err)
	}
	defer rows.Close()
	var out []PerpBatch
	for rows.Next() {
		var b PerpBatch
		if err := rows.Scan(&b.ID, &b.RootHex, &b.EventSeqLo, &b.EventSeqHi, &b.Status, &b.AnchorTx, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan perp batch: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastAnchoredPerpEventSeq returns the highest journal seq committed by any
// sealed-or-later batch (the anchoring worker's durable cursor).
func (s *Store) LastAnchoredPerpEventSeq(ctx context.Context) (int64, error) {
	var hi int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(event_seq_hi), 0) FROM perp_batches`).Scan(&hi); err != nil {
		return 0, fmt.Errorf("store: last anchored perp seq: %w", err)
	}
	return hi, nil
}
