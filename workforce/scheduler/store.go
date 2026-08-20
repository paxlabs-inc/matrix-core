package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"
)

type Store struct {
	pool     *pgxpool.Pool
	vault    *vault.UserVault
	tenantID string
	config   Config
	now      func() time.Time
}

func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	config Config,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" || now == nil {
		return nil, fmt.Errorf("scheduler: pool, Vault, tenant_id, and time source are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("scheduler: Vault user does not match tenant")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Store{pool: pool, vault: userVault, tenantID: tenantID, config: config, now: now}, nil
}

func (store *Store) Enqueue(
	ctx context.Context,
	wake WakeEnvelope,
) (EnqueueResult, error) {
	now, err := store.currentTime()
	if err != nil {
		return EnqueueResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: begin enqueue: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	result, err := store.EnqueueTx(ctx, tx, wake, now)
	if err != nil {
		return EnqueueResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: commit enqueue: %v", ErrUncertain, err)
	}
	return result, nil
}

// EnqueueTx applies the scheduler's exact validation, sealing, authority,
// coalescing, idempotency, and event rules inside an owning caller's
// serializable transaction.
func (store *Store) EnqueueTx(
	ctx context.Context,
	tx pgx.Tx,
	wake WakeEnvelope,
	now time.Time,
) (EnqueueResult, error) {
	if tx == nil {
		return EnqueueResult{}, fmt.Errorf("scheduler: transaction is required")
	}
	if err := wake.Validate(); err != nil {
		return EnqueueResult{}, err
	}
	if wake.TenantID != store.tenantID {
		return EnqueueResult{}, ErrUnauthorized
	}
	if now.IsZero() || now.Location() != time.UTC {
		return EnqueueResult{}, fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	if err := store.verifyAuthority(ctx, tx, wake); err != nil {
		return EnqueueResult{}, err
	}
	encoded, err := json.Marshal(wake)
	if err != nil {
		return EnqueueResult{}, err
	}
	sum := sha256.Sum256(encoded)
	envelopeHash := hex.EncodeToString(sum[:])
	sealed, err := store.vault.SealRecord(store.ad(wake.OrganizationID, wake.WakeID), encoded)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: seal wake: %v", ErrUncertain, err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.tenantID+"|"+wake.OrganizationID+"|"+wake.SeatID+"|"+wake.CoalesceKey); err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: lock wake: %v", ErrUncertain, err)
	}
	var existingID, existingHash string
	err = tx.QueryRow(ctx, `
		SELECT wake_id,envelope_hash FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND idempotency_key=$3
	`, store.tenantID, wake.OrganizationID, wake.IdempotencyKey).Scan(
		&existingID, &existingHash,
	)
	if err == nil {
		if existingID != wake.WakeID || existingHash != envelopeHash {
			return EnqueueResult{}, ErrConflict
		}
		return EnqueueResult{WakeID: existingID, Deduplicated: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return EnqueueResult{}, fmt.Errorf("%w: inspect identity: %v", ErrUncertain, err)
	}
	var coalesceID string
	err = tx.QueryRow(ctx, `
		SELECT wake_id FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3
		  AND coalesce_key=$4 AND state IN ('queued','dispatched')
		ORDER BY scheduled_at,wake_id LIMIT 1
	`, store.tenantID, wake.OrganizationID, wake.SeatID, wake.CoalesceKey).Scan(&coalesceID)
	coalesced := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return EnqueueResult{}, fmt.Errorf("%w: inspect coalescing: %v", ErrUncertain, err)
	}
	state := "queued"
	if coalesced {
		state = "coalesced"
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_scheduled_wakes (
			tenant_id,organization_id,wake_id,schedule_id,seat_id,mandate_id,
			mandate_version,trigger_kind,reason,graph_scope,model_provider,model_id,
			mgs_reference,mgs_digest,budget_tasks,budget_spend_microunits,
			idempotency_key,coalesce_key,envelope_hash,sealed_envelope,state,
			scheduled_at,created_at,updated_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
			$17,$18,$19,$20,$21,$22,$23,$23
		)
	`, store.tenantID, wake.OrganizationID, wake.WakeID, wake.ScheduleID,
		wake.SeatID, wake.MandateID, wake.MandateVersion, wake.Trigger,
		wake.Reason, wake.GraphScope, wake.Model.Provider, wake.Model.ModelID,
		wake.MGS.Reference, wake.MGS.Digest, wake.Budget.MaxTasks,
		wake.Budget.MaxSpendMicrounits, wake.IdempotencyKey, wake.CoalesceKey,
		envelopeHash, sealed, state, wake.ScheduledAt, now)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: insert wake: %v", ErrUncertain, err)
	}
	eventKind := "queued"
	detail := "durably accepted"
	if coalesced {
		eventKind = "coalesced"
		detail = "coalesced into " + coalesceID
	}
	if err := store.event(ctx, tx, wake.OrganizationID, wake.WakeID,
		eventKind, detail, now); err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{WakeID: wake.WakeID, Coalesced: coalesced}, nil
}

func (store *Store) ForceSeatTx(
	ctx context.Context,
	tx pgx.Tx,
	organizationID string,
	seatID string,
	forceID string,
	reason string,
	now time.Time,
) (EnqueueResult, error) {
	if tx == nil || validText("organization_id", organizationID, 512) != nil ||
		validText("seat_id", seatID, 512) != nil || validText("force_id", forceID, 512) != nil ||
		validText("reason", reason, 512) != nil || now.IsZero() || now.Location() != time.UTC {
		return EnqueueResult{}, ErrUnauthorized
	}
	var sourceWakeID string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT wake_id,sealed_envelope
		FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3
		ORDER BY updated_at DESC,wake_id DESC LIMIT 1
	`, store.tenantID, organizationID, seatID).Scan(&sourceWakeID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnqueueResult{}, ErrNotReady
	}
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: load force-wake template: %v", ErrUncertain, err)
	}
	opened, err := store.vault.OpenRecord(store.ad(organizationID, sourceWakeID), sealed)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: open force-wake template: %v", ErrUncertain, err)
	}
	var wake WakeEnvelope
	if err := json.Unmarshal(opened, &wake); err != nil ||
		wake.TenantID != store.tenantID || wake.OrganizationID != organizationID ||
		wake.SeatID != seatID {
		return EnqueueResult{}, ErrUnauthorized
	}
	wake.WakeID = "wake:force:" + forceID
	wake.ScheduleID = "schedule:force:" + forceID
	wake.Trigger = TriggerForce
	wake.Reason = reason
	wake.ScheduledAt = now
	wake.IdempotencyKey = "force:" + forceID
	wake.CoalesceKey = "force:" + forceID
	return store.EnqueueTx(ctx, tx, wake, now)
}

