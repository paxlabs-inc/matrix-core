package circuit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
)

// Store is the only authority allowed to transition durable circuit state.
type Store struct {
	pool     *pgxpool.Pool
	tenantID string
	config   Config
	now      func() time.Time
}

// New constructs one tenant-scoped durable circuit authority.
func New(pool *pgxpool.Pool, tenantID string, config Config, now func() time.Time) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || tenantID == "" || now == nil {
		return nil, fmt.Errorf("circuit: pool, tenant_id, and time source are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Store{pool: pool, tenantID: tenantID, config: config, now: now}, nil
}

// Keys returns the three mandatory breaker dimensions for an effect.
func Keys(
	organizationID contracts.OrganizationID,
	provider, skill, effectClass string,
) ([]Key, error) {
	keys := []Key{
		{OrganizationID: organizationID, Kind: KindProvider, Name: provider},
		{OrganizationID: organizationID, Kind: KindSkill, Name: skill},
		{OrganizationID: organizationID, Kind: KindEffectClass, Name: effectClass},
	}
	for _, key := range keys {
		if err := key.Validate(); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// Admit atomically evaluates every applicable circuit and mints a bounded permit.
func (store *Store) Admit(
	ctx context.Context,
	keys []Key,
	irreversible bool,
) (Permit, error) {
	ordered, err := normalizeKeys(keys)
	if err != nil {
		return Permit{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return Permit{}, err
	}
	permitID, err := randomID()
	if err != nil {
		return Permit{}, fmt.Errorf("%w: mint permit: %v", ErrUncertain, err)
	}
	// The ordered transaction-scoped advisory locks below serialize every
	// breaker key shared by concurrent admissions. Read committed avoids a
	// PostgreSQL serialization abort after a waiter resumes while preserving
	// the same single-writer state transition boundary.
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Permit{}, fmt.Errorf("%w: begin admission: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, key := range ordered {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
			store.lockKey(key)); err != nil {
			return Permit{}, fmt.Errorf("%w: lock breaker: %v", ErrUncertain, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_circuit_breakers (
				tenant_id,organization_id,breaker_kind,breaker_name,state,
				window_started_at,updated_at
			) VALUES ($1,$2,$3,$4,'closed',$5,$5)
			ON CONFLICT DO NOTHING
		`, store.tenantID, key.OrganizationID, key.Kind, key.Name, now); err != nil {
			return Permit{}, fmt.Errorf("%w: initialize breaker: %v", ErrUncertain, err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM workforce_circuit_trials
			WHERE tenant_id=$1 AND organization_id=$2 AND breaker_kind=$3
			  AND breaker_name=$4 AND expires_at<=$5
		`, store.tenantID, key.OrganizationID, key.Kind, key.Name, now); err != nil {
			return Permit{}, fmt.Errorf("%w: expire trials: %v", ErrUncertain, err)
		}
	}
	type loaded struct {
		key     Key
		state   State
		retryAt *time.Time
	}
	rows := make([]loaded, 0, len(ordered))
	for _, key := range ordered {
		var row loaded
		row.key = key
		err := tx.QueryRow(ctx, `
			SELECT state,retry_at
			FROM workforce_circuit_breakers
			WHERE tenant_id=$1 AND organization_id=$2
			  AND breaker_kind=$3 AND breaker_name=$4
			FOR UPDATE
		`, store.tenantID, key.OrganizationID, key.Kind, key.Name).Scan(&row.state, &row.retryAt)
		if err != nil {
			return Permit{}, fmt.Errorf("%w: load breaker: %v", ErrUncertain, err)
		}
		if row.state == StateOpen {
			if row.retryAt == nil || now.Before(*row.retryAt) {
				return Permit{}, store.openError(ctx, tx, key)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE workforce_circuit_breakers
				SET state='half_open',success_count=0,updated_at=$1,version=version+1
				WHERE tenant_id=$2 AND organization_id=$3
				  AND breaker_kind=$4 AND breaker_name=$5 AND state='open'
			`, now, store.tenantID, key.OrganizationID, key.Kind, key.Name); err != nil {
				return Permit{}, fmt.Errorf("%w: enter half-open: %v", ErrUncertain, err)
			}
			row.state = StateHalfOpen
		}
		if row.state == StateHalfOpen {
			if irreversible {
				return Permit{}, fmt.Errorf("%w: irreversible half-open trial denied for %s/%s",
					ErrOpen, key.Kind, key.Name)
			}
			var count uint32
			if err := tx.QueryRow(ctx, `
				SELECT COUNT(*) FROM workforce_circuit_trials
				WHERE tenant_id=$1 AND organization_id=$2
				  AND breaker_kind=$3 AND breaker_name=$4
			`, store.tenantID, key.OrganizationID, key.Kind, key.Name).Scan(&count); err != nil {
				return Permit{}, fmt.Errorf("%w: count half-open trials: %v", ErrUncertain, err)
			}
			if count >= store.config.HalfOpenLimit {
				return Permit{}, fmt.Errorf("%w: half-open limit reached for %s/%s",
					ErrOpen, key.Kind, key.Name)
			}
		}
		rows = append(rows, row)
	}
	expiresAt := now.Add(store.config.TrialDuration)
	for _, row := range rows {
		if row.state != StateHalfOpen {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_circuit_trials (
				tenant_id,organization_id,breaker_kind,breaker_name,
				permit_id,expires_at,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, store.tenantID, row.key.OrganizationID, row.key.Kind, row.key.Name,
			permitID, expiresAt, now); err != nil {
			return Permit{}, fmt.Errorf("%w: reserve half-open trial: %v", ErrUncertain, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Permit{}, fmt.Errorf("%w: commit admission: %v", ErrUncertain, err)
	}
	return Permit{ID: permitID, Keys: ordered, ExpiresAt: expiresAt}, nil
}

// Succeed records one authoritative success and closes recovered circuits.
func (store *Store) Succeed(ctx context.Context, permit Permit) error {
	return store.complete(ctx, permit, true, "")
}

// Fail records one authoritative failure and opens thresholded circuits.
func (store *Store) Fail(ctx context.Context, permit Permit, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 {
		return fmt.Errorf("circuit: bounded failure reason is required")
	}
	return store.complete(ctx, permit, false, reason)
}

// Release relinquishes half-open capacity without treating an internal rejection as provider health.
func (store *Store) Release(ctx context.Context, permit Permit) error {
	ordered, err := validatePermit(permit)
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("%w: begin release: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, key := range ordered {
		if _, err := tx.Exec(ctx, `
			DELETE FROM workforce_circuit_trials
			WHERE tenant_id=$1 AND organization_id=$2 AND breaker_kind=$3
			  AND breaker_name=$4 AND permit_id=$5
		`, store.tenantID, key.OrganizationID, key.Kind, key.Name, permit.ID); err != nil {
			return fmt.Errorf("%w: release trial: %v", ErrUncertain, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit release: %v", ErrUncertain, err)
	}
	return nil
}

// Inspect returns deterministic observable state for the requested dimensions.
func (store *Store) Inspect(ctx context.Context, keys []Key) ([]Snapshot, error) {
	ordered, err := normalizeKeys(keys)
	if err != nil {
		return nil, err
	}
	result := make([]Snapshot, 0, len(ordered))
	for _, key := range ordered {
		var snapshot Snapshot
		snapshot.Key = key
		var retryAt *time.Time
		err := store.pool.QueryRow(ctx, `
			SELECT state,failure_count,success_count,COALESCE(reason,''),
				retry_at,version,updated_at,
				(SELECT COUNT(*) FROM workforce_circuit_trials trial
				 WHERE trial.tenant_id=breaker.tenant_id
				   AND trial.organization_id=breaker.organization_id
				   AND trial.breaker_kind=breaker.breaker_kind
				   AND trial.breaker_name=breaker.breaker_name
				   AND trial.expires_at>$5)
			FROM workforce_circuit_breakers breaker
			WHERE tenant_id=$1 AND organization_id=$2
			  AND breaker_kind=$3 AND breaker_name=$4
		`, store.tenantID, key.OrganizationID, key.Kind, key.Name, store.now()).Scan(
			&snapshot.State, &snapshot.FailureCount, &snapshot.SuccessCount,
			&snapshot.Reason, &retryAt, &snapshot.Version, &snapshot.UpdatedAt,
			&snapshot.HalfOpenInUse,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			snapshot.State = StateClosed
			result = append(result, snapshot)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect breaker: %v", ErrUncertain, err)
		}
		normalizeTime(&snapshot.UpdatedAt)
		if retryAt != nil {
			normalizeTime(retryAt)
		}
		snapshot.RetryAt = retryAt
		result = append(result, snapshot)
	}
	return result, nil
}

func (store *Store) complete(
	ctx context.Context,
	permit Permit,
	success bool,
	reason string,
) error {
	var err error
	for attempt := 0; attempt < 32; attempt++ {
		err = store.completeOnce(ctx, permit, success, reason)
		if !retryable(err) {
			return err
		}
	}
	return fmt.Errorf("%w: circuit completion retry budget exhausted: %v", ErrUncertain, err)
}

func (store *Store) completeOnce(
	ctx context.Context,
	permit Permit,
	success bool,
	reason string,
) error {
	ordered, err := validatePermit(permit)
	if err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("%w: begin completion: %w", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, key := range ordered {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
			store.lockKey(key)); err != nil {
			return fmt.Errorf("%w: lock completion: %w", ErrUncertain, err)
		}
		var state State
		var failures, successes uint32
		var windowStarted time.Time
		err := tx.QueryRow(ctx, `
			SELECT state,failure_count,success_count,window_started_at
			FROM workforce_circuit_breakers
			WHERE tenant_id=$1 AND organization_id=$2
			  AND breaker_kind=$3 AND breaker_name=$4
			FOR UPDATE
		`, store.tenantID, key.OrganizationID, key.Kind, key.Name).Scan(
			&state, &failures, &successes, &windowStarted,
		)
		if err != nil {
			return fmt.Errorf("%w: load completion breaker: %w", ErrUncertain, err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM workforce_circuit_trials
			WHERE tenant_id=$1 AND organization_id=$2 AND breaker_kind=$3
			  AND breaker_name=$4 AND permit_id=$5
		`, store.tenantID, key.OrganizationID, key.Kind, key.Name, permit.ID); err != nil {
			return fmt.Errorf("%w: consume trial: %w", ErrUncertain, err)
		}
		normalizeTime(&windowStarted)
		if success {
			nextState := state
			nextFailures := failures
			nextSuccesses := successes
			if state == StateHalfOpen {
				nextSuccesses++
				if nextSuccesses >= store.config.SuccessThreshold {
					nextState, nextFailures, nextSuccesses = StateClosed, 0, 0
				}
			} else if state == StateClosed {
				nextFailures = 0
			}
			if _, err := tx.Exec(ctx, `
				UPDATE workforce_circuit_breakers
				SET state=$1,failure_count=$2,success_count=$3,
					opened_at=CASE WHEN $1='closed' THEN NULL ELSE opened_at END,
					retry_at=CASE WHEN $1='closed' THEN NULL ELSE retry_at END,
					reason=CASE WHEN $1='closed' THEN NULL ELSE reason END,
					window_started_at=CASE WHEN $1='closed' THEN $4 ELSE window_started_at END,
					updated_at=$4,version=version+1
				WHERE tenant_id=$5 AND organization_id=$6
				  AND breaker_kind=$7 AND breaker_name=$8
			`, nextState, nextFailures, nextSuccesses, now, store.tenantID,
				key.OrganizationID, key.Kind, key.Name); err != nil {
				return fmt.Errorf("%w: record success: %w", ErrUncertain, err)
			}
			continue
		}
		if state == StateClosed && now.Sub(windowStarted) > store.config.Window {
			failures = 0
			windowStarted = now
		}
		failures++
		nextState := state
		if state == StateHalfOpen || failures >= store.config.FailureThreshold {
			nextState = StateOpen
		}
		var openedAt, retryAt *time.Time
		if nextState == StateOpen {
			open := now
			retry := now.Add(store.config.OpenDuration)
			openedAt, retryAt = &open, &retry
			successes = 0
		}
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_circuit_breakers
			SET state=$1,failure_count=$2,success_count=$3,window_started_at=$4,
				opened_at=$5,retry_at=$6,reason=$7,updated_at=$8,version=version+1
			WHERE tenant_id=$9 AND organization_id=$10
			  AND breaker_kind=$11 AND breaker_name=$12
		`, nextState, failures, successes, windowStarted, openedAt, retryAt,
			reason, now, store.tenantID, key.OrganizationID, key.Kind, key.Name); err != nil {
			return fmt.Errorf("%w: record failure: %w", ErrUncertain, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit completion: %w", ErrUncertain, err)
	}
	return nil
}

func retryable(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) &&
		(databaseError.Code == "40001" || databaseError.Code == "40P01")
}

func (store *Store) openError(ctx context.Context, tx pgx.Tx, key Key) error {
	var reason string
	var retryAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(reason,''),retry_at
		FROM workforce_circuit_breakers
		WHERE tenant_id=$1 AND organization_id=$2
		  AND breaker_kind=$3 AND breaker_name=$4
	`, store.tenantID, key.OrganizationID, key.Kind, key.Name).Scan(&reason, &retryAt)
	if err != nil {
		return fmt.Errorf("%w: inspect open breaker: %v", ErrUncertain, err)
	}
	return fmt.Errorf("%w: %s/%s reason=%s retry_at=%v",
		ErrOpen, key.Kind, key.Name, reason, retryAt)
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	return now, nil
}

func (store *Store) lockKey(key Key) string {
	return store.tenantID + "|" + string(key.OrganizationID) + "|" +
		string(key.Kind) + "|" + key.Name
}

func normalizeKeys(keys []Key) ([]Key, error) {
	if len(keys) == 0 || len(keys) > 16 {
		return nil, fmt.Errorf("circuit: 1 to 16 breaker keys are required")
	}
	ordered := append([]Key(nil), keys...)
	for _, key := range ordered {
		if err := key.Validate(); err != nil {
			return nil, err
		}
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].OrganizationID != ordered[right].OrganizationID {
			return ordered[left].OrganizationID < ordered[right].OrganizationID
		}
		if ordered[left].Kind != ordered[right].Kind {
			return ordered[left].Kind < ordered[right].Kind
		}
		return ordered[left].Name < ordered[right].Name
	})
	for index := 1; index < len(ordered); index++ {
		if ordered[index] == ordered[index-1] {
			return nil, fmt.Errorf("circuit: duplicate breaker key")
		}
	}
	return ordered, nil
}

func validatePermit(permit Permit) ([]Key, error) {
	if err := validateToken("permit_id", permit.ID); err != nil {
		return nil, err
	}
	if permit.ExpiresAt.IsZero() || permit.ExpiresAt.Location() != time.UTC {
		return nil, fmt.Errorf("circuit: permit expiry must be UTC")
	}
	return normalizeKeys(permit.Keys)
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func normalizeTime(value *time.Time) {
	*value = value.UTC()
}
