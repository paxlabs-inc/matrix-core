package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// QuoteRow is a persisted signed quote.
type QuoteRow struct {
	ID              string
	ServiceID       string
	EndpointID      string
	PricingVersion  int
	UnitPriceWei    string
	MaxUnits        string
	ExpiresAt       time.Time
	Signature       string
	CallerDID       string
	Digest          string
}

// InsertQuote persists a quote.
func (s *Store) InsertQuote(ctx context.Context, q QuoteRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO quotes (
			service_id, endpoint_id, pricing_version, unit_price_wei, max_units,
			expires_at, signature, caller_did
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id::text`,
		q.ServiceID, q.EndpointID, q.PricingVersion, q.UnitPriceWei, q.MaxUnits,
		q.ExpiresAt, q.Signature, q.CallerDID,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert quote: %w", err)
	}
	return id, nil
}

// GetQuote loads a quote by id.
func (s *Store) GetQuote(ctx context.Context, id string) (QuoteRow, error) {
	var q QuoteRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, service_id::text, endpoint_id::text, pricing_version,
		       unit_price_wei, max_units, expires_at, signature, caller_did
		FROM quotes WHERE id = $1`, id,
	).Scan(&q.ID, &q.ServiceID, &q.EndpointID, &q.PricingVersion,
		&q.UnitPriceWei, &q.MaxUnits, &q.ExpiresAt, &q.Signature, &q.CallerDID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return QuoteRow{}, fmt.Errorf("store: quote not found")
		}
		return QuoteRow{}, fmt.Errorf("store: get quote: %w", err)
	}
	return q, nil
}
