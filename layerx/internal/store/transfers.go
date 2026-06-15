package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TransferRow is a transfer joined with its (optional) settlement batch — the
// data a Receipt is built from.
type TransferRow struct {
	Seq          int64
	BatchID      string
	FromDID      string
	ToDID        string
	AmountMicro  int64
	Tier         string
	LeafHex      string
	SigHex       string
	TS           time.Time
	BatchRootHex string
	AnchorTx     string
	Settled      bool
}

// GetTransfer returns a transfer the caller is a party to (payer or payee),
// joined with its batch (root/anchor populated once sealed/anchored).
func (s *Store) GetTransfer(ctx context.Context, seq int64, callerDID string) (TransferRow, error) {
	var r TransferRow
	var batchID, root, anchor *string
	var status *string
	err := s.pool.QueryRow(ctx, `
		SELECT t.seq, t.batch_id, t.from_did, t.to_did, t.amount_usdx, t.tier,
		       COALESCE(t.leaf_hash,''), COALESCE(t.sig,''), t.ts,
		       b.root, b.anchor_tx, b.status
		FROM transfers t
		LEFT JOIN batches b ON b.id = t.batch_id
		WHERE t.seq = $1 AND (t.from_did = $2 OR t.to_did = $2)`, seq, callerDID).
		Scan(&r.Seq, &batchID, &r.FromDID, &r.ToDID, &r.AmountMicro, &r.Tier,
			&r.LeafHex, &r.SigHex, &r.TS, &root, &anchor, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TransferRow{}, ErrNotFound
		}
		return TransferRow{}, fmt.Errorf("store: get transfer: %w", err)
	}
	if batchID != nil {
		r.BatchID = *batchID
	}
	if root != nil {
		r.BatchRootHex = *root
	}
	if anchor != nil {
		r.AnchorTx = *anchor
	}
	if status != nil && *status == "anchored" {
		r.Settled = true
	}
	return r, nil
}

// BatchLeaf is a transfer's seq + leaf within a sealed batch, used to recompute
// inclusion proofs for receipts.
type BatchLeaf struct {
	Seq     int64
	LeafHex string
}

// ListBatchLeaves returns the ordered (seq ASC) leaves of a sealed batch — the
// input set for recomputing a Merkle inclusion proof.
func (s *Store) ListBatchLeaves(ctx context.Context, batchID string) ([]BatchLeaf, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, COALESCE(leaf_hash,'') FROM transfers
		WHERE batch_id = $1 ORDER BY seq ASC`, batchID)
	if err != nil {
		return nil, fmt.Errorf("store: list batch leaves: %w", err)
	}
	defer rows.Close()
	var out []BatchLeaf
	for rows.Next() {
		var b BatchLeaf
		if err := rows.Scan(&b.Seq, &b.LeafHex); err != nil {
			return nil, fmt.Errorf("store: scan batch leaf: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UnsettledTransfer is a transfer not yet assigned to a batch.
type UnsettledTransfer struct {
	Seq         int64
	FromDID     string
	ToDID       string
	AmountMicro int64
	LeafHex     string
}

// ListUnsettled returns transfers with no batch yet, in seq order — the input
// to a settlement window.
func (s *Store) ListUnsettled(ctx context.Context, limit int) ([]UnsettledTransfer, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT seq, from_did, to_did, amount_usdx, COALESCE(leaf_hash,'')
		FROM transfers WHERE batch_id IS NULL ORDER BY seq ASC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list unsettled: %w", err)
	}
	defer rows.Close()
	var out []UnsettledTransfer
	for rows.Next() {
		var u UnsettledTransfer
		if err := rows.Scan(&u.Seq, &u.FromDID, &u.ToDID, &u.AmountMicro, &u.LeafHex); err != nil {
			return nil, fmt.Errorf("store: scan unsettled: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