func (store *Store) ClaimDue(
	ctx context.Context,
	organizationID string,
	limit uint32,
) ([]Claim, error) {
	if err := validText("organization_id", organizationID, 128); err != nil {
		return nil, err
	}
	if limit == 0 || limit > 1000 {
		return nil, fmt.Errorf("scheduler: claim limit must be 1 to 1000")
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("%w: begin claim: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_scheduled_wakes
		SET state='queued',claimed_at=NULL,updated_at=$1,
		    last_error='claim lease expired; recovered'
		WHERE tenant_id=$2 AND organization_id=$3 AND state='dispatched'
		  AND claimed_at<$1::timestamptz-($4 * interval '1 second')
	`, now, store.tenantID, organizationID, store.config.ClaimLease.Seconds()); err != nil {
		return nil, fmt.Errorf("%w: recover expired claims: %v", ErrUncertain, err)
	}
	var organizationActive, dailyTasks uint32
	var dailySpend uint64
	err = tx.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE state='dispatched'),
		  COUNT(*) FILTER (WHERE state IN ('dispatched','completed')
		    AND updated_at>=date_trunc('day',$3::timestamptz)),
		  COALESCE(SUM(CASE
		    WHEN state='completed' AND updated_at>=date_trunc('day',$3::timestamptz)
		      THEN actual_spend_microunits
		    WHEN state='dispatched' AND updated_at>=date_trunc('day',$3::timestamptz)
		      THEN budget_spend_microunits ELSE 0 END),0)
		FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.tenantID, organizationID, now).Scan(
		&organizationActive, &dailyTasks, &dailySpend,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect organization limits: %v", ErrUncertain, err)
	}
	rows, err := tx.Query(ctx, `
		SELECT wake_id,seat_id,trigger_kind,sealed_envelope
		FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND state='queued'
		  AND scheduled_at<=$3
		ORDER BY scheduled_at,wake_id
		LIMIT $4 FOR UPDATE SKIP LOCKED
	`, store.tenantID, organizationID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: query due wakes: %v", ErrUncertain, err)
	}
	type candidate struct {
		wakeID, seatID string
		trigger        TriggerKind
		sealed         []byte
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.wakeID, &item.seatID, &item.trigger, &item.sealed); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%w: scan due wake: %v", ErrUncertain, err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("%w: iterate due wakes: %v", ErrUncertain, err)
	}
	rows.Close()
	claims := make([]Claim, 0, len(candidates))
	for _, item := range candidates {
		if item.trigger != TriggerForce && store.quiet(now) {
			_ = store.event(ctx, tx, organizationID, item.wakeID,
				"deferred_quiet_hours", now.Format("2006-01-02"), now)
			continue
		}
		if organizationActive >= store.config.MaxOrganizationConcurrency {
			_ = store.event(ctx, tx, organizationID, item.wakeID,
				"deferred_concurrency", "organization ceiling", now)
			continue
		}
		var seatActive uint32
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_scheduled_wakes
			WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3
			  AND state='dispatched'
		`, store.tenantID, organizationID, item.seatID).Scan(&seatActive); err != nil {
			return nil, fmt.Errorf("%w: inspect seat concurrency: %v", ErrUncertain, err)
		}
		if seatActive >= store.config.MaxSeatConcurrency {
			_ = store.event(ctx, tx, organizationID, item.wakeID,
				"deferred_concurrency", "seat ceiling", now)
			continue
		}
		opened, err := store.vault.OpenRecord(store.ad(organizationID, item.wakeID), item.sealed)
		if err != nil {
			return nil, fmt.Errorf("%w: open wake: %v", ErrUncertain, err)
		}
		var envelope WakeEnvelope
		if err := json.Unmarshal(opened, &envelope); err != nil || envelope.Validate() != nil {
			return nil, fmt.Errorf("%w: invalid sealed wake", ErrUncertain)
		}
		if dailyTasks+envelope.Budget.MaxTasks > store.config.DailyTaskLimit {
			_ = store.event(ctx, tx, organizationID, item.wakeID,
				"deferred_task_ceiling", "daily task ceiling", now)
			continue
		}
		if dailySpend >= store.config.DailySpendMicrounits ||
			envelope.Budget.MaxSpendMicrounits >
				store.config.DailySpendMicrounits-dailySpend {
			_ = store.event(ctx, tx, organizationID, item.wakeID,
				"deferred_spend_ceiling", "daily spend ceiling", now)
			continue
		}
		command, err := tx.Exec(ctx, `
			UPDATE workforce_scheduled_wakes
			SET state='dispatched',claimed_at=$1,updated_at=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND wake_id=$4
			  AND state='queued'
		`, now, store.tenantID, organizationID, item.wakeID)
		if err != nil {
			return nil, fmt.Errorf("%w: claim wake: %v", ErrUncertain, err)
		}
		if command.RowsAffected() != 1 {
			continue
		}
		if err := store.event(ctx, tx, organizationID, item.wakeID,
			"dispatched", "lease claimed", now); err != nil {
			return nil, err
		}
		claims = append(claims, Claim{Envelope: envelope, ClaimedAt: now})
		organizationActive++
		dailyTasks += envelope.Budget.MaxTasks
		dailySpend += envelope.Budget.MaxSpendMicrounits
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("%w: commit claim: %v", ErrUncertain, err)
	}
	return claims, nil
}

