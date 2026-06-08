package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Endpoint is a callable operation mirror with id.
type Endpoint struct {
	ID           string
	ServiceID    string
	Operation    string
	Method       string
	ProxyURL     *string
	InputSchema  []byte
	OutputSchema []byte
}

// EndpointsByService lists endpoints for a service.
func (s *Store) EndpointsByService(ctx context.Context, serviceID string) ([]Endpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, service_id::text, operation, method, proxy_url, input_schema, output_schema
		FROM endpoints WHERE service_id = $1 ORDER BY operation`, serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: endpoints: %w", err)
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		var ep Endpoint
		if err := rows.Scan(&ep.ID, &ep.ServiceID, &ep.Operation, &ep.Method, &ep.ProxyURL, &ep.InputSchema, &ep.OutputSchema); err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// EndpointByServiceOperation returns one endpoint row.
func (s *Store) EndpointByServiceOperation(ctx context.Context, serviceID, operation string) (Endpoint, error) {
	var ep Endpoint
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, service_id::text, operation, method, proxy_url, input_schema, output_schema
		FROM endpoints WHERE service_id = $1 AND operation = $2`, serviceID, operation,
	).Scan(&ep.ID, &ep.ServiceID, &ep.Operation, &ep.Method, &ep.ProxyURL, &ep.InputSchema, &ep.OutputSchema)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Endpoint{}, fmt.Errorf("store: endpoint not found")
		}
		return Endpoint{}, fmt.Errorf("store: endpoint: %w", err)
	}
	return ep, nil
}

// DeveloperPayoutByService returns payout address for a service's developer.
func (s *Store) DeveloperPayoutByService(ctx context.Context, serviceID string) (string, error) {
	var payout string
	err := s.pool.QueryRow(ctx, `
		SELECT d.payout_address
		FROM services s
		JOIN developers d ON d.id = s.developer_id
		WHERE s.id = $1`, serviceID,
	).Scan(&payout)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("store: service not found")
		}
		return "", fmt.Errorf("store: payout: %w", err)
	}
	return payout, nil
}
