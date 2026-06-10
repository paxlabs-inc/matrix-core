package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ServiceRow is a persisted listing mirror.
type ServiceRow struct {
	ID           string
	ChainID      *int64
	DeveloperID  string
	Slug         string
	Kind         string
	Mode         string
	DisplayName  string
	Summary      string
	Manifest     json.RawMessage
	ManifestHash string
	Status       string
	Confidential bool
	QualityScore *string
	UptimeBPS    *int
}

// InsertDraftService creates a draft listing from manifest fields.
func (s *Store) InsertDraftService(ctx context.Context, row ServiceRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO services (
			developer_id, slug, kind, mode, display_name, summary,
			manifest, manifest_hash, status, confidential, chain_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULL)
		RETURNING id::text`,
		row.DeveloperID, row.Slug, row.Kind, row.Mode, row.DisplayName, row.Summary,
		row.Manifest, row.ManifestHash, row.Status, row.Confidential,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert service: %w", err)
	}
	return id, nil
}

// GetServiceByID loads a service by uuid.
func (s *Store) GetServiceByID(ctx context.Context, id string) (ServiceRow, error) {
	var row ServiceRow
	var chainID *int64
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, chain_id, developer_id::text, slug, kind, mode,
		       display_name, summary, manifest, manifest_hash, status,
		       confidential, quality_score::text, uptime_bps
		FROM services WHERE id = $1`, id,
	).Scan(
		&row.ID, &chainID, &row.DeveloperID, &row.Slug, &row.Kind, &row.Mode,
		&row.DisplayName, &row.Summary, &row.Manifest, &row.ManifestHash, &row.Status,
		&row.Confidential, &row.QualityScore, &row.UptimeBPS,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ServiceRow{}, fmt.Errorf("store: service not found: %w", err)
		}
		return ServiceRow{}, fmt.Errorf("store: get service: %w", err)
	}
	row.ChainID = chainID
	return row, nil
}

// GetServiceBySlug loads a service by slug.
func (s *Store) GetServiceBySlug(ctx context.Context, slug string) (ServiceRow, error) {
	var row ServiceRow
	var chainID *int64
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, chain_id, developer_id::text, slug, kind, mode,
		       display_name, summary, manifest, manifest_hash, status,
		       confidential, quality_score::text, uptime_bps
		FROM services WHERE slug = $1`, slug,
	).Scan(
		&row.ID, &chainID, &row.DeveloperID, &row.Slug, &row.Kind, &row.Mode,
		&row.DisplayName, &row.Summary, &row.Manifest, &row.ManifestHash, &row.Status,
		&row.Confidential, &row.QualityScore, &row.UptimeBPS,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ServiceRow{}, fmt.Errorf("store: service not found: %w", err)
		}
		return ServiceRow{}, fmt.Errorf("store: get service by slug: %w", err)
	}
	row.ChainID = chainID
	return row, nil
}

// UpdateServiceStatus sets lifecycle status.
func (s *Store) UpdateServiceStatus(ctx context.Context, id, status string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE services SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("store: update status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: service not found")
	}
	return nil
}

// ActivateFromChain marks a service active with on-chain id and hashes.
func (s *Store) ActivateFromChain(ctx context.Context, id string, chainID int64, manifestHash, pricingHash string, hosted, confidential bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE services SET
			chain_id = $2,
			manifest_hash = $3,
			status = 'active',
			confidential = $4,
			mode = CASE WHEN $5 THEN mode ELSE mode END,
			updated_at = now()
		WHERE id = $1`,
		id, chainID, manifestHash, confidential, hosted,
	)
	if err != nil {
		return fmt.Errorf("store: activate from chain: %w", err)
	}
	return nil
}

