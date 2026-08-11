// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

func applyMigrations(ctx context.Context, db *sql.DB, now time.Time) error {
	available, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, item := range available {
		applied, err := migrationApplied(ctx, db, item.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("turnstate: begin migration %q: %w", item.name, err)
		}
		if _, err := tx.ExecContext(ctx, item.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("turnstate: execute migration %q: %w", item.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			item.version, now.UnixMicro(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("turnstate: record migration %q: %w", item.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("turnstate: commit migration %q: %w", item.name, err)
		}
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("turnstate: list migrations: %w", err)
	}
	available := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		prefix, _, found := strings.Cut(entry.Name(), "_")
		version, parseErr := strconv.Atoi(prefix)
		if !found || parseErr != nil || version <= 0 {
			return nil, fmt.Errorf("turnstate: invalid migration name %q", entry.Name())
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("turnstate: read migration %q: %w", entry.Name(), err)
		}
		available = append(available, migration{
			version: version, name: entry.Name(), sql: string(content),
		})
	}
	sort.Slice(available, func(left, right int) bool {
		return available[left].version < available[right].version
	})
	return available, nil
}

func migrationApplied(ctx context.Context, db *sql.DB, version int) (bool, error) {
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type = 'table' AND name = 'schema_migrations'`,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("turnstate: inspect migration table: %w", err)
	}
	if exists == 0 {
		return false, nil
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("turnstate: inspect migration version: %w", err)
	}
	return count == 1, nil
}
