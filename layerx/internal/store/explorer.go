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

// batchSelect reads the denormalized transfer_count column (set at SealBatch)
// instead of re-aggregating child transfers via a LEFT JOIN + GROUP BY on every
// page — the latter re-counts thousands of leaves per batch on each feed hit.
const batchSelect = `
	SELECT b.id, b.root, b.window_start, b.window_end, b.status,
	       COALESCE(b.anchor_tx, ''), b.transfer_count, b.created_at
	FROM batches b`

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
	// Batches are low-cardinality (one per settlement window), so OFFSET
	// pagination here is cheap and kept for back-compat.
	rows, err := s.pool.Query(ctx, batchSelect+`
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
	b, err := scanBatch(s.pool.QueryRow(ctx, batchSelect+` WHERE b.id = $1`, id))
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
	b, err := scanBatch(s.pool.QueryRow(ctx, batchSelect+` WHERE b.root = $1 ORDER BY b.created_at DESC LIMIT 1`, rootHex))
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
	Ref          string
	TS           time.Time
	BatchRootHex string
	AnchorTx     string
	Settled      bool
}

const transferSelect = `
	SELECT t.seq, COALESCE(t.batch_id::text, ''), t.from_did, t.to_did, t.amount_usdx,
	       t.tier, COALESCE(t.leaf_hash, ''), COALESCE(t.ref, ''), t.ts,
	       COALESCE(b.root, ''), COALESCE(b.anchor_tx, ''), COALESCE(b.status, '')
	FROM transfers t
	LEFT JOIN batches b ON b.id = t.batch_id`

func scanTransfer(rows pgx.Rows) (TransferSummary, error) {
	var t TransferSummary
	var status string
	if err := rows.Scan(&t.Seq, &t.BatchID, &t.FromDID, &t.ToDID, &t.AmountMicro,
		&t.Tier, &t.LeafHex, &t.Ref, &t.TS, &t.BatchRootHex, &t.AnchorTx, &status); err != nil {
		return TransferSummary{}, err
	}
	t.Settled = status == "anchored"
	return t, nil
}

// ListTransfers returns transfers newest-first (by seq), KEYSET-paginated: pass
// beforeSeq = the smallest seq from the previous page (0 for the first page) to
// get the next older page. Keyset (WHERE seq < cursor) is O(limit) regardless of
// how deep the page is, unlike OFFSET which walks + discards every skipped row —
// the difference between a feed that stays fast and one that degrades linearly as
// the table grows into the millions.
//
// When did is non-empty, only transfers that DID is a party to are returned. A
// single `from_did = $ OR to_did = $` cannot use either composite index for an
// ordered LIMIT (the planner falls back to a BitmapOr + sort), so it is split
// into a UNION ALL of two index-ordered legs (each rides its own
// (from_did|to_did, seq DESC) index) merged + re-limited. Self-pay is rejected,
// so from_did != to_did always and the legs never produce duplicate rows.
func (s *Store) ListTransfers(ctx context.Context, did string, limit int, beforeSeq int64) ([]TransferSummary, error) {
	limit, _ = clampPage(limit, 0)
	var rows pgx.Rows
	var err error
	if did == "" {
		rows, err = s.pool.Query(ctx, transferSelect+`
			WHERE ($1 <= 0 OR t.seq < $1)
			ORDER BY t.seq DESC LIMIT $2`, beforeSeq, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT * FROM (
				(`+transferSelect+`
				  WHERE t.from_did = $1 AND ($3 <= 0 OR t.seq < $3)
				  ORDER BY t.seq DESC LIMIT $2)
				UNION ALL
				(`+transferSelect+`
				  WHERE t.to_did = $1 AND ($3 <= 0 OR t.seq < $3)
				  ORDER BY t.seq DESC LIMIT $2)
			) u ORDER BY u.seq DESC LIMIT $2`, did, limit, beforeSeq)
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

// ListTransfersSince returns transfers with seq > sinceSeq in ASCENDING seq
// order (capped at limit) — the replay query the SSE stream uses to backfill a
// reconnecting client from its Last-Event-ID before resuming the live tail.
func (s *Store) ListTransfersSince(ctx context.Context, sinceSeq int64, limit int) ([]TransferSummary, error) {
	limit, _ = clampPage(limit, 0)
	rows, err := s.pool.Query(ctx, transferSelect+`
		WHERE t.seq > $1 ORDER BY t.seq ASC LIMIT $2`, sinceSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list transfers since: %w", err)
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
		       COALESCE(t.leaf_hash,''), COALESCE(t.sig,''), COALESCE(t.ref,''), t.ts,
		       b.root, b.anchor_tx, b.status
		FROM transfers t
		LEFT JOIN batches b ON b.id = t.batch_id
		WHERE t.seq = $1`, seq).
		Scan(&r.Seq, &batchID, &r.FromDID, &r.ToDID, &r.AmountMicro, &r.Tier,
			&r.LeafHex, &r.SigHex, &r.Ref, &r.TS, &root, &anchor, &status)
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

// Supply returns the circulating USDX supply and descriptive counts. Open
// holds, held perps margin reservations, open-position margin, and protocol
// pool capital are USDX claims that have left spendable balances without
// leaving the system, so they count as circulating — the reserve proof
// (invariant i1) and the perps conservation identity stay exact across every
// hold/capture/release and reserve/fill/funding/close interleaving.
func (s *Store) Supply(ctx context.Context) (SupplyStats, error) {
	var st SupplyStats
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(balance_usdx), 0)
		       + (SELECT COALESCE(SUM(amount_usdx), 0) FROM holds WHERE status = 'open')
		       + (SELECT COALESCE(SUM(amount_usdx), 0) FROM perp_margin_reservations WHERE status = 'held')
		       + (SELECT COALESCE(SUM(margin_usdx), 0) FROM perp_positions WHERE status IN ('OPEN', 'LIQUIDATING'))
		       + (SELECT COALESCE(SUM(capital_usdx), 0) FROM perp_pools),
		       COUNT(*) FROM accounts`).
		Scan(&st.CirculatingMicroUSDX, &st.Accounts); err != nil {
		return SupplyStats{}, fmt.Errorf("store: supply accounts: %w", err)
	}
	// transfers is the largest, append-only table; an exact COUNT(*) is a full
	// scan that gets linearly slower forever on a public, pollable endpoint. The
	// transfer count here is purely descriptive (the reserve proof is
	// circulating-vs-reserve, not this), so use the planner's row estimate.
	// GREATEST handles both shapes: a plain table (reltuples on the relation) and
	// the RANGE-partitioned table (parent reltuples is 0; sum the partitions).
	if err := s.pool.QueryRow(ctx, `
		SELECT GREATEST(
			(SELECT reltuples::bigint FROM pg_class WHERE oid = 'transfers'::regclass),
			COALESCE((SELECT SUM(c.reltuples)::bigint
			          FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			          WHERE i.inhparent = 'transfers'::regclass), 0),
			0)`).Scan(&st.Transfers); err != nil {
		return SupplyStats{}, fmt.Errorf("store: supply transfers: %w", err)
	}
	return st, nil
}