func (store *Store) Complete(
	ctx context.Context,
	organizationID, wakeID string,
	actualSpendMicrounits uint64,
) error {
	return store.finish(ctx, organizationID, wakeID, "completed",
		actualSpendMicrounits, "")
}

// CompleteAndEnqueue atomically closes one claimed wake and durably accepts
// its dependency successor, preventing a crash gap between those scheduler
// state changes.
func (store *Store) CompleteAndEnqueue(
	ctx context.Context,
	organizationID, wakeID string,
	actualSpendMicrounits uint64,
	next WakeEnvelope,
) (EnqueueResult, error) {
	now, err := store.currentTime()
	if err != nil {
		return EnqueueResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return EnqueueResult{}, fmt.Errorf(
			"%w: begin complete and enqueue: %v", ErrUncertain, err,
		)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.finishTx(
		ctx, tx, organizationID, wakeID, "completed",
		actualSpendMicrounits, "", now,
	); err != nil {
		return EnqueueResult{}, err
	}
	result, err := store.EnqueueTx(ctx, tx, next, now)
	if err != nil {
		return EnqueueResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EnqueueResult{}, fmt.Errorf(
			"%w: commit complete and enqueue: %v", ErrUncertain, err,
		)
	}
	return result, nil
}

func (store *Store) Fail(
	ctx context.Context,
	organizationID, wakeID, reason string,
) error {
	if err := validText("failure reason", reason, 512); err != nil {
		return err
	}
	return store.finish(ctx, organizationID, wakeID, "failed", 0, reason)
}

func (store *Store) finish(
	ctx context.Context,
	organizationID, wakeID, state string,
	spend uint64,
	reason string,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("%w: begin finish: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.finishTx(
		ctx, tx, organizationID, wakeID, state, spend, reason, now,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit finish: %v", ErrUncertain, err)
	}
	return nil
}

func (store *Store) finishTx(
	ctx context.Context,
	tx pgx.Tx,
	organizationID, wakeID, state string,
	spend uint64,
	reason string,
	now time.Time,
) error {
	command, err := tx.Exec(ctx, `
		UPDATE workforce_scheduled_wakes
		SET state=$1,actual_spend_microunits=$2,last_error=NULLIF($3,''),
		    completed_at=$4,claimed_at=NULL,updated_at=$4
		WHERE tenant_id=$5 AND organization_id=$6 AND wake_id=$7
		  AND state='dispatched'
	`, state, spend, reason, now, store.tenantID, organizationID, wakeID)
	if err != nil {
		return fmt.Errorf("%w: finish wake: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		var currentState, currentReason string
		var currentSpend uint64
		err := tx.QueryRow(ctx, `
			SELECT state,actual_spend_microunits,COALESCE(last_error,'')
			FROM workforce_scheduled_wakes
			WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
		`, store.tenantID, organizationID, wakeID).Scan(
			&currentState, &currentSpend, &currentReason,
		)
		if err == nil && currentState == state && currentSpend == spend &&
			currentReason == reason {
			return nil
		}
		return ErrConflict
	}
	if err := store.event(ctx, tx, organizationID, wakeID,
		state, reason, now); err != nil {
		return err
	}
	return nil
}

type authorityQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (store *Store) verifyAuthority(
	ctx context.Context,
	querier authorityQuerier,
	wake WakeEnvelope,
) error {
	var seats, mandates int
	err := querier.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE authority_kind='seat' AND authority_id=$3),
		  COUNT(*) FILTER (WHERE authority_kind='mandate' AND authority_id=$4
		    AND latest_version=$5)
		FROM workforce_authority_heads
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.tenantID, wake.OrganizationID, wake.SeatID, wake.MandateID,
		wake.MandateVersion).Scan(&seats, &mandates)
	if err != nil {
		return fmt.Errorf("%w: verify wake authority: %v", ErrUncertain, err)
	}
	if seats != 1 || mandates != 1 {
		return ErrUnauthorized
	}
	return nil
}

func (store *Store) event(
	ctx context.Context,
	tx pgx.Tx,
	organizationID, wakeID, kind, detail string,
	now time.Time,
) error {
	sum := sha256.Sum256([]byte(wakeID + "|" + kind + "|" + detail))
	eventID := "wake-event:" + hex.EncodeToString(sum[:16])
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_wake_events (
			tenant_id,organization_id,wake_id,event_id,event_kind,detail,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT DO NOTHING
	`, store.tenantID, organizationID, wakeID, eventID, kind, detail, now)
	if err != nil {
		return fmt.Errorf("%w: append wake event: %v", ErrUncertain, err)
	}
	return nil
}

func (store *Store) quiet(now time.Time) bool {
	start, end := store.config.QuietHoursStartUTC, store.config.QuietHoursEndUTC
	if start == end {
		return false
	}
	hour := now.Hour()
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	return now, nil
}

func (store *Store) ad(organizationID, wakeID string) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.scheduler",
		Stream: organizationID + ":" + wakeID, Schema: "workforce.wake.v1",
	}
}
