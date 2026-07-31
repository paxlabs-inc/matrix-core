// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"matrix/router/internal/exa"
)

func (d *DB) PutRun(ctx context.Context, record exa.RunRecord) error {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	const query = `
		INSERT INTO exa_research_runs(id,user_id,workflow,subject,cache_key,status,cost_dollars,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status,cost_dollars=EXCLUDED.cost_dollars,updated_at=EXCLUDED.updated_at
		WHERE exa_research_runs.user_id=EXCLUDED.user_id`
	_, err := d.pool.Exec(ctx, query, record.ID, record.User, record.Workflow, record.Subject, record.CacheKey, record.Status, record.Cost, record.CreatedAt.UTC(), record.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("db: put exa run: %w", err)
	}
	return nil
}

func (d *DB) GetRun(ctx context.Context, user, id string) (exa.RunRecord, bool, error) {
	const query = `SELECT id,user_id,workflow,subject,cache_key,status,cost_dollars,created_at,updated_at FROM exa_research_runs WHERE id=$1 AND user_id=$2`
	var record exa.RunRecord
	err := d.pool.QueryRow(ctx, query, id, user).Scan(&record.ID, &record.User, &record.Workflow, &record.Subject, &record.CacheKey, &record.Status, &record.Cost, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return exa.RunRecord{}, false, nil
	}
	if err != nil {
		return exa.RunRecord{}, false, fmt.Errorf("db: get exa run: %w", err)
	}
	return record, true, nil
}

func (d *DB) ActiveRuns(ctx context.Context, user string) (int, error) {
	const query = `SELECT count(*) FROM exa_research_runs WHERE user_id=$1 AND status IN ('queued','running')`
	var count int
	if err := d.pool.QueryRow(ctx, query, user).Scan(&count); err != nil {
		return 0, fmt.Errorf("db: count active exa runs: %w", err)
	}
	return count, nil
}

func (d *DB) PutCache(ctx context.Context, record exa.CacheRecord) error {
	const query = `
		INSERT INTO exa_research_cache(user_id,cache_key,payload,cost_dollars,expires_at)
		VALUES($1,$2,$3::jsonb,$4,$5)
		ON CONFLICT(user_id,cache_key) DO UPDATE
		SET payload=EXCLUDED.payload,cost_dollars=EXCLUDED.cost_dollars,expires_at=EXCLUDED.expires_at,created_at=now()`
	if _, err := d.pool.Exec(ctx, query, record.User, record.Key, string(record.Payload), record.Cost, record.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("db: put exa cache: %w", err)
	}
	return nil
}

func (d *DB) GetCache(ctx context.Context, user, key string, now time.Time) (exa.CacheRecord, bool, error) {
	const query = `SELECT cache_key,user_id,payload,cost_dollars,expires_at FROM exa_research_cache WHERE user_id=$1 AND cache_key=$2 AND expires_at>$3`
	var record exa.CacheRecord
	err := d.pool.QueryRow(ctx, query, user, key, now.UTC()).Scan(&record.Key, &record.User, &record.Payload, &record.Cost, &record.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return exa.CacheRecord{}, false, nil
	}
	if err != nil {
		return exa.CacheRecord{}, false, fmt.Errorf("db: get exa cache: %w", err)
	}
	return record, true, nil
}

func (d *DB) ReserveSpend(ctx context.Context, user string, day time.Time, amount, limit float64) (bool, error) {
	const query = `
		INSERT INTO exa_daily_spend(user_id,spend_day,reserved_dollars)
		VALUES($1,$2,$3)
		ON CONFLICT(user_id,spend_day) DO UPDATE
		SET reserved_dollars=exa_daily_spend.reserved_dollars+EXCLUDED.reserved_dollars,updated_at=now()
		WHERE exa_daily_spend.reserved_dollars+EXCLUDED.reserved_dollars<=$4
		RETURNING reserved_dollars`
	var reserved float64
	err := d.pool.QueryRow(ctx, query, user, day.UTC().Format("2006-01-02"), amount, limit).Scan(&reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: reserve exa spend: %w", err)
	}
	return true, nil
}
