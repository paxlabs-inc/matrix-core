package ledger

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationLockID int64 = 8_729_871_126_387_411_009

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

// ApplyMigrations applies each embedded PostgreSQL migration exactly once and
// rejects any checksum drift in a previously applied version.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	if pool == nil {
		return fmt.Errorf("ledger migrations: pool is required")
	}
	if now.IsZero() || now.Location() != time.UTC {
		return fmt.Errorf("ledger migrations: now must be a non-zero UTC timestamp")
	}
	available, err := loadMigrations()
	if err != nil {
		return err
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("ledger migrations: acquire connection: %w", err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("ledger migrations: acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()
	if _, err := connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS workforce_schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			checksum CHAR(64) NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL
		)`); err != nil {
		return fmt.Errorf("ledger migrations: create migration table: %w", err)
	}
	for _, item := range available {
		if err := applyMigration(ctx, connection, item, now); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(
	ctx context.Context,
	connection *pgxpool.Conn,
	item migration,
	now time.Time,
) error {
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("ledger migration %q: begin: %w", item.name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var existingChecksum string
	err = tx.QueryRow(ctx,
		`SELECT checksum FROM workforce_schema_migrations WHERE version = $1`,
		item.version,
	).Scan(&existingChecksum)
	switch {
	case err == nil:
		if existingChecksum != item.checksum {
			return fmt.Errorf(
				"ledger migration %q: checksum drift: applied %s, embedded %s",
				item.name,
				existingChecksum,
				item.checksum,
			)
		}
		return tx.Commit(ctx)
	case err != pgx.ErrNoRows:
		return fmt.Errorf("ledger migration %q: inspect version: %w", item.name, err)
	}
	if _, err := tx.Exec(ctx, item.sql); err != nil {
		return fmt.Errorf("ledger migration %q: execute: %w", item.name, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_schema_migrations(version, name, checksum, applied_at)
		VALUES ($1, $2, $3, $4)
	`, item.version, item.name, item.checksum, now); err != nil {
		return fmt.Errorf("ledger migration %q: record: %w", item.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ledger migration %q: commit: %w", item.name, err)
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	return loadMigrationsFrom(migrationFiles, "migrations")
}

func loadMigrationsFrom(files fs.FS, directory string) ([]migration, error) {
	entries, err := fs.ReadDir(files, directory)
	if err != nil {
		return nil, fmt.Errorf("ledger migrations: list embedded files: %w", err)
	}
	available := make([]migration, 0, len(entries))
	seenVersions := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		prefix, _, found := strings.Cut(entry.Name(), "_")
		version, parseErr := strconv.Atoi(prefix)
		if !found || parseErr != nil || version <= 0 {
			return nil, fmt.Errorf("ledger migrations: invalid file name %q", entry.Name())
		}
		if previous, duplicate := seenVersions[version]; duplicate {
			return nil, fmt.Errorf(
				"ledger migrations: version %d is duplicated by %q and %q",
				version,
				previous,
				entry.Name(),
			)
		}
		content, err := fs.ReadFile(files, directory+"/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("ledger migrations: read %q: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(content)
		available = append(available, migration{
			version:  version,
			name:     entry.Name(),
			checksum: hex.EncodeToString(sum[:]),
			sql:      string(content),
		})
		seenVersions[version] = entry.Name()
	}
	sort.Slice(available, func(left, right int) bool {
		return available[left].version < available[right].version
	})
	return available, nil
}
