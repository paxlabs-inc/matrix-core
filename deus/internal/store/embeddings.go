package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// UpsertEmbedding stores or replaces the search vector for a service.
func (s *Store) UpsertEmbedding(ctx context.Context, serviceID, model string, vec []float32) error {
	v := pgvector.NewVector(vec)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO embeddings (service_id, model, vec)
		VALUES ($1, $2, $3)
		ON CONFLICT (service_id) DO UPDATE SET model = EXCLUDED.model, vec = EXCLUDED.vec`,
		serviceID, model, v,
	)
	if err != nil {
		return fmt.Errorf("store: upsert embedding: %w", err)
	}
	return nil
}

// SetSearchDocument updates the lexical search document for a service.
func (s *Store) SetSearchDocument(ctx context.Context, serviceID, document string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE services SET search_document = to_tsvector('english', $2)
		WHERE id = $1`, serviceID, document,
	)
	if err != nil {
		return fmt.Errorf("store: set search document: %w", err)
	}
	return nil
}

// DiscoverCandidate is a retrieval hit before final ranking.
type DiscoverCandidate struct {
	ServiceRow
	SemanticSim float64
	LexicalRank float64
}

// VectorSearchDiscover returns top-K active services by cosine similarity.
func (s *Store) VectorSearchDiscover(ctx context.Context, queryVec []float32, kind string, limit int) ([]DiscoverCandidate, error) {
	if limit <= 0 {
		limit = 20
	}
	v := pgvector.NewVector(queryVec)
	q := `
		SELECT s.id::text, s.chain_id, s.developer_id::text, s.slug, s.kind, s.mode,
		       s.display_name, s.summary, s.manifest, s.manifest_hash, s.status,
		       s.confidential, s.quality_score::text, s.uptime_bps,
		       1 - (e.vec <=> $1) AS semantic_sim
		FROM embeddings e
		JOIN services s ON s.id = e.service_id
		WHERE s.status = 'active'`
	args := []any{v}
	n := 2
	if kind != "" {
		q += fmt.Sprintf(" AND s.kind = $%d", n)
		args = append(args, kind)
		n++
	}
	q += fmt.Sprintf(" ORDER BY e.vec <=> $1 LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: vector search: %w", err)
	}
	defer rows.Close()
	var out []DiscoverCandidate
	for rows.Next() {
		c, err := scanServiceCandidate(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LexicalSearchDiscover returns top-K by ts_rank on search_document.
func (s *Store) LexicalSearchDiscover(ctx context.Context, query, kind string, limit int) ([]DiscoverCandidate, error) {
	if limit <= 0 {
		limit = 20
	}
	if query == "" {
		return s.ListDiscoverCandidates(ctx, kind, limit)
	}
	q := `
		SELECT s.id::text, s.chain_id, s.developer_id::text, s.slug, s.kind, s.mode,
		       s.display_name, s.summary, s.manifest, s.manifest_hash, s.status,
		       s.confidential, s.quality_score::text, s.uptime_bps,
		       ts_rank(s.search_document, websearch_to_tsquery('english', $1)) AS lex_rank
		FROM services s
		WHERE s.status = 'active'
		  AND s.search_document @@ websearch_to_tsquery('english', $1)`
	args := []any{query}
	n := 2
	if kind != "" {
		q += fmt.Sprintf(" AND s.kind = $%d", n)
		args = append(args, kind)
		n++
	}
	q += fmt.Sprintf(" ORDER BY lex_rank DESC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: lexical search: %w", err)
	}
	defer rows.Close()
	var out []DiscoverCandidate
	for rows.Next() {
		c, err := scanServiceCandidate(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListDiscoverCandidates returns active services for filter-only browse.
func (s *Store) ListDiscoverCandidates(ctx context.Context, kind string, limit int) ([]DiscoverCandidate, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `
		SELECT s.id::text, s.chain_id, s.developer_id::text, s.slug, s.kind, s.mode,
		       s.display_name, s.summary, s.manifest, s.manifest_hash, s.status,
		       s.confidential, s.quality_score::text, s.uptime_bps
		FROM services s
		WHERE s.status = 'active'`
	args := []any{}
	n := 1
	if kind != "" {
		q += fmt.Sprintf(" AND s.kind = $%d", n)
		args = append(args, kind)
		n++
	}
	q += fmt.Sprintf(" ORDER BY s.quality_score DESC NULLS LAST, s.display_name ASC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list discover candidates: %w", err)
	}
	defer rows.Close()
	var out []DiscoverCandidate
	for rows.Next() {
		var row ServiceRow
		var chainID *int64
		if err := rows.Scan(
			&row.ID, &chainID, &row.DeveloperID, &row.Slug, &row.Kind, &row.Mode,
			&row.DisplayName, &row.Summary, &row.Manifest, &row.ManifestHash, &row.Status,
			&row.Confidential, &row.QualityScore, &row.UptimeBPS,
		); err != nil {
			return nil, err
		}
		row.ChainID = chainID
		out = append(out, DiscoverCandidate{ServiceRow: row})
	}
	return out, rows.Err()
}

// MinPriceWeiForService returns the lowest operation price for filter checks.
func (s *Store) MinPriceWeiForService(ctx context.Context, serviceID string) (string, error) {
	var minPrice *string
	err := s.pool.QueryRow(ctx, `
		SELECT MIN(price_wei::numeric)::text
		FROM pricing_plans WHERE service_id = $1`, serviceID,
	).Scan(&minPrice)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("store: min price: %w", err)
	}
	if minPrice == nil {
		return "", nil
	}
	return *minPrice, nil
}

func scanServiceCandidate(rows pgx.Rows, semantic bool) (DiscoverCandidate, error) {
	var row ServiceRow
	var chainID *int64
	var score float64
	if err := rows.Scan(
		&row.ID, &chainID, &row.DeveloperID, &row.Slug, &row.Kind, &row.Mode,
		&row.DisplayName, &row.Summary, &row.Manifest, &row.ManifestHash, &row.Status,
		&row.Confidential, &row.QualityScore, &row.UptimeBPS, &score,
	); err != nil {
		return DiscoverCandidate{}, err
	}
	row.ChainID = chainID
	c := DiscoverCandidate{ServiceRow: row}
	if semantic {
		c.SemanticSim = score
	} else {
		c.LexicalRank = score
	}
	return c, nil
}
