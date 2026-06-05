// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package db wraps Postgres queries used by matrix-router.
//
// Schema source of truth: deploy/box/postgres/schema.sql (idempotent,
// applied by bootstrap.sh).
//
// Two query surfaces:
//   - LookupForRoute     hot-path: JWT subject → user row needed for proxy
//   - admin.Provision*   slow-path: user creation, machine assignment,
//     lifecycle bookkeeping
//
// Connection pooling: pgxpool with sane defaults. ParseConfig accepts a
// libpq-style DATABASE_URL.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User mirrors the columns of the public.users table that matrix-router
// reads at request time. State is one of the closed enum values
// {provisioning, active, suspended, deleted, failed}.
type User struct {
	ID           string
	Email        string
	State        string
	FlyMachineID string
	FlyVolumeID  string
	FlyRegion    string
	S3AccessKey  string
	DailyBudget  int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastSeenAt   *time.Time
}

// User states. Keep aligned with the closed enum in schema.sql line 18.
const (
	StateProvisioning = "provisioning"
	StateActive       = "active"
	StateSuspended    = "suspended"
	StateDeleted      = "deleted"
	StateFailed       = "failed"
)

// ErrUserNotFound surfaces from LookupForRoute when no row matches.
var ErrUserNotFound = errors.New("db: user not found")

// DB is the matrix-router DB facade. Holds a pgxpool.Pool for cheap
// connection reuse across requests.
type DB struct {
	pool *pgxpool.Pool
}

// Open parses dsn (DATABASE_URL form) and opens a pool. Fails fast on
// Ping so misconfiguration surfaces at boot rather than first request.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases all pool connections. Safe to call once.
func (d *DB) Close() {
	if d.pool != nil {
		d.pool.Close()
	}
}

// Ping returns nil iff at least one DB connection is healthy.
func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

// LookupForRoute fetches the row needed to route a request: state,
// fly_machine_id, region. Bumps last_seen_at as a side effect (single
// row update, single connection acquisition).
//
// Returns ErrUserNotFound on no row.
func (d *DB) LookupForRoute(ctx context.Context, supabaseUserID string) (*User, error) {
	const q = `
		UPDATE users
		   SET last_seen_at = now()
		 WHERE id = $1
		   AND state IN ('active','provisioning')
		RETURNING id, email, state, COALESCE(fly_machine_id,''), COALESCE(fly_volume_id,''),
		          COALESCE(fly_region,''), COALESCE(s3_access_key,''), daily_token_budget,
		          created_at, updated_at, last_seen_at
	`
	var u User
	err := d.pool.QueryRow(ctx, q, supabaseUserID).Scan(
		&u.ID, &u.Email, &u.State,
		&u.FlyMachineID, &u.FlyVolumeID, &u.FlyRegion, &u.S3AccessKey,
		&u.DailyBudget, &u.CreatedAt, &u.UpdatedAt, &u.LastSeenAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("db: lookup user %s: %w", supabaseUserID, err)
	}
	return &u, nil
}

// CreateOrTouchUser inserts a row in 'provisioning' state if absent;
// otherwise updates email/handle. Returns whether a new row was made.
func (d *DB) CreateOrTouchUser(ctx context.Context, supabaseUserID, email, handle string) (created bool, err error) {
	const q = `
		INSERT INTO users (id, email, handle, state)
		VALUES ($1, $2, $3, 'provisioning')
		ON CONFLICT (id) DO UPDATE
		   SET email   = COALESCE(NULLIF(EXCLUDED.email,''), users.email),
		       handle  = COALESCE(NULLIF(EXCLUDED.handle,''), users.handle),
		       updated_at = now()
		 RETURNING (xmax = 0) AS inserted
	`
	err = d.pool.QueryRow(ctx, q, supabaseUserID, email, handle).Scan(&created)
	if err != nil {
		return false, fmt.Errorf("db: upsert user %s: %w", supabaseUserID, err)
	}
	return created, nil
}

// AttachMachine binds a fly machine + volume + region to a user and
// transitions the user state to active. Returns ErrUserNotFound if
// the user row vanished between provision-create and attach.
func (d *DB) AttachMachine(ctx context.Context, supabaseUserID, machineID, volumeID, region string) error {
	const q = `
		UPDATE users
		   SET fly_machine_id = $2,
		       fly_volume_id  = $3,
		       fly_region     = $4,
		       state          = 'active',
		       updated_at     = now()
		 WHERE id = $1
	`
	tag, err := d.pool.Exec(ctx, q, supabaseUserID, machineID, volumeID, region)
	if err != nil {
		return fmt.Errorf("db: attach machine %s: %w", supabaseUserID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetUserState transitions an existing user to the given closed enum
// state. Used by admin endpoints (suspend, restore, etc).
func (d *DB) SetUserState(ctx context.Context, supabaseUserID, newState string) error {
	const q = `UPDATE users SET state = $2, updated_at = now() WHERE id = $1`
	tag, err := d.pool.Exec(ctx, q, supabaseUserID, newState)
	if err != nil {
		return fmt.Errorf("db: state %s: %w", supabaseUserID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// QueueProvisionJob writes a row in provision_jobs with state=queued.
// Returns the new job id.
func (d *DB) QueueProvisionJob(ctx context.Context, userID, op string) (int64, error) {
	const q = `INSERT INTO provision_jobs (user_id, op) VALUES ($1, $2) RETURNING id`
	var id int64
	if err := d.pool.QueryRow(ctx, q, userID, op).Scan(&id); err != nil {
		return 0, fmt.Errorf("db: queue %s/%s: %w", userID, op, err)
	}
	return id, nil
}

// FinishProvisionJob updates a provision_jobs row to terminal state.
func (d *DB) FinishProvisionJob(ctx context.Context, jobID int64, state, errMsg string, response []byte) error {
	const q = `
		UPDATE provision_jobs
		   SET state = $2, error = $3, fly_response = $4::jsonb,
		       finished_at = now()
		 WHERE id = $1
	`
	if _, err := d.pool.Exec(ctx, q, jobID, state, errMsg, string(response)); err != nil {
		return fmt.Errorf("db: finish job %d: %w", jobID, err)
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
