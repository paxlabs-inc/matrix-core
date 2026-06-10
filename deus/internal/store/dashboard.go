package store

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// LooksLikeUUID reports whether s is uuid-shaped (safe to cast to pg uuid).
func LooksLikeUUID(s string) bool {
	return uuidShape.MatchString(s)
}

// GetServiceByIDOrSlug resolves a service by uuid when the input is
// uuid-shaped, otherwise by slug. Public routes link services by slug while
// internal flows use the uuid; both must resolve (marketplace contract).
func (s *Store) GetServiceByIDOrSlug(ctx context.Context, idOrSlug string) (ServiceRow, error) {
	if LooksLikeUUID(idOrSlug) {
		return s.GetServiceByID(ctx, idOrSlug)
	}
	return s.GetServiceBySlug(ctx, idOrSlug)
}

// DeveloperRow mirrors the developers table for dashboard reads.
type DeveloperRow struct {
	ID            string
	WalletAddress string
	PayoutAddress string
	DisplayName   string
}

// DeveloperByWallet loads a developer row by wallet address.
func (s *Store) DeveloperByWallet(ctx context.Context, wallet string) (DeveloperRow, error) {
	var row DeveloperRow
	var displayName *string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, wallet_address, payout_address, display_name
		FROM developers WHERE lower(wallet_address) = lower($1)`, wallet,
	).Scan(&row.ID, &row.WalletAddress, &row.PayoutAddress, &displayName)
	if err != nil {
		if err == pgx.ErrNoRows {
			return DeveloperRow{}, fmt.Errorf("store: developer not found")
		}
		return DeveloperRow{}, fmt.Errorf("store: developer by wallet: %w", err)
	}
	if displayName != nil {
		row.DisplayName = *displayName
	}
	return row, nil
}

// UpdateDeveloperPayoutAddress sets a developer's payout address.
func (s *Store) UpdateDeveloperPayoutAddress(ctx context.Context, developerID, payout string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE developers SET payout_address = $2 WHERE id = $1`, developerID, payout)
	if err != nil {
		return fmt.Errorf("store: update payout address: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: developer not found")
	}
	return nil
}

// OwnedServiceRow is one developer-owned listing with usage aggregates.
type OwnedServiceRow struct {
	ServiceRow
	Invocations int
	RevenueWei  string
}

