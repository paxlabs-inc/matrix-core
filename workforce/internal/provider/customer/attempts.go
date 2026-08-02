package customer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"matrix/workforce/internal/contracts"
)

type attemptKind string

const (
	attemptDispatch   attemptKind = "dispatch"
	attemptProbe      attemptKind = "probe"
	attemptCompensate attemptKind = "compensate"
)

type attemptClaim struct {
	id             string
	organizationID contracts.OrganizationID
	connectionID   string
	version        uint64
	operation      string
	idempotencyKey string
	kind           attemptKind
	limits         ResourceLimits
}

func (store *Store) claimAttempt(
	ctx context.Context,
	authorized authorizedOperation,
	idempotencyKey string,
	kind attemptKind,
) (attemptClaim, error) {
	if token("idempotency key", idempotencyKey) != nil {
		return attemptClaim{}, fmt.Errorf("%w: idempotency key is invalid", ErrDenied)
	}
	now, err := store.currentTime()
	if err != nil {
		return attemptClaim{}, err
	}
	connection := authorized.connection.connection
	request := authorized.envelope.Request
	claim := attemptClaim{
		organizationID: connection.OrganizationID, connectionID: connection.ID,
		version: connection.Version, operation: authorized.policy.Name,
		idempotencyKey: idempotencyKey, kind: kind, limits: connection.Limits,
	}
	claim.id, err = randomID("cust-attempt")
	if err != nil {
		return attemptClaim{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return attemptClaim{}, fmt.Errorf("customer adapter: begin attempt claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	identityLock := store.lock(connection.OrganizationID, "effect",
		connection.ID+"|"+authorized.policy.Name+"|"+idempotencyKey)
	recipientLock := store.lock(connection.OrganizationID, "recipient",
		connection.ID+"|"+authorized.recipientHash.Digest+"|"+request.Channel+"|"+request.Purpose)
	for _, lock := range []string{identityLock, recipientLock} {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return attemptClaim{}, fmt.Errorf("customer adapter: lock attempt scope: %w", err)
		}
	}
	identity, err := tx.Exec(ctx, `
		INSERT INTO workforce_customer_effect_identities (
			tenant_id,organization_id,connection_id,connection_version,
			operation,idempotency_key,request_hash,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT DO NOTHING
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
		authorized.policy.Name, idempotencyKey, authorized.requestHash.Digest, now)
	if err != nil {
		return attemptClaim{}, fmt.Errorf("customer adapter: claim idempotency identity: %w", err)
	}
	if identity.RowsAffected() == 0 {
		var existingHash string
		if err := tx.QueryRow(ctx, `
			SELECT request_hash FROM workforce_customer_effect_identities
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
		`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
			authorized.policy.Name, idempotencyKey).Scan(&existingHash); err != nil ||
			existingHash != authorized.requestHash.Digest {
			return attemptClaim{}, ErrConflict
		}
	}
	if kind != attemptProbe {
		var uncertain int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_customer_effect_attempts
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
			  AND attempt_kind IN ('dispatch','compensate')
			  AND state IN ('in_flight','ambiguous','completed')
		`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
			authorized.policy.Name, idempotencyKey).Scan(&uncertain); err != nil {
			return attemptClaim{}, err
		}
		if uncertain > 0 {
			_ = store.recordIncidentTx(ctx, tx, connection.OrganizationID, connection.ID,
				connection.Version, authorized.policy.Name, idempotencyKey,
				"duplicate_communication", "idempotency_identity_already_dispatched", now)
			return attemptClaim{}, ErrAmbiguous
		}
	}
	var circuitState string
	var retryAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT state,retry_at
		FROM workforce_customer_operation_circuits
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5
		FOR UPDATE
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, authorized.policy.Name).Scan(&circuitState, &retryAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_customer_operation_circuits (
				tenant_id,organization_id,connection_id,connection_version,operation,
				state,failure_count,success_count,window_started_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,'closed',0,0,$6,$6)
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, authorized.policy.Name, now)
		circuitState = "closed"
	case err != nil:
		return attemptClaim{}, fmt.Errorf("customer adapter: load circuit: %w", err)
	case circuitState == "open" && retryAt != nil && retryAt.After(now):
		_ = store.recordIncidentTx(ctx, tx, connection.OrganizationID, connection.ID,
			connection.Version, authorized.policy.Name, idempotencyKey,
			"circuit_open", "customer_operation_circuit_open", now)
		return attemptClaim{}, ErrCircuitOpen
	case circuitState == "open":
		_, err = tx.Exec(ctx, `
			UPDATE workforce_customer_operation_circuits
			SET state='half_open',retry_at=NULL,success_count=0,updated_at=$1,version=version+1
			WHERE tenant_id=$2 AND organization_id=$3 AND connection_id=$4
			  AND connection_version=$5 AND operation=$6
		`, now, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, authorized.policy.Name)
		circuitState = "half_open"
	}
	if err != nil {
		return attemptClaim{}, fmt.Errorf("customer adapter: update circuit claim: %w", err)
	}
	var attempts int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_customer_effect_attempts
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
		  AND started_at >= $7
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
		authorized.policy.Name, idempotencyKey, now.Add(-connection.Limits.RetryWindow)).Scan(&attempts); err != nil {
		return attemptClaim{}, err
	}
	if attempts >= int(connection.Limits.MaxAttempts) {
		return attemptClaim{}, ErrCapacity
	}
	var concurrent int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_customer_effect_inflight
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND expires_at>$5
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, now).Scan(&concurrent); err != nil {
		return attemptClaim{}, err
	}
	if concurrent >= int(connection.Limits.MaxConcurrent) {
		_ = store.recordIncidentTx(ctx, tx, connection.OrganizationID, connection.ID,
			connection.Version, authorized.policy.Name, idempotencyKey,
			"capacity_exhausted", "customer_connection_concurrency_exhausted", now)
		return attemptClaim{}, ErrCapacity
	}
	if authorized.policy.Action.Mutates() {
		var openDrift int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_customer_drift_exposures
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND operation=$5 AND state='open'
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, authorized.policy.Name).Scan(&openDrift); err != nil {
			return attemptClaim{}, err
		}
		if openDrift > 0 && (connection.Limits.DriftBlindMutations == 0 ||
			openDrift >= int(connection.Limits.DriftBlindMutations)) {
			return attemptClaim{}, ErrAmbiguous
		}
	}
	if kind != attemptProbe && authorized.policy.CountsFrequency {
		var windowCount, dayCount, connectionDayCount int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_customer_frequency_events
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND recipient_hash=$5
			  AND channel=$6 AND purpose=$7 AND occurred_at>$8
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, authorized.recipientHash.Digest,
			request.Channel, request.Purpose, now.Add(-authorized.frequencyWindow)).Scan(&windowCount); err != nil {
			return attemptClaim{}, err
		}
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_customer_frequency_events
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND recipient_hash=$5 AND occurred_at>$6
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, authorized.recipientHash.Digest, now.Add(-24*time.Hour)).Scan(&dayCount); err != nil {
			return attemptClaim{}, err
		}
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_customer_frequency_events
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND occurred_at>$5
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, now.Add(-24*time.Hour)).Scan(&connectionDayCount); err != nil {
			return attemptClaim{}, err
		}
		if windowCount >= int(authorized.frequencyLimit) ||
			dayCount >= int(connection.Limits.MaxPerRecipientDay) ||
			connectionDayCount >= int(connection.Limits.MaxPerConnectionDay) {
			_ = store.recordIncidentTx(ctx, tx, connection.OrganizationID, connection.ID,
				connection.Version, authorized.policy.Name, idempotencyKey,
				"frequency_exhausted", "customer_frequency_limit_reached", now)
			return attemptClaim{}, ErrFrequencyLimit
		}
		frequencyID, err := randomID("cust-frequency")
		if err != nil {
			return attemptClaim{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_customer_frequency_events (
				tenant_id,organization_id,event_id,connection_id,connection_version,
				customer_id,recipient_hash,channel,purpose,operation,idempotency_key,occurred_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		`, store.tenantID, connection.OrganizationID, frequencyID, connection.ID,
			connection.Version, request.CustomerID, authorized.recipientHash.Digest,
			request.Channel, request.Purpose, authorized.policy.Name, idempotencyKey, now); err != nil {
			return attemptClaim{}, fmt.Errorf("customer adapter: commit frequency spend: %w", err)
		}
	}
	expiresAt := now.Add(connection.Limits.RetryWindow)
	if request.ExpiresAt.Before(expiresAt) {
		expiresAt = request.ExpiresAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_customer_effect_attempts (
			tenant_id,organization_id,attempt_id,connection_id,connection_version,
			operation,idempotency_key,attempt_kind,request_hash,recipient_hash,
			counts_frequency,state,started_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'in_flight',$12,$13)
	`, store.tenantID, connection.OrganizationID, claim.id, connection.ID,
		connection.Version, authorized.policy.Name, idempotencyKey, kind,
		authorized.requestHash.Digest, authorized.recipientHash.Digest,
		authorized.policy.CountsFrequency && kind != attemptProbe, now, expiresAt); err != nil {
		return attemptClaim{}, fmt.Errorf("customer adapter: persist attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_customer_effect_inflight (
			tenant_id,organization_id,attempt_id,connection_id,connection_version,
			operation,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, store.tenantID, connection.OrganizationID, claim.id, connection.ID,
		connection.Version, authorized.policy.Name, expiresAt, now); err != nil {
		return attemptClaim{}, fmt.Errorf("customer adapter: persist inflight claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return attemptClaim{}, fmt.Errorf("customer adapter: commit attempt claim: %w", err)
	}
	return claim, nil
}

func (store *Store) completeAttempt(
	ctx context.Context,
	claim attemptClaim,
	state, safeCode, externalID, observationHash string,
) error {
	if state != "completed" && state != "failed" && state != "ambiguous" {
		return fmt.Errorf("customer adapter: invalid attempt completion state")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	command, err := tx.Exec(ctx, `
		UPDATE workforce_customer_effect_attempts
		SET state=$1,safe_code=NULLIF($2,''),external_id=NULLIF($3,''),
		    observation_hash=NULLIF($4,''),finished_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND attempt_id=$8 AND state='in_flight'
	`, state, safeCode, externalID, observationHash, now,
		store.tenantID, claim.organizationID, claim.id)
	if err != nil || command.RowsAffected() != 1 {
		return fmt.Errorf("customer adapter: complete attempt: %w", ErrConflict)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM workforce_customer_effect_inflight
		WHERE tenant_id=$1 AND organization_id=$2 AND attempt_id=$3
	`, store.tenantID, claim.organizationID, claim.id); err != nil {
		return err
	}
	if err := store.updateCircuitTx(ctx, tx, claim, state == "completed", safeCode, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func (store *Store) updateCircuitTx(
	ctx context.Context,
	tx pgx.Tx,
	claim attemptClaim,
	succeeded bool,
	safeCode string,
	now time.Time,
) error {
	var state string
	var failures, successes int
	var windowStarted time.Time
	if err := tx.QueryRow(ctx, `
		SELECT state,failure_count,success_count,window_started_at
		FROM workforce_customer_operation_circuits
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5 FOR UPDATE
	`, store.tenantID, claim.organizationID, claim.connectionID,
		claim.version, claim.operation).Scan(&state, &failures, &successes, &windowStarted); err != nil {
		return err
	}
	if now.Sub(windowStarted) > claim.limits.CircuitWindow {
		failures, successes, windowStarted = 0, 0, now
	}
	var retryAt *time.Time
	if succeeded {
		successes++
		if state == "half_open" && successes >= int(claim.limits.SuccessThreshold) || state == "closed" {
			state, failures = "closed", 0
		}
	} else {
		failures++
		successes = 0
		if failures >= int(claim.limits.FailureThreshold) {
			state = "open"
			value := now.Add(claim.limits.CircuitOpenDuration)
			retryAt = &value
		}
	}
	_, err := tx.Exec(ctx, `
		UPDATE workforce_customer_operation_circuits
		SET state=$1,failure_count=$2,success_count=$3,window_started_at=$4,
		    retry_at=$5,last_safe_code=NULLIF($6,''),updated_at=$7,version=version+1
		WHERE tenant_id=$8 AND organization_id=$9 AND connection_id=$10
		  AND connection_version=$11 AND operation=$12
	`, state, failures, successes, windowStarted, retryAt, safeCode, now,
		store.tenantID, claim.organizationID, claim.connectionID,
		claim.version, claim.operation)
	return err
}

func (store *Store) recordDrift(ctx context.Context, claim attemptClaim) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO workforce_customer_drift_exposures (
			tenant_id,organization_id,connection_id,connection_version,
			operation,idempotency_key,state,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,'open',$7)
		ON CONFLICT DO NOTHING
	`, store.tenantID, claim.organizationID, claim.connectionID,
		claim.version, claim.operation, claim.idempotencyKey, now)
	return err
}

func (store *Store) resolveDrift(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	operation, idempotencyKey, resolution string,
) error {
	if resolution != "reconciled" && resolution != "compensated" {
		return fmt.Errorf("customer adapter: invalid drift resolution")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	_, err = store.pool.Exec(ctx, `
		UPDATE workforce_customer_drift_exposures
		SET state=$1,resolved_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND connection_id=$5
		  AND connection_version=$6 AND operation=$7 AND idempotency_key=$8
		  AND state='open'
	`, resolution, now, store.tenantID, organizationID, connectionID,
		version, operation, idempotencyKey)
	return err
}

func (store *Store) recordIncident(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	operation, idempotencyKey, kind, safeCode string,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.recordIncidentTx(ctx, tx, organizationID, connectionID, version,
		operation, idempotencyKey, kind, safeCode, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) recordIncidentTx(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	operation, idempotencyKey, kind, safeCode string,
	now time.Time,
) error {
	id, err := randomID("cust-incident")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_customer_incidents (
			tenant_id,organization_id,incident_id,connection_id,connection_version,
			operation,idempotency_key,kind,safe_code,state,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',$10)
	`, store.tenantID, organizationID, id, connectionID, version,
		operation, idempotencyKey, kind, safeCode, now)
	return err
}

func randomID(prefix string) (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(bytes), nil
}
