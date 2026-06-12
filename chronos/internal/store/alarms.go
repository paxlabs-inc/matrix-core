package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/paxlabs-inc/chronos/pkg/types"
)

// ErrNotFound is returned when an alarm id is unknown or not owned by the caller.
var ErrNotFound = errors.New("store: alarm not found")

// alarmColumns is the canonical SELECT/RETURNING projection. scanAlarm depends
// on this exact order.
const alarmColumns = `id, owner_did, user_id, label, kind, fire_at, cron_expr, timezone,
	next_fire_at, conversation_id, wake_message, payload, status, idempotency_key,
	max_failures, failure_count, last_error, claimed_at, created_at, updated_at, last_fired_at`

func scanAlarm(row pgx.Row) (types.Alarm, error) {
	var a types.Alarm
	var payload []byte
	err := row.Scan(
		&a.ID, &a.OwnerDID, &a.UserID, &a.Label, &a.Kind, &a.FireAt, &a.CronExpr, &a.Timezone,
		&a.NextFireAt, &a.ConversationID, &a.WakeMessage, &payload, &a.Status, &a.IdempotencyKey,
		&a.MaxFailures, &a.FailureCount, &a.LastError, &a.ClaimedAt, &a.CreatedAt, &a.UpdatedAt, &a.LastFiredAt,
	)
	if err != nil {
		return types.Alarm{}, err
	}
	a.Payload = payload
	return a, nil
}

