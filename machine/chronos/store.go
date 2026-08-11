// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package chronos

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"matrix/vault"

	robfigcron "github.com/robfig/cron/v3"
	_ "modernc.org/sqlite"
)

const (
	storeSchema = "alarm-body.v1"
	storeName   = "machine.chronos"
)

type Config struct {
	Path        string
	MachineGene string
	Vault       *vault.UserVault
	Now         func() time.Time
}

type Store struct {
	mu          sync.Mutex
	db          *sql.DB
	path        string
	machineGene string
	vault       *vault.UserVault
	now         func() time.Time
	closed      bool
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	cfg.Path = filepath.Clean(strings.TrimSpace(cfg.Path))
	cfg.MachineGene = strings.TrimSpace(cfg.MachineGene)
	if cfg.Path == "." || cfg.MachineGene == "" || cfg.Vault == nil {
		return nil, fmt.Errorf("local chronos: path, machine Gene, and encrypting Vault are required")
	}
	directory := filepath.Dir(cfg.Path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: cfg.Path}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("local chronos: open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: cfg.Path, machineGene: cfg.MachineGene, vault: cfg.Vault, now: cfg.Now}
	if store.now == nil {
		store.now = time.Now
	}
	if err := store.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.secureSQLiteFiles(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) initialize(ctx context.Context) error {
	var journalMode string
	if err := store.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&journalMode); err != nil {
		return fmt.Errorf("local chronos: enable WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("local chronos: SQLite refused WAL mode: %s", journalMode)
	}
	for _, statement := range []string{
		`PRAGMA synchronous=FULL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
            version INTEGER PRIMARY KEY,
            applied_at_us INTEGER NOT NULL
        ) STRICT`,
		`CREATE TABLE IF NOT EXISTS alarms (
            id TEXT PRIMARY KEY,
            idempotency_key TEXT NOT NULL UNIQUE,
            next_fire_us INTEGER NOT NULL,
            interval_ns INTEGER NOT NULL CHECK(interval_ns >= 0),
            cron_expr TEXT NOT NULL DEFAULT '',
            timezone TEXT NOT NULL DEFAULT 'UTC',
            misfire_policy TEXT NOT NULL CHECK(misfire_policy IN ('fire_once','skip','coalesce')),
			status TEXT NOT NULL CHECK(status IN ('scheduled','leased','completed','canceled','skipped','failed')),
            sealed_body BLOB NOT NULL,
            lease_token TEXT,
            lease_until_us INTEGER,
			occurrence_us INTEGER,
			override_next_us INTEGER,
            delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK(delivery_attempts >= 0),
            last_error TEXT NOT NULL DEFAULT '',
            last_fired_us INTEGER,
            created_at_us INTEGER NOT NULL,
            updated_at_us INTEGER NOT NULL,
            CHECK((status = 'leased') = (lease_token IS NOT NULL AND lease_until_us IS NOT NULL))
        ) STRICT`,
		`CREATE INDEX IF NOT EXISTS alarms_due_idx ON alarms(status, next_fire_us)`,
		`CREATE TABLE IF NOT EXISTS import_mappings (
			source TEXT NOT NULL,
			source_id TEXT NOT NULL,
			local_id TEXT NOT NULL REFERENCES alarms(id),
			created_at_us INTEGER NOT NULL,
			PRIMARY KEY(source,source_id)
		) STRICT`,
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at_us) VALUES (1, CAST(unixepoch('subsec') * 1000000 AS INTEGER))`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("local chronos: initialize schema: %w", err)
		}
	}
	return nil
}

func (store *Store) Create(ctx context.Context, request CreateRequest) (Alarm, bool, error) {
	request.ID = strings.TrimSpace(request.ID)
	providedID := request.ID != ""
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.CronExpr = strings.TrimSpace(request.CronExpr)
	request.Timezone = strings.TrimSpace(request.Timezone)
	if request.Timezone == "" {
		request.Timezone = "UTC"
	}
	if request.NextFire.IsZero() && request.CronExpr != "" {
		now := store.now().UTC()
		next, err := nextScheduled(request.Interval, request.CronExpr, request.Timezone, now, now)
		if err != nil {
			return Alarm{}, false, err
		}
		request.NextFire = next
	}
	request.NextFire = request.NextFire.UTC()
	if request.ID == "" {
		var err error
		request.ID, err = randomID("alarm")
		if err != nil {
			return Alarm{}, false, err
		}
	}
	if err := validateCreate(request); err != nil {
		return Alarm{}, false, err
	}
	bodyBytes, err := canonicalBody(request.Body)
	if err != nil {
		return Alarm{}, false, err
	}
	defer zero(bodyBytes)
	sealed, err := store.vault.SealRecord(store.ad(request.ID), bodyBytes)
	if err != nil {
		return Alarm{}, false, fmt.Errorf("local chronos: seal alarm body: %w", err)
	}
	defer zero(sealed)
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return Alarm{}, false, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return Alarm{}, false, fmt.Errorf("local chronos: begin create: %w", err)
	}
	defer tx.Rollback()
	existing, err := store.alarmByIdempotency(ctx, tx, request.IdempotencyKey)
	if err == nil {
		if !sameCreate(existing, request, providedID) {
			return Alarm{}, false, ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return Alarm{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Alarm{}, false, err
	}
	now := store.now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO alarms(
		id,idempotency_key,next_fire_us,interval_ns,cron_expr,timezone,misfire_policy,status,sealed_body,created_at_us,updated_at_us
		) VALUES(?,?,?,?,?,?,?,'scheduled',?,?,?)`, request.ID, request.IdempotencyKey,
		request.NextFire.UnixMicro(), int64(request.Interval), request.CronExpr, request.Timezone, string(request.MisfirePolicy), sealed,
		now.UnixMicro(), now.UnixMicro())
	if err != nil {
		return Alarm{}, false, fmt.Errorf("local chronos: insert alarm: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Alarm{}, false, fmt.Errorf("local chronos: commit create: %w", err)
	}
	return Alarm{ID: request.ID, IdempotencyKey: request.IdempotencyKey, NextFire: request.NextFire,
		Interval: request.Interval, CronExpr: request.CronExpr, Timezone: request.Timezone,
		MisfirePolicy: request.MisfirePolicy, Status: StatusScheduled,
		Body: request.Body, CreatedAt: now, UpdatedAt: now}, true, nil
}

func (store *Store) List(ctx context.Context) ([]Alarm, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT `+alarmColumns+` FROM alarms ORDER BY next_fire_us,id`)
	if err != nil {
		return nil, fmt.Errorf("local chronos: list alarms: %w", err)
	}
	defer rows.Close()
	var alarms []Alarm
	for rows.Next() {
		alarm, err := store.scanAlarm(rows)
		if err != nil {
			return nil, err
		}
		alarms = append(alarms, alarm)
	}
	return alarms, rows.Err()
}

func (store *Store) FindByIdempotency(ctx context.Context, key string) (Alarm, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return Alarm{}, err
	}
	return store.scanAlarm(store.db.QueryRowContext(ctx, `SELECT `+alarmColumns+` FROM alarms WHERE idempotency_key=?`, strings.TrimSpace(key)))
}

func (store *Store) importMapping(ctx context.Context, source, sourceID string) (string, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var localID string
	err := store.db.QueryRowContext(ctx, `SELECT local_id FROM import_mappings WHERE source=? AND source_id=?`, source, sourceID).Scan(&localID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return localID, err == nil, err
}

func (store *Store) recordImportMapping(ctx context.Context, source, sourceID, localID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	_, err := store.db.ExecContext(ctx, `INSERT INTO import_mappings(source,source_id,local_id,created_at_us)
		VALUES(?,?,?,?) ON CONFLICT(source,source_id) DO UPDATE SET local_id=excluded.local_id`,
		source, sourceID, localID, store.now().UTC().UnixMicro())
	return err
}

func (store *Store) Cancel(ctx context.Context, id string) (bool, error) {
	return store.mutate(ctx, id, `UPDATE alarms SET status='canceled',lease_token=NULL,lease_until_us=NULL,
		occurrence_us=NULL,override_next_us=NULL,updated_at_us=? WHERE id=? AND status NOT IN ('canceled','completed','skipped')`, store.now().UTC().UnixMicro())
}

func (store *Store) Reschedule(ctx context.Context, id string, next time.Time) (bool, error) {
	if next.IsZero() {
		return false, fmt.Errorf("local chronos: next fire is required")
	}
	id = strings.TrimSpace(id)
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return false, err
	}
	result, err := store.db.ExecContext(ctx, `UPDATE alarms SET
		next_fire_us=CASE WHEN status='leased' THEN next_fire_us ELSE ? END,
		override_next_us=CASE WHEN status='leased' THEN ? ELSE NULL END,
		status=CASE WHEN status='leased' THEN status ELSE 'scheduled' END,
		lease_token=CASE WHEN status='leased' THEN lease_token ELSE NULL END,
		lease_until_us=CASE WHEN status='leased' THEN lease_until_us ELSE NULL END,
		occurrence_us=CASE WHEN status='leased' THEN occurrence_us ELSE NULL END,
		last_error='',updated_at_us=? WHERE id=? AND status NOT IN ('completed','canceled','skipped','failed')`,
		next.UTC().UnixMicro(), next.UTC().UnixMicro(), store.now().UTC().UnixMicro(), id)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed > 0 {
		return true, nil
	}
	var exists int
	if err := store.db.QueryRowContext(ctx, `SELECT 1 FROM alarms WHERE id=?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	return false, nil
}

func (store *Store) mutate(ctx context.Context, id, query string, values ...any) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("local chronos: alarm id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return false, err
	}
	values = append(values, id)
	result, err := store.db.ExecContext(ctx, query, values...)
	if err != nil {
		return false, fmt.Errorf("local chronos: mutate alarm: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed > 0 {
		return true, nil
	}
	var exists int
	if err := store.db.QueryRowContext(ctx, `SELECT 1 FROM alarms WHERE id=?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	return false, nil
}

func (store *Store) Recover(ctx context.Context, now time.Time) (Recovery, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return Recovery{}, err
	}
	now = now.UTC()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return Recovery{}, err
	}
	defer tx.Rollback()
	recovery := Recovery{}
	result, err := tx.ExecContext(ctx, `UPDATE alarms SET status='scheduled',next_fire_us=COALESCE(override_next_us,next_fire_us),
		override_next_us=NULL,lease_token=NULL,lease_until_us=NULL,updated_at_us=?
		WHERE status='leased' AND lease_until_us<=?`, now.UnixMicro(), now.UnixMicro())
	if err != nil {
		return Recovery{}, err
	}
	recovered, _ := result.RowsAffected()
	recovery.RecoveredLeases = int(recovered)
	rows, err := tx.QueryContext(ctx, `SELECT id,next_fire_us,interval_ns,cron_expr,timezone,misfire_policy FROM alarms
        WHERE status='scheduled' AND next_fire_us<? ORDER BY next_fire_us`, now.UnixMicro())
	if err != nil {
		return Recovery{}, err
	}
	type overdue struct {
		id             string
		next, interval int64
		cronExpr       string
		timezone       string
		policy         string
	}
	var due []overdue
	for rows.Next() {
		var item overdue
		if err := rows.Scan(&item.id, &item.next, &item.interval, &item.cronExpr, &item.timezone, &item.policy); err != nil {
			rows.Close()
			return Recovery{}, err
		}
		due = append(due, item)
	}
	if err := rows.Close(); err != nil {
		return Recovery{}, err
	}
	for _, item := range due {
		switch MisfirePolicy(item.policy) {
		case MisfireSkip:
			if item.interval == 0 && item.cronExpr == "" {
				_, err = tx.ExecContext(ctx, `UPDATE alarms SET status='skipped',updated_at_us=? WHERE id=?`, now.UnixMicro(), item.id)
			} else {
				next, nextErr := nextScheduled(time.Duration(item.interval), item.cronExpr, item.timezone, time.UnixMicro(item.next), now)
				if nextErr != nil {
					return Recovery{}, nextErr
				}
				_, err = tx.ExecContext(ctx, `UPDATE alarms SET next_fire_us=?,updated_at_us=? WHERE id=?`, next.UnixMicro(), now.UnixMicro(), item.id)
			}
			if err != nil {
				return Recovery{}, err
			}
			recovery.Skipped++
		case MisfireFireOnce, MisfireCoalesce:
			recovery.Coalesced++
		default:
			return Recovery{}, fmt.Errorf("local chronos: invalid persisted misfire policy %q", item.policy)
		}
	}
	if err := tx.Commit(); err != nil {
		return Recovery{}, err
	}
	return recovery, nil
}

func (store *Store) ClaimDue(ctx context.Context, now time.Time, lease time.Duration) (*Claim, error) {
	if lease <= 0 {
		return nil, fmt.Errorf("local chronos: positive lease duration is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT `+alarmColumns+` FROM alarms
        WHERE status='scheduled' AND next_fire_us<=? ORDER BY next_fire_us,id LIMIT 1`, now.UTC().UnixMicro())
	alarm, err := store.scanAlarm(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	token, err := randomID("lease")
	if err != nil {
		return nil, err
	}
	leaseUntil := now.UTC().Add(lease)
	result, err := tx.ExecContext(ctx, `UPDATE alarms SET status='leased',lease_token=?,lease_until_us=?,
		occurrence_us=COALESCE(occurrence_us,next_fire_us),delivery_attempts=delivery_attempts+1,updated_at_us=?
        WHERE id=? AND status='scheduled'`, token, leaseUntil.UnixMicro(), now.UTC().UnixMicro(), alarm.ID)
	if err != nil {
		return nil, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return nil, fmt.Errorf("local chronos: due claim lost")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	alarm.Status = StatusLeased
	alarm.LeaseUntil = leaseUntil
	alarm.DeliveryAttempts++
	return &Claim{Alarm: alarm, LeaseToken: token, ScheduledFor: alarm.NextFire}, nil
}

func (store *Store) Acknowledge(ctx context.Context, id, token string, at time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var interval, occurrence int64
	var cronExpr, timezone string
	var override sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT interval_ns,cron_expr,timezone,occurrence_us,override_next_us FROM alarms
		WHERE id=? AND status='leased' AND lease_token=?`, id, token).Scan(&interval, &cronExpr, &timezone, &occurrence, &override); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLease
		}
		return err
	}
	at = at.UTC()
	if override.Valid {
		_, err = tx.ExecContext(ctx, `UPDATE alarms SET status='scheduled',next_fire_us=?,override_next_us=NULL,
			lease_token=NULL,lease_until_us=NULL,occurrence_us=NULL,last_fired_us=?,last_error='',updated_at_us=? WHERE id=?`,
			override.Int64, at.UnixMicro(), at.UnixMicro(), id)
	} else if interval == 0 && cronExpr == "" {
		_, err = tx.ExecContext(ctx, `UPDATE alarms SET status='completed',lease_token=NULL,lease_until_us=NULL,
            occurrence_us=NULL,last_fired_us=?,last_error='',updated_at_us=? WHERE id=?`, at.UnixMicro(), at.UnixMicro(), id)
	} else {
		next, nextErr := nextScheduled(time.Duration(interval), cronExpr, timezone, time.UnixMicro(occurrence), at)
		if nextErr != nil {
			return nextErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE alarms SET status='scheduled',next_fire_us=?,lease_token=NULL,
            lease_until_us=NULL,occurrence_us=NULL,last_fired_us=?,last_error='',updated_at_us=? WHERE id=?`,
			next.UnixMicro(), at.UnixMicro(), at.UnixMicro(), id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) Retry(ctx context.Context, id, token, lastError string, next time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return err
	}
	result, err := store.db.ExecContext(ctx, `UPDATE alarms SET status='scheduled',next_fire_us=?,override_next_us=NULL,lease_token=NULL,
		lease_until_us=NULL,last_error=?,updated_at_us=? WHERE id=? AND status='leased' AND lease_token=?`,
		next.UTC().UnixMicro(), truncate(lastError, 1024), store.now().UTC().UnixMicro(), id, token)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrLease
	}
	return nil
}

func (store *Store) Fail(ctx context.Context, id, token, lastError string, at time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return err
	}
	result, err := store.db.ExecContext(ctx, `UPDATE alarms SET status='failed',lease_token=NULL,
		lease_until_us=NULL,occurrence_us=NULL,override_next_us=NULL,last_error=?,updated_at_us=?
		WHERE id=? AND status='leased' AND lease_token=?`, truncate(lastError, 1024), at.UTC().UnixMicro(), id, token)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrLease
	}
	return nil
}

