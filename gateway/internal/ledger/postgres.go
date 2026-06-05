// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Postgres-backed Ledger implementation.
//
// Posture (sess#32 v1):
//   - The wire shape uses database/sql exclusively. Concrete drivers
//     are registered out-of-tree by the binary's main package via
//     blank-imports (e.g. `_ "github.com/lib/pq"`). The gateway
//     module itself stays driver-free so its `go.mod` remains
//     stdlib-only — a requirement of the leaf-module posture.
//   - Construction signature accepts a *sql.DB, NOT a connection
//     string. The CLI wires it: open the DB with the chosen driver
//     and inject. Tests can inject the in-memory Memory instead.
//   - The Postgres ledger never panics on a write error; errors
//     bubble up to the proxy which logs + decides whether to fail
//     the request (default: fail closed, return 500 to avoid silent
//     under-billing).
//
// TODO(sess#32+): when the box is wired in, swap to pgx pool
// (jackc/pgx/v5) for prepared-statement caching + structured err
// types. The interface stays the same; only postgres.go changes.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"matrix/gateway/internal/rates"
)

// Postgres is the Ledger implementation backed by database/sql.
type Postgres struct {
	db     *sql.DB
	defCap string
}

// NewPostgres returns a Ledger that talks to the supplied *sql.DB.
// defCap is the default daily cap applied when an actor has no row
// in daily_budget_caps; empty falls back to DefaultDailyPaxCap.
//
// The caller retains ownership of db.Close(); Close() on the returned
// Postgres is a no-op so callers can safely share one *sql.DB across
// the gateway lifetime + a future admin endpoint.
func NewPostgres(db *sql.DB, defCap string) *Postgres {
	if defCap == "" {
		defCap = DefaultDailyPaxCap
	}
	return &Postgres{db: db, defCap: defCap}
}

// Record inserts a row into credit_ledger. Idempotency notes:
// the schema does NOT enforce a unique key on (actor, intent, ts) so
// duplicates are accepted; downstream rollups MUST sum without DISTINCT.
// This is intentional — re-tries on transient network errors should
// be safe, and reconciliation is a problem for a future audit job.
func (p *Postgres) Record(ctx context.Context, e Entry) error {
	if e.RateTableVersion == 0 {
		e.RateTableVersion = rates.RateTableVersion
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	const q = `
		INSERT INTO credit_ledger (
			actor_did, intent_id, goal_id, model, slot, kind_route,
			tokens_input, tokens_output, cost_pax, rate_table_v, occurred_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	_, err := p.db.ExecContext(ctx, q,
		e.ActorDID,
		nullIfEmpty(e.IntentID),
		nullIfEmpty(e.GoalID),
		e.Model,
		nullIfEmpty(e.Slot),
		nullIfEmpty(e.KindRoute),
		e.TokensInput,
		e.TokensOutput,
		e.CostPax,
		e.RateTableVersion,
		e.OccurredAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("gateway.ledger.postgres: insert: %w", err)
	}
	return nil
}

// DailySpend returns the actor's PAX-denominated total spent on the
// UTC calendar day containing now. Uses NUMERIC arithmetic in Postgres
// so precision matches credit_ledger.cost_pax exactly.
func (p *Postgres) DailySpend(ctx context.Context, actor string, now time.Time) (string, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	const q = `
		SELECT COALESCE(SUM(cost_pax), 0)::text
		FROM credit_ledger
		WHERE actor_did = $1 AND occurred_at >= $2 AND occurred_at < $3
	`
	var spent sql.NullString
	if err := p.db.QueryRowContext(ctx, q, actor, dayStart, dayEnd).Scan(&spent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "0", nil
		}
		return "", fmt.Errorf("gateway.ledger.postgres: daily spend: %w", err)
	}
	if !spent.Valid || spent.String == "" {
		return "0", nil
	}
	return spent.String, nil
}

// DailyCap reads daily_budget_caps; returns the default when empty.
func (p *Postgres) DailyCap(ctx context.Context, actor string) (string, error) {
	const q = `SELECT daily_pax_max::text FROM daily_budget_caps WHERE actor_did = $1`
	var cap sql.NullString
	err := p.db.QueryRowContext(ctx, q, actor).Scan(&cap)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return p.defCap, nil
	case err != nil:
		return "", fmt.Errorf("gateway.ledger.postgres: daily cap: %w", err)
	}
	if !cap.Valid || cap.String == "" {
		return p.defCap, nil
	}
	return cap.String, nil
}

// Close is a no-op; the *sql.DB is owned by the caller. Returning nil
// keeps the Ledger interface contract trivial for callers using
// defer ledger.Close().
func (p *Postgres) Close() error { return nil }

// nullIfEmpty wraps a string into a sql.NullString to keep optional
// columns (intent/goal/slot/kind_route) NULL when unset, mirroring
// the schema's nullability.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Compile-time assertion.
var _ Ledger = (*Postgres)(nil)

// Copyright © 2026 Paxlabs Inc. All rights reserved.