// UpsertFromChainEvent mirrors an on-chain registration into services.
func (s *Store) UpsertFromChainEvent(ctx context.Context, chainID int64, ownerWallet, manifestHash, pricingHash string, hosted, confidential bool, manifest json.RawMessage) (string, error) {
	devID, err := s.UpsertDeveloperByWallet(ctx, ownerWallet, ownerWallet, "")
	if err != nil {
		return "", err
	}
	var slug, displayName, summary, kind, mode string
	if len(manifest) > 0 {
		var m struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Summary     string `json:"summary"`
			Kind        string `json:"kind"`
			Mode        string `json:"mode"`
		}
		_ = json.Unmarshal(manifest, &m)
		slug, displayName, summary, kind, mode = m.Slug, m.DisplayName, m.Summary, m.Kind, m.Mode
	}
	if slug == "" {
		slug = fmt.Sprintf("chain.%d", chainID)
	}
	if displayName == "" {
		displayName = slug
	}
	if kind == "" {
		kind = "data"
	}
	if mode == "" {
		if hosted {
			mode = "hosted"
		} else {
			mode = "proxy"
		}
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO services (
			chain_id, developer_id, slug, kind, mode, display_name, summary,
			manifest, manifest_hash, status, confidential
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',$10)
		ON CONFLICT (slug) DO UPDATE SET
			chain_id = EXCLUDED.chain_id,
			manifest_hash = EXCLUDED.manifest_hash,
			status = 'active',
			confidential = EXCLUDED.confidential,
			updated_at = now()
		RETURNING id::text`,
		chainID, devID, slug, kind, mode, displayName, summary,
		manifest, manifestHash, confidential,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: upsert from chain: %w", err)
	}
	_ = pricingHash
	return id, nil
}

// ListDiscoverable returns active services matching lexical query and filters.
func (s *Store) ListDiscoverable(ctx context.Context, query, kind string, limit int) ([]ServiceRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	q := `
		SELECT id::text, chain_id, developer_id::text, slug, kind, mode,
		       display_name, summary, manifest, manifest_hash, status,
		       confidential, quality_score::text, uptime_bps
		FROM services
		WHERE status = 'active'`
	args := []any{}
	n := 1
	if kind != "" {
		q += fmt.Sprintf(" AND kind = $%d", n)
		args = append(args, kind)
		n++
	}
	if query != "" {
		q += fmt.Sprintf(" AND (summary ILIKE $%d OR display_name ILIKE $%d OR slug ILIKE $%d)", n, n, n)
		args = append(args, "%"+query+"%")
		n++
	}
	q += fmt.Sprintf(" ORDER BY quality_score DESC NULLS LAST, display_name ASC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list discoverable: %w", err)
	}
	defer rows.Close()

	var out []ServiceRow
	for rows.Next() {
		var row ServiceRow
		var chainID *int64
		if err := rows.Scan(
			&row.ID, &chainID, &row.DeveloperID, &row.Slug, &row.Kind, &row.Mode,
			&row.DisplayName, &row.Summary, &row.Manifest, &row.ManifestHash, &row.Status,
			&row.Confidential, &row.QualityScore, &row.UptimeBPS,
		); err != nil {
			return nil, fmt.Errorf("store: scan discoverable: %w", err)
		}
		row.ChainID = chainID
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListPublishedServices returns active (published, listable) services for the
// public catalog, ordered by quality then name, with limit/offset pagination.
// It also returns the total count of active services for client paging.
func (s *Store) ListPublishedServices(ctx context.Context, limit, offset int) ([]ServiceRow, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM services WHERE status = 'active'`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count published: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, chain_id, developer_id::text, slug, kind, mode,
		       display_name, summary, manifest, manifest_hash, status,
		       confidential, quality_score::text, uptime_bps
		FROM services
		WHERE status = 'active'
		ORDER BY quality_score DESC NULLS LAST, display_name ASC
		LIMIT $1 OFFSET $2`, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list published: %w", err)
	}
	defer rows.Close()

	var out []ServiceRow
	for rows.Next() {
		var row ServiceRow
		var chainID *int64
		if err := rows.Scan(
			&row.ID, &chainID, &row.DeveloperID, &row.Slug, &row.Kind, &row.Mode,
			&row.DisplayName, &row.Summary, &row.Manifest, &row.ManifestHash, &row.Status,
			&row.Confidential, &row.QualityScore, &row.UptimeBPS,
		); err != nil {
			return nil, 0, fmt.Errorf("store: scan published: %w", err)
		}
		row.ChainID = chainID
		out = append(out, row)
	}
	return out, total, rows.Err()
}

// InsertEndpoints replaces endpoint rows for a service from manifest operations.
func (s *Store) InsertEndpoints(ctx context.Context, serviceID string, ops []EndpointRow) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM endpoints WHERE service_id = $1`, serviceID); err != nil {
		return fmt.Errorf("store: clear endpoints: %w", err)
	}
	for _, op := range ops {
		_, err := tx.Exec(ctx, `
			INSERT INTO endpoints (service_id, operation, method, input_schema, output_schema, proxy_url)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			serviceID, op.Operation, op.Method, op.InputSchema, op.OutputSchema, op.ProxyURL,
		)
		if err != nil {
			return fmt.Errorf("store: insert endpoint: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// EndpointRow is one callable operation mirror.
type EndpointRow struct {
	Operation    string
	Method       string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	ProxyURL     *string
}

// InsertPricingPlans replaces pricing rows for a service.
func (s *Store) InsertPricingPlans(ctx context.Context, serviceID string, plans []PricingRow) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM pricing_plans WHERE service_id = $1`, serviceID); err != nil {
		return fmt.Errorf("store: clear pricing: %w", err)
	}
	for _, p := range plans {
		_, err := tx.Exec(ctx, `
			INSERT INTO pricing_plans (service_id, model, unit, price_wei, min_charge_wei, version)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			serviceID, p.Model, p.Unit, p.PriceWei, p.MinChargeWei, p.Version,
		)
		if err != nil {
			return fmt.Errorf("store: insert pricing: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// PricingRow is a persisted pricing plan row.
type PricingRow struct {
	Model        string
	Unit         string
	PriceWei     string
	MinChargeWei string
	Version      int
}

// SetQualityScore updates the rolling quality score for a service.
func (s *Store) SetQualityScore(ctx context.Context, serviceID, score string) error {
	_, err := s.pool.Exec(ctx, `UPDATE services SET quality_score = $2::numeric, updated_at = now() WHERE id = $1`, serviceID, score)
	if err != nil {
		return fmt.Errorf("store: set quality score: %w", err)
	}
	return nil
}

// PricingByService returns pricing plans for discovery responses.
func (s *Store) PricingByService(ctx context.Context, serviceID string) ([]PricingRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT model, unit, price_wei, min_charge_wei, version
		FROM pricing_plans WHERE service_id = $1 ORDER BY version DESC`, serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: pricing by service: %w", err)
	}
	defer rows.Close()
	var out []PricingRow
	for rows.Next() {
		var p PricingRow
		if err := rows.Scan(&p.Model, &p.Unit, &p.PriceWei, &p.MinChargeWei, &p.Version); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
