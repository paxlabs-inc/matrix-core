package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeploymentRow mirrors deployments table.
type DeploymentRow struct {
	ID                 string
	ServiceID          string
	AppwriteFunctionID *string
	SiteID             *string
	Runtime            string
	DeploymentID       *string
	ExecEndpoint       *string
	Status             string
	Region             *string
	LastInvokedAt      *time.Time
	AlwaysWarm         bool
	ArtifactKey        *string
	CreatedAt          time.Time
}

// InsertDeployment creates a deployment row.
func (s *Store) InsertDeployment(ctx context.Context, row DeploymentRow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO deployments (
			service_id, appwrite_function_id, site_id, runtime, deployment_id,
			exec_endpoint, status, region, always_warm
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id::text`,
		row.ServiceID, row.AppwriteFunctionID, row.SiteID, row.Runtime, row.DeploymentID,
		row.ExecEndpoint, row.Status, row.Region, row.AlwaysWarm,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert deployment: %w", err)
	}
	return id, nil
}

// ActiveDeploymentForService returns the latest active deployment for invoke routing.
func (s *Store) ActiveDeploymentForService(ctx context.Context, serviceID string) (DeploymentRow, error) {
	var row DeploymentRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, service_id::text, appwrite_function_id, site_id, runtime,
		       deployment_id, exec_endpoint, status, region, last_invoked_at, always_warm, created_at
		FROM deployments
		WHERE service_id = $1 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1`, serviceID,
	).Scan(
		&row.ID, &row.ServiceID, &row.AppwriteFunctionID, &row.SiteID, &row.Runtime,
		&row.DeploymentID, &row.ExecEndpoint, &row.Status, &row.Region, &row.LastInvokedAt,
		&row.AlwaysWarm, &row.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return DeploymentRow{}, fmt.Errorf("store: deployment not found")
		}
		return DeploymentRow{}, fmt.Errorf("store: active deployment: %w", err)
	}
	return row, nil
}

// ListDeploymentsForService returns all deployments for a service, newest first.
func (s *Store) ListDeploymentsForService(ctx context.Context, serviceID string, limit int) ([]DeploymentRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, service_id::text, appwrite_function_id, site_id, runtime,
		       deployment_id, exec_endpoint, status, region, last_invoked_at, always_warm, created_at
		FROM deployments
		WHERE service_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, serviceID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list deployments: %w", err)
	}
	defer rows.Close()

	var out []DeploymentRow
	for rows.Next() {
		var row DeploymentRow
		if err := rows.Scan(
			&row.ID, &row.ServiceID, &row.AppwriteFunctionID, &row.SiteID, &row.Runtime,
			&row.DeploymentID, &row.ExecEndpoint, &row.Status, &row.Region, &row.LastInvokedAt,
			&row.AlwaysWarm, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan deployment: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetDeployment loads a deployment by id.
func (s *Store) GetDeployment(ctx context.Context, id string) (DeploymentRow, error) {
	var row DeploymentRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, service_id::text, appwrite_function_id, site_id, runtime,
		       deployment_id, exec_endpoint, status, region, last_invoked_at, always_warm, created_at
		FROM deployments WHERE id = $1`, id,
	).Scan(
		&row.ID, &row.ServiceID, &row.AppwriteFunctionID, &row.SiteID, &row.Runtime,
		&row.DeploymentID, &row.ExecEndpoint, &row.Status, &row.Region, &row.LastInvokedAt,
		&row.AlwaysWarm, &row.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return DeploymentRow{}, fmt.Errorf("store: deployment not found")
		}
		return DeploymentRow{}, fmt.Errorf("store: get deployment: %w", err)
	}
	return row, nil
}

// UpdateDeploymentStatus sets status and optional exec endpoint.
func (s *Store) UpdateDeploymentStatus(ctx context.Context, id, status string, execEndpoint *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET status = $2, exec_endpoint = COALESCE($3, exec_endpoint)
		WHERE id = $1`, id, status, execEndpoint,
	)
	if err != nil {
		return fmt.Errorf("store: update deployment: %w", err)
	}
	return nil
}

// CountAlwaysWarmDeployments returns active always-warm hosted deployments.
func (s *Store) CountAlwaysWarmDeployments(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int FROM deployments
		WHERE status = 'active' AND always_warm = true`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count always warm: %w", err)
	}
	return n, nil
}

// TouchDeploymentInvoked updates last_invoked_at.
func (s *Store) TouchDeploymentInvoked(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET last_invoked_at = now() WHERE id = $1`, id,
	)
	return err
}

// SetDeploymentBackendIDs records Appwrite function and deployment ids after provision.
func (s *Store) SetDeploymentBackendIDs(ctx context.Context, id, functionID, backendDeploymentID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET appwrite_function_id = $2, deployment_id = $3
		WHERE id = $1`, id, functionID, backendDeploymentID,
	)
	if err != nil {
		return fmt.Errorf("store: set deployment backend ids: %w", err)
	}
	return nil
}

// DeactivateDeploymentsForService marks prior deployments inactive before a new active deploy.
func (s *Store) DeactivateDeploymentsForService(ctx context.Context, serviceID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET status = 'superseded'
		WHERE service_id = $1 AND status = 'active'`, serviceID,
	)
	if err != nil {
		return fmt.Errorf("store: deactivate deployments: %w", err)
	}
	return nil
}