func (store *Store) NextDue(ctx context.Context) (time.Time, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return time.Time{}, false, err
	}
	var value int64
	if err := store.db.QueryRowContext(ctx, `SELECT next_fire_us FROM alarms WHERE status='scheduled' ORDER BY next_fire_us LIMIT 1`).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	return time.UnixMicro(value).UTC(), true, nil
}

type deadline struct {
	id string
	at time.Time
}

func (store *Store) deadlines(ctx context.Context) ([]deadline, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id,next_fire_us FROM alarms WHERE status='scheduled' ORDER BY next_fire_us,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []deadline
	for rows.Next() {
		var item deadline
		var at int64
		if err := rows.Scan(&item.id, &at); err != nil {
			return nil, err
		}
		item.at = time.UnixMicro(at).UTC()
		values = append(values, item)
	}
	return values, rows.Err()
}

func (store *Store) overdueCount(ctx context.Context, now time.Time) (int, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOpen(); err != nil {
		return 0, err
	}
	var count int
	err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alarms WHERE status='scheduled' AND next_fire_us<=?`, now.UTC().UnixMicro()).Scan(&count)
	return count, err
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	_, checkpointErr := store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return errors.Join(checkpointErr, store.db.Close())
}

const alarmColumns = `id,idempotency_key,next_fire_us,interval_ns,cron_expr,timezone,misfire_policy,status,sealed_body,
    lease_until_us,delivery_attempts,last_error,last_fired_us,created_at_us,updated_at_us`

type scanner interface {
	Scan(...any) error
}

func (store *Store) scanAlarm(row scanner) (Alarm, error) {
	var alarm Alarm
	var next, interval, created, updated int64
	var policy, status string
	var sealed []byte
	var leaseUntil, lastFired sql.NullInt64
	if err := row.Scan(&alarm.ID, &alarm.IdempotencyKey, &next, &interval, &alarm.CronExpr, &alarm.Timezone, &policy, &status, &sealed,
		&leaseUntil, &alarm.DeliveryAttempts, &alarm.LastError, &lastFired, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Alarm{}, ErrNotFound
		}
		return Alarm{}, fmt.Errorf("local chronos: scan alarm: %w", err)
	}
	plaintext, err := store.vault.OpenRecord(store.ad(alarm.ID), sealed)
	zero(sealed)
	if err != nil {
		return Alarm{}, fmt.Errorf("local chronos: open alarm body: %w", err)
	}
	defer zero(plaintext)
	if err := json.Unmarshal(plaintext, &alarm.Body); err != nil {
		return Alarm{}, err
	}
	alarm.NextFire = time.UnixMicro(next).UTC()
	alarm.Interval = time.Duration(interval)
	alarm.MisfirePolicy = MisfirePolicy(policy)
	alarm.Status = Status(status)
	if leaseUntil.Valid {
		alarm.LeaseUntil = time.UnixMicro(leaseUntil.Int64).UTC()
	}
	if lastFired.Valid {
		alarm.LastFiredAt = time.UnixMicro(lastFired.Int64).UTC()
	}
	alarm.CreatedAt = time.UnixMicro(created).UTC()
	alarm.UpdatedAt = time.UnixMicro(updated).UTC()
	return alarm, nil
}

func (store *Store) alarmByIdempotency(ctx context.Context, tx *sql.Tx, key string) (Alarm, error) {
	return store.scanAlarm(tx.QueryRowContext(ctx, `SELECT `+alarmColumns+` FROM alarms WHERE idempotency_key=?`, key))
}

func (store *Store) ad(id string) vault.AD {
	return vault.AD{User: store.vault.User(), Store: storeName + "." + store.machineGene, Stream: id, Schema: storeSchema}
}

func (store *Store) checkOpen() error {
	if store.closed {
		return ErrClosed
	}
	return nil
}

func (store *Store) secureSQLiteFiles() error {
	for _, path := range []string{store.path, store.path + "-wal", store.path + "-shm"} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("local chronos: secure SQLite file %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func validateCreate(request CreateRequest) error {
	if request.ID == "" || len(request.ID) > 128 || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 || request.NextFire.IsZero() {
		return fmt.Errorf("local chronos: valid id, idempotency key, and next fire are required")
	}
	if request.Interval < 0 || (request.Interval > 0 && request.CronExpr != "") {
		return fmt.Errorf("local chronos: interval cannot be negative")
	}
	if request.CronExpr != "" {
		if _, err := parseSchedule(request.CronExpr, request.Timezone); err != nil {
			return err
		}
	}
	switch request.MisfirePolicy {
	case MisfireFireOnce, MisfireSkip, MisfireCoalesce:
	default:
		return fmt.Errorf("local chronos: invalid misfire policy %q", request.MisfirePolicy)
	}
	return nil
}

func canonicalBody(body Body) ([]byte, error) {
	if len(body.Payload) == 0 {
		body.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(body.Payload) {
		return nil, fmt.Errorf("local chronos: payload must be valid JSON")
	}
	compact := new(bytes.Buffer)
	if err := json.Compact(compact, body.Payload); err != nil {
		return nil, err
	}
	body.Payload = append(json.RawMessage(nil), compact.Bytes()...)
	return json.Marshal(body)
}

func sameCreate(alarm Alarm, request CreateRequest, compareID bool) bool {
	want, err := canonicalBody(request.Body)
	if err != nil {
		return false
	}
	got, err := canonicalBody(alarm.Body)
	// A cron request's NextFire is derived from the wall clock when the request
	// is decoded. It is scheduling state, not part of the caller's idempotent
	// intent, and it legitimately changes after every fire or process restart.
	// Once alarms retain the explicit fire time as part of their identity.
	sameNextFire := alarm.NextFire.Equal(request.NextFire)
	if request.CronExpr != "" {
		sameNextFire = true
	}
	return err == nil && (!compareID || alarm.ID == request.ID) && sameNextFire &&
		alarm.Interval == request.Interval && alarm.CronExpr == request.CronExpr && alarm.Timezone == request.Timezone &&
		alarm.MisfirePolicy == request.MisfirePolicy && bytes.Equal(got, want)
}

var cronParser = robfigcron.NewParser(robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor)

func parseSchedule(expression, timezone string) (robfigcron.Schedule, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("local chronos: invalid timezone %q: %w", timezone, err)
	}
	schedule, err := cronParser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("local chronos: invalid cron expression: %w", err)
	}
	return cronInLocation{schedule: schedule, location: location}, nil
}

type cronInLocation struct {
	schedule robfigcron.Schedule
	location *time.Location
}

func (schedule cronInLocation) Next(value time.Time) time.Time {
	return schedule.schedule.Next(value.In(schedule.location)).UTC()
}

func nextScheduled(interval time.Duration, expression, timezone string, anchor, after time.Time) (time.Time, error) {
	if expression != "" {
		schedule, err := parseSchedule(expression, timezone)
		if err != nil {
			return time.Time{}, err
		}
		return schedule.Next(after).UTC(), nil
	}
	if interval <= 0 {
		return time.Time{}, fmt.Errorf("local chronos: recurring alarm has no schedule")
	}
	return advanceAfter(anchor, interval, after), nil
}

func advanceAfter(scheduled time.Time, interval time.Duration, now time.Time) time.Time {
	if interval <= 0 {
		return scheduled
	}
	if scheduled.After(now) {
		return scheduled
	}
	steps := now.Sub(scheduled)/interval + 1
	return scheduled.Add(steps * interval)
}

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("local chronos: random id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		return os.Chmod(path, 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("local chronos: directory %s must be mode 0700 and not a symlink", path)
	}
	return nil
}