// CreateAlarm inserts a new alarm. When the alarm carries a non-empty
// idempotency key that already exists for the owner, the existing row is
// returned with deduped=true (no duplicate is created).
func (s *Store) CreateAlarm(ctx context.Context, a types.Alarm) (types.Alarm, bool, error) {
	payload := a.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO alarms (
			owner_did, user_id, label, kind, fire_at, cron_expr, timezone,
			next_fire_at, conversation_id, wake_message, payload, status,
			idempotency_key, max_failures
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (owner_did, idempotency_key) WHERE idempotency_key <> ''
		DO NOTHING
		RETURNING `+alarmColumns,
		a.OwnerDID, a.UserID, a.Label, a.Kind, a.FireAt, a.CronExpr, a.Timezone,
		a.NextFireAt, a.ConversationID, a.WakeMessage, payload, types.StatusActive,
		a.IdempotencyKey, a.MaxFailures,
	)
	created, err := scanAlarm(row)
	if err == nil {
		return created, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return types.Alarm{}, false, fmt.Errorf("store: insert alarm: %w", err)
	}
	// Conflict on the idempotency key: return the pre-existing alarm.
	existing, gErr := scanAlarm(s.pool.QueryRow(ctx,
		`SELECT `+alarmColumns+` FROM alarms WHERE owner_did = $1 AND idempotency_key = $2`,
		a.OwnerDID, a.IdempotencyKey))
	if gErr != nil {
		return types.Alarm{}, false, fmt.Errorf("store: fetch idempotent alarm: %w", gErr)
	}
	return existing, true, nil
}

// ListAlarms returns the owner's alarms, most-recent first.
func (s *Store) ListAlarms(ctx context.Context, ownerDID string, limit int) ([]types.Alarm, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+alarmColumns+` FROM alarms WHERE owner_did = $1 ORDER BY created_at DESC LIMIT $2`,
		ownerDID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list alarms: %w", err)
	}
	defer rows.Close()
	var out []types.Alarm
	for rows.Next() {
		a, err := scanAlarm(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan alarm: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAlarm returns one alarm, owner-checked.
func (s *Store) GetAlarm(ctx context.Context, id, ownerDID string) (types.Alarm, error) {
	a, err := scanAlarm(s.pool.QueryRow(ctx,
		`SELECT `+alarmColumns+` FROM alarms WHERE id = $1 AND owner_did = $2`, id, ownerDID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.Alarm{}, ErrNotFound
		}
		return types.Alarm{}, fmt.Errorf("store: get alarm: %w", err)
	}
	return a, nil
}

// CancelAlarm marks an active alarm cancelled, owner-checked. Already-fired or
// already-cancelled alarms are left untouched but still return success.
func (s *Store) CancelAlarm(ctx context.Context, id, ownerDID string) (types.Alarm, error) {
	a, err := scanAlarm(s.pool.QueryRow(ctx, `
		UPDATE alarms SET status = 'cancelled', claimed_at = NULL, updated_at = now()
		WHERE id = $1 AND owner_did = $2 AND status = 'active'
		RETURNING `+alarmColumns, id, ownerDID))
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return types.Alarm{}, fmt.Errorf("store: cancel alarm: %w", err)
	}
	// Either unknown/unowned, or already in a terminal state: disambiguate.
	return s.GetAlarm(ctx, id, ownerDID)
}

// ClaimDue atomically leases up to `batch` active alarms whose next_fire_at has
// passed and that are not currently leased (claimed_at older than `lease`).
// FOR UPDATE SKIP LOCKED so concurrent/future workers never double-claim
// (dispatch.claim). A crash mid-fire leaves the lease, which expires after
// `lease` and is reclaimed — at-least-once delivery (invariant i3).
func (s *Store) ClaimDue(ctx context.Context, batch int, lease time.Duration) ([]types.Alarm, error) {
	if batch <= 0 {
		batch = 100
	}
	leaseSec := lease.Seconds()
	rows, err := s.pool.Query(ctx, `
		UPDATE alarms SET claimed_at = now(), updated_at = now()
		WHERE id IN (
			SELECT id FROM alarms
			WHERE status = 'active'
			  AND next_fire_at <= now()
			  AND (claimed_at IS NULL OR claimed_at < now() - ($1 * interval '1 second'))
			ORDER BY next_fire_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+alarmColumns, leaseSec, batch)
	if err != nil {
		return nil, fmt.Errorf("store: claim due: %w", err)
	}
	defer rows.Close()
	var out []types.Alarm
	for rows.Next() {
		a, err := scanAlarm(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan claimed alarm: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkFired records a successful once-alarm fire (retained for audit).
func (s *Store) MarkFired(ctx context.Context, id string) error {
	return s.exec(ctx, `
		UPDATE alarms SET status = 'fired', last_fired_at = now(), claimed_at = NULL,
			failure_count = 0, last_error = '', updated_at = now()
		WHERE id = $1`, id)
}

// Reschedule records a successful cron fire and advances next_fire_at.
func (s *Store) Reschedule(ctx context.Context, id string, next time.Time) error {
	return s.exec(ctx, `
		UPDATE alarms SET next_fire_at = $2, last_fired_at = now(), claimed_at = NULL,
			failure_count = 0, last_error = '', updated_at = now()
		WHERE id = $1`, id, next)
}

// RecordRetry increments the failure count and re-arms next_fire_at for a
// bounded backoff retry (dispatch.retry ladder_1).
func (s *Store) RecordRetry(ctx context.Context, id string, nextRetry time.Time, errMsg string) error {
	return s.exec(ctx, `
		UPDATE alarms SET failure_count = failure_count + 1, last_error = $3,
			next_fire_at = $2, claimed_at = NULL, updated_at = now()
		WHERE id = $1`, id, nextRetry, truncErr(errMsg))
}

// MarkFailed terminally fails a once alarm whose retries are exhausted
// (dispatch.retry ladder_2 — never silently dropped, invariant i6).
func (s *Store) MarkFailed(ctx context.Context, id, errMsg string) error {
	return s.exec(ctx, `
		UPDATE alarms SET status = 'failed', failure_count = failure_count + 1,
			last_error = $2, claimed_at = NULL, updated_at = now()
		WHERE id = $1`, id, truncErr(errMsg))
}

// RescheduleAfterFailure advances a cron alarm past a permanently-failed fire so
// one bad fire does not wedge the whole series (dispatch.retry ladder_3,
// skip-and-advance). The error is retained for observability.
func (s *Store) RescheduleAfterFailure(ctx context.Context, id string, next time.Time, errMsg string) error {
	return s.exec(ctx, `
		UPDATE alarms SET next_fire_at = $2, failure_count = 0, last_error = $3,
			claimed_at = NULL, updated_at = now()
		WHERE id = $1`, id, next, truncErr(errMsg))
}

func (s *Store) exec(ctx context.Context, sql string, args ...any) error {
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("store: exec: %w", err)
	}
	return nil
}

func truncErr(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}