// ListServicesByDeveloperWallet returns the developer's listings (any status)
// with finalized invocation count and revenue aggregates, newest first.
func (s *Store) ListServicesByDeveloperWallet(ctx context.Context, wallet string) ([]OwnedServiceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sv.id::text, sv.chain_id, sv.developer_id::text, sv.slug, sv.kind, sv.mode,
		       sv.display_name, sv.summary, sv.manifest, sv.manifest_hash, sv.status,
		       sv.confidential, sv.quality_score::text, sv.uptime_bps,
		       COUNT(i.id) FILTER (WHERE i.outcome = 'ok')::int,
		       COALESCE(SUM(i.price_wei::numeric) FILTER (WHERE i.outcome = 'ok'), 0)::text
		FROM services sv
		JOIN developers d ON d.id = sv.developer_id
		LEFT JOIN invocations i ON i.service_id = sv.id
		WHERE lower(d.wallet_address) = lower($1)
		GROUP BY sv.id
		ORDER BY sv.created_at DESC`, wallet,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list services by developer: %w", err)
	}
	defer rows.Close()

	var out []OwnedServiceRow
	for rows.Next() {
		var row OwnedServiceRow
		var chainID *int64
		if err := rows.Scan(
			&row.ID, &chainID, &row.DeveloperID, &row.Slug, &row.Kind, &row.Mode,
			&row.DisplayName, &row.Summary, &row.Manifest, &row.ManifestHash, &row.Status,
			&row.Confidential, &row.QualityScore, &row.UptimeBPS,
			&row.Invocations, &row.RevenueWei,
		); err != nil {
			return nil, fmt.Errorf("store: scan owned service: %w", err)
		}
		row.ChainID = chainID
		out = append(out, row)
	}
	return out, rows.Err()
}

// SpendEntryRow is per-service caller spend.
type SpendEntryRow struct {
	ServiceID   string
	DisplayName string
	Invocations int
	TotalWei    string
}

// SpendByCaller aggregates a caller's finalized spend grouped by service.
// Matches on caller DID, falling back to caller wallet when provided.
func (s *Store) SpendByCaller(ctx context.Context, did, wallet string) (string, []SpendEntryRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.service_id::text, sv.display_name,
		       COUNT(i.id)::int,
		       COALESCE(SUM(i.price_wei::numeric), 0)::text
		FROM invocations i
		JOIN services sv ON sv.id = i.service_id
		WHERE i.outcome = 'ok'
		  AND (i.caller_did = $1 OR ($2 <> '' AND lower(COALESCE(i.caller_wallet,'')) = lower($2)))
		GROUP BY i.service_id, sv.display_name
		ORDER BY SUM(i.price_wei::numeric) DESC`, did, wallet,
	)
	if err != nil {
		return "", nil, fmt.Errorf("store: spend by caller: %w", err)
	}
	defer rows.Close()

	var entries []SpendEntryRow
	for rows.Next() {
		var e SpendEntryRow
		if err := rows.Scan(&e.ServiceID, &e.DisplayName, &e.Invocations, &e.TotalWei); err != nil {
			return "", nil, fmt.Errorf("store: scan spend entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	var total string
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(price_wei::numeric), 0)::text
		FROM invocations
		WHERE outcome = 'ok'
		  AND (caller_did = $1 OR ($2 <> '' AND lower(COALESCE(caller_wallet,'')) = lower($2)))`,
		did, wallet,
	).Scan(&total)
	if err != nil {
		return "", nil, fmt.Errorf("store: spend total: %w", err)
	}
	return total, entries, nil
}

// AnalyticsTotals are lifetime aggregates for one service.
type AnalyticsTotals struct {
	TotalInvocations int
	TotalRevenueWei  string
	AvgLatencyMS     int
	SuccessRate      float64
}

// ServiceAnalyticsTotals computes lifetime invocation aggregates.
func (s *Store) ServiceAnalyticsTotals(ctx context.Context, serviceID string) (AnalyticsTotals, error) {
	var t AnalyticsTotals
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE outcome = 'ok')::int,
		       COALESCE(SUM(price_wei::numeric) FILTER (WHERE outcome = 'ok'), 0)::text,
		       COALESCE(AVG(latency_ms) FILTER (WHERE outcome = 'ok'), 0)::int,
		       CASE WHEN COUNT(*) FILTER (WHERE outcome IN ('ok','error')) = 0 THEN 0
		            ELSE COUNT(*) FILTER (WHERE outcome = 'ok')::float /
		                 COUNT(*) FILTER (WHERE outcome IN ('ok','error'))::float
		       END
		FROM invocations WHERE service_id = $1`, serviceID,
	).Scan(&t.TotalInvocations, &t.TotalRevenueWei, &t.AvgLatencyMS, &t.SuccessRate)
	if err != nil {
		return AnalyticsTotals{}, fmt.Errorf("store: analytics totals: %w", err)
	}
	return t, nil
}

// AnalyticsDayRow is one day's aggregates.
type AnalyticsDayRow struct {
	Date         string
	Invocations  int
	RevenueWei   string
	AvgLatencyMS int
	SuccessRate  float64
}

