package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds the public, unauthenticated READ surface the explorer/RPC is
// built on (PHASE 4, full-transparency rollup model): batch + transfer
// enumeration, root lookup, public (unscoped) transfer reads, and supply stats.
// None of these scope on a caller DID — the data is public by design.

// maxPageLimit caps a single page of any paginated enumeration.
const maxPageLimit = 200

// defaultPageLimit is used when a caller omits/zeroes the limit.
const defaultPageLimit = 50

// clampPage normalizes caller-supplied pagination into safe bounds.
func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// BatchSummary is the public view of a settlement batch.
type BatchSummary struct {
	ID            string
	RootHex       string
	WindowStart   time.Time
	WindowEnd     time.Time
	Status        string
	AnchorTx      string
	TransferCount int
	CreatedAt     time.Time
}

const batchSelect = `
	SELECT b.id, b.root, b.window_start, b.window_end, b.status,
	       COALESCE(b.anchor_tx, ''), COUNT(t.seq), b.created_at
	FROM batches b
	LEFT JOIN transfers t ON t.batch_id = b.id`

func scanBatch(row pgx.Row) (BatchSummary, error) {
	var b BatchSummary
	if err := row.Scan(&b.ID, &b.RootHex, &b.WindowStart, &b.WindowEnd, &b.Status,
		&b.AnchorTx, &b.TransferCount, &b.CreatedAt); err != nil {
		return BatchSummary{}, err
	}
	return b, nil
}

// ListBatches returns settlement batches newest-first, paginated.
func (s *Store) ListBatches(ctx context.Context, limit, offset int) ([]BatchSummary, error) {
	limit, offset = clampPage(limit, offset)
	rows, err := s.pool.Query(ctx, batchSelect+`
		GROUP BY b.id
		ORDER BY b.created_at DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list batches: %w", err)
	}
	defer rows.Close()
	var out []BatchSummary
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan batch: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBatch returns a single batch by id, or ErrNotFound.
func (s *Store) GetBatch(ctx context.Context, id string) (BatchSummary, error) {
	b, err := scanBatch(s.pool.QueryRow(ctx, batchSelect+` WHERE b.id = $1 GROUP BY b.id`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BatchSummary{}, ErrNotFound
		}
		return BatchSummary{}, fmt.Errorf("store: get batch: %w", err)
	}
	return b, nil
}

// GetBatchByRoot returns the batch that committed rootHex, or ErrNotFound — the
// backing query for GET /v1/anchor/{root} (the public root -> anchor-tx lookup).
func (s *Store) GetBatchByRoot(ctx context.Context, rootHex string) (BatchSummary, error) {
	b, err := scanBatch(s.pool.QueryRow(ctx, batchSelect+` WHERE b.root = $1 GROUP BY b.id ORDER BY b.created_at DESC LIMIT 1`, rootHex))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BatchSummary{}, ErrNotFound
		}
		return BatchSummary{}, fmt.Errorf("store: get batch by root: %w", err)
	}
	return b, nil
}

// TransferSummary is the public explorer view of a transfer joined with its
// (optional) settlement batch.
type TransferSummary struct {
	Seq          int64
	BatchID      string
	FromDID      string
	ToDID        string
	AmountMicro  int64
	Tier         string
	LeafHex      string
	TS           time.Time
	BatchRootHex string
	AnchorTx     string
	Settled      bool
}

const transferSelect = `
	SELECT t.seq, COALESCE(t.batch_id::text, ''), t.from_did, t.to_did, t.amount_usdx,
	       t.tier, COALESCE(t.leaf_hash, ''), t.ts,
	       COALESCE(b.root, ''), COALESCE(b.anchor_tx, ''), COALESCE(b.status, '')
	FROM transfers t
	LEFT JOIN batches b ON b.id = t.batch_id`

func scanTransfer(rows pgx.Rows) (TransferSummary, error) {
	var t TransferSummary
	var status string
	if err := rows.Scan(&t.Seq, &t.BatchID, &t.FromDID, &t.ToDID, &t.AmountMicro,
		&t.Tier, &t.LeafHex, &t.TS, &t.BatchRootHex, &t.AnchorTx, &status); err != nil {
		return TransferSummary{}, err
	}
	t.Settled = status == "anchored"
	return t, nil
}

// ListTransfers returns transfers newest-first (by seq), paginated. When did is
// non-empty, only transfers that DID is a party to (payer or payee) are returned.
func (s *Store) ListTransfers(ctx context.Context, did string, limit, offset int) ([]TransferSummary, error) {
	limit, offset = clampPage(limit, offset)
	var rows pgx.Rows
	var err error
	if did == "" {
		rows, err = s.pool.Query(ctx, transferSelect+`
			ORDER BY t.seq DESC LIMIT $1 OFFSET $2`, limit, offset)
	} else {
		rows, err = s.pool.Query(ctx, transferSelect+`
			WHERE t.from_did = $1 OR t.to_did = $1
			ORDER BY t.seq DESC LIMIT $2 OFFSET $3`, did, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list transfers: %w", err)
	}
	defer rows.Close()
	var out []TransferSummary
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan transfer: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTransferPublic returns a transfer by seq WITHOUT caller-DID scoping — the
// public receipt read (GET /v1/receipt/{seq}). Mirrors GetTransfer but visible
// to anyone, since the explorer surface is fully transparent.
func (s *Store) GetTransferPublic(ctx context.Context, seq int64) (TransferRow, error) {
	var r TransferRow
	var batchID, root, anchor, status *string
	err := s.pool.QueryRow(ctx, `
		SELECT t.seq, t.batch_id, t.from_did, t.to_did, t.amount_usdx, t.tier,
		       COALESCE(t.leaf_hash,''), COALESCE(t.sig,''), t.ts,
		       b.root, b.anchor_tx, b.status
		FROM transfers t
		LEFT JOIN batches b ON b.id = t.batch_id
		WHERE t.seq = $1`, seq).
		Scan(&r.Seq, &batchID, &r.FromDID, &r.ToDID, &r.AmountMicro, &r.Tier,
			&r.LeafHex, &r.SigHex, &r.TS, &root, &anchor, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TransferRow{}, ErrNotFound
		}
		return TransferRow{}, fmt.Errorf("store: get transfer public: %w", err)
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

// SupplyStats is the off-chain side of the reserve proof (invariant i1): the
// circulating USDX supply plus a few descriptive counts. The on-chain reserve is
// read separately from the vault and compared by the server.
type SupplyStats struct {
	CirculatingMicroUSDX int64
	Accounts             int64
	Transfers            int64
}

// Supply returns the circulating USDX supply and descriptive counts.
func (s *Store) Supply(ctx context.Context) (SupplyStats, error) {
	var st SupplyStats
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(balance_usdx), 0), COUNT(*) FROM accounts`).
		Scan(&st.CirculatingMicroUSDX, &st.Accounts); err != nil {
		return SupplyStats{}, fmt.Errorf("store: supply accounts: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfers`).Scan(&st.Transfers); err != nil {
		return SupplyStats{}, fmt.Errorf("store: supply transfers: %w", err)
	}
	return st, nil
}
