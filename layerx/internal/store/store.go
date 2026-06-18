// Package store provides Postgres access and the migration runner for layerxd.
// The accounts + transfers tables are the authoritative off-chain ledger
// between settlements (layerx.frozen.kvx [ledger]); the vault contract is
// authoritative for reserve + final settled state.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when an account, transfer, or batch does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrInsufficientFunds is returned when a pay/withdraw exceeds the escrow-bounded
// spendable balance (layerx.frozen.kvx [escrow]).
var ErrInsufficientFunds = errors.New("store: insufficient funds")

// TransferPartitionSize is the seq span of each transfers RANGE partition
// (migration 005). Must match the initial partition bounds in the migration.
const TransferPartitionSize = 10_000_000

// Pool sizing defaults for the single always-on sequencer's high-write OLTP
// workload. Applied unless the connection URI already carries the matching
// pool_* param, so an operator can still override per-deploy.
const (
	defaultMaxConns        = 20
	defaultMinConns        = 2
	defaultMaxConnLifetime = time.Hour
	defaultMaxConnIdleTime = 30 * time.Minute
	// statement_timeout bounds any single query so a pathological statement
	// cannot pin a pooled connection indefinitely. Generous enough for the OLTP
	// queries + the (tiny-table) migrations that run through this same pool.
	defaultStatementTimeoutMS = "30000"
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a Postgres pool with sane sizing + liveness bounds and a
// per-statement timeout (each tunable via the URI's pool_* / options params).
func New(ctx context.Context, uri string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(uri)
	if err != nil {
		return nil, fmt.Errorf("store: parse uri: %w", err)
	}
	if !strings.Contains(uri, "pool_max_conns") {
		cfg.MaxConns = defaultMaxConns
	}
	if !strings.Contains(uri, "pool_min_conns") {
		cfg.MinConns = defaultMinConns
	}
	if !strings.Contains(uri, "pool_max_conn_lifetime") {
		cfg.MaxConnLifetime = defaultMaxConnLifetime
	}
	if !strings.Contains(uri, "pool_max_conn_idle_time") {
		cfg.MaxConnIdleTime = defaultMaxConnIdleTime
	}
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, set := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !set {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = defaultStatementTimeoutMS
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// EnsureTransferPartitions creates the seq-range partitions of the (partitioned)
// transfers table up to well ahead of the current high-water seq, so inserts
// always land in a concrete partition rather than the DEFAULT overflow safety
// net. Idempotent (CREATE ... IF NOT EXISTS), cheap (instant DDL), and safe to
// call at boot + on a periodic ticker. A no-op on PG installs where transfers is
// not yet partitioned (pre-005): the CREATE ... PARTITION OF would error, so we
// first confirm the table is partitioned.
func (s *Store) EnsureTransferPartitions(ctx context.Context) error {
	var partitioned bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_partitioned_table pt
			JOIN pg_class c ON c.oid = pt.partrelid
			WHERE c.relname = 'transfers')`).Scan(&partitioned); err != nil {
		return fmt.Errorf("store: check transfers partitioning: %w", err)
	}
	if !partitioned {
		return nil
	}
	var maxSeq int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM transfers`).Scan(&maxSeq); err != nil {
		return fmt.Errorf("store: read max seq: %w", err)
	}
	// Cover the current range plus the next two, so we are always ahead.
	target := maxSeq + 2*TransferPartitionSize
	for lo := int64(1); lo <= target; lo += TransferPartitionSize {
		hi := lo + TransferPartitionSize
		idx := lo / TransferPartitionSize // lo=1 -> p0, 10000001 -> p1, ...
		name := fmt.Sprintf("transfers_p%d", idx)
		q := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF transfers FOR VALUES FROM (%d) TO (%d)`,
			name, lo, hi)
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("store: ensure partition %s: %w", name, err)
		}
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Migrate applies forward-only SQL files from dir in lexical order, using a
// dedicated layerx_schema_migrations ledger.
func (s *Store) Migrate(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("store: read migrations dir: %w", err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("store: no migrations in %s", dir)
	}

	if err := s.ensureMigrationTable(ctx); err != nil {
		return err
	}

	for _, name := range files {
		applied, err := s.isApplied(ctx, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		path := filepath.Join(dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("store: begin tx for %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO layerx_schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: commit %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) ensureMigrationTable(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS layerx_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("store: ensure layerx_schema_migrations: %w", err)
	}
	return nil
}

func (s *Store) isApplied(ctx context.Context, version string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(1) FROM layerx_schema_migrations WHERE version = $1`, version).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: check migration %s: %w", version, err)
	}
	return n > 0, nil
}