// ServiceAnalyticsSeries returns per-day aggregates for the trailing window.
func (s *Store) ServiceAnalyticsSeries(ctx context.Context, serviceID string, days int) ([]AnalyticsDayRow, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	rows, err := s.pool.Query(ctx, `
		SELECT to_char(date_trunc('day', created_at), 'YYYY-MM-DD'),
		       COUNT(*) FILTER (WHERE outcome = 'ok')::int,
		       COALESCE(SUM(price_wei::numeric) FILTER (WHERE outcome = 'ok'), 0)::text,
		       COALESCE(AVG(latency_ms) FILTER (WHERE outcome = 'ok'), 0)::int,
		       CASE WHEN COUNT(*) FILTER (WHERE outcome IN ('ok','error')) = 0 THEN 0
		            ELSE COUNT(*) FILTER (WHERE outcome = 'ok')::float /
		                 COUNT(*) FILTER (WHERE outcome IN ('ok','error'))::float
		       END
		FROM invocations
		WHERE service_id = $1 AND created_at >= now() - make_interval(days => $2)
		GROUP BY 1 ORDER BY 1 ASC`, serviceID, days,
	)
	if err != nil {
		return nil, fmt.Errorf("store: analytics series: %w", err)
	}
	defer rows.Close()

	var out []AnalyticsDayRow
	for rows.Next() {
		var d AnalyticsDayRow
		if err := rows.Scan(&d.Date, &d.Invocations, &d.RevenueWei, &d.AvgLatencyMS, &d.SuccessRate); err != nil {
			return nil, fmt.Errorf("store: scan analytics day: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TopOperationRow is per-operation usage for one service.
type TopOperationRow struct {
	Operation   string
	Invocations int
	RevenueWei  string
}

// ServiceTopOperations returns the most invoked operations by revenue.
func (s *Store) ServiceTopOperations(ctx context.Context, serviceID string, limit int) ([]TopOperationRow, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.operation,
		       COUNT(i.id)::int,
		       COALESCE(SUM(i.price_wei::numeric), 0)::text
		FROM invocations i
		JOIN endpoints e ON e.id = i.endpoint_id
		WHERE i.service_id = $1 AND i.outcome = 'ok'
		GROUP BY e.operation
		ORDER BY SUM(i.price_wei::numeric) DESC
		LIMIT $2`, serviceID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: top operations: %w", err)
	}
	defer rows.Close()

	var out []TopOperationRow
	for rows.Next() {
		var t TopOperationRow
		if err := rows.Scan(&t.Operation, &t.Invocations, &t.RevenueWei); err != nil {
			return nil, fmt.Errorf("store: scan top operation: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// InvocationLogRow is a ledger entry shaped for the dashboard activity log.
type InvocationLogRow struct {
	CreatedAt time.Time
	Operation string
	Units     string
	LatencyMS *int
	Outcome   string
}

// RecentInvocationLogs returns the newest finalized invocations for a service,
// oldest-first, joined to operation names for the activity log view.
func (s *Store) RecentInvocationLogs(ctx context.Context, serviceID string, limit int) ([]InvocationLogRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT * FROM (
			SELECT i.created_at, COALESCE(e.operation, ''), i.units, i.latency_ms, i.outcome
			FROM invocations i
			LEFT JOIN endpoints e ON e.id = i.endpoint_id
			WHERE i.service_id = $1 AND i.outcome <> 'reserved'
			ORDER BY i.created_at DESC
			LIMIT $2
		) recent ORDER BY 1 ASC`, serviceID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: recent invocation logs: %w", err)
	}
	defer rows.Close()

	var out []InvocationLogRow
	for rows.Next() {
		var r InvocationLogRow
		if err := rows.Scan(&r.CreatedAt, &r.Operation, &r.Units, &r.LatencyMS, &r.Outcome); err != nil {
			return nil, fmt.Errorf("store: scan invocation log: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EarningsTotals are lifetime developer revenue aggregates.
type EarningsTotals struct {
	TotalEarnedWei string
	PendingWei     string
	SettledWei     string
}

// EarningsForDeveloper sums finalized revenue across the developer's services,
// split into settled (attached to a settlement) and pending (unsettled).
func (s *Store) EarningsForDeveloper(ctx context.Context, developerID string) (EarningsTotals, error) {
	var t EarningsTotals
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(i.price_wei::numeric), 0)::text,
		       COALESCE(SUM(i.price_wei::numeric) FILTER (WHERE i.settlement_id IS NULL), 0)::text,
		       COALESCE(SUM(i.price_wei::numeric) FILTER (WHERE i.settlement_id IS NOT NULL), 0)::text
		FROM invocations i
		JOIN services sv ON sv.id = i.service_id
		WHERE sv.developer_id = $1 AND i.outcome = 'ok'`, developerID,
	).Scan(&t.TotalEarnedWei, &t.PendingWei, &t.SettledWei)
	if err != nil {
		return EarningsTotals{}, fmt.Errorf("store: earnings totals: %w", err)
	}
	return t, nil
}

// ListSettlementsForDeveloper returns settlement windows, newest first.
func (s *Store) ListSettlementsForDeveloper(ctx context.Context, developerID string, limit int) ([]SettlementRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, developer_id::text, rail, total_wei, invocation_count,
		       COALESCE(merkle_root,''), tx_hash, window_start, window_end, status
		FROM settlements
		WHERE developer_id = $1
		ORDER BY window_end DESC
		LIMIT $2`, developerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list settlements: %w", err)
	}
	defer rows.Close()

	var out []SettlementRow
	for rows.Next() {
		var r SettlementRow
		if err := rows.Scan(
			&r.ID, &r.DeveloperID, &r.Rail, &r.TotalWei, &r.InvocationCount,
			&r.MerkleRoot, &r.TxHash, &r.WindowStart, &r.WindowEnd, &r.Status,
		); err != nil {
			return nil, fmt.Errorf("store: scan settlement: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
