package financial

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"centra/workforce/internal/contracts"
)

type preparedObservation struct {
	id        string
	hash      contracts.ContentHash
	canonical []byte
	sealed    []byte
}

func (store *Store) prepareObservation(observation Observation, reservationID string) (preparedObservation, error) {
	canonical, err := contracts.EncodeCanonical(&observation)
	if err != nil {
		return preparedObservation{}, err
	}
	hash := contracts.ContentHash{Algorithm: "sha256", Digest: digest(canonical)}
	idDigest := digest([]byte(reservationID + "|" + hash.Digest))
	id := "finobs-" + idDigest[:32]
	sealed, err := store.vault.SealRecord(store.observationAD(observation.OrganizationID, id), canonical)
	if err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: seal observation: %w", err)
	}
	return preparedObservation{id: id, hash: hash, canonical: canonical, sealed: sealed}, nil
}

func (store *Store) recordPreliminaryObservation(
	ctx context.Context,
	claim attemptClaim,
	observation Observation,
) (preparedObservation, error) {
	record, err := store.prepareObservation(observation, claim.reservationID)
	if err != nil {
		return preparedObservation{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return preparedObservation{}, err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_financial_observations (
			tenant_id,organization_id,observation_id,reservation_id,connection_id,
			connection_version,operation,idempotency_key,external_id,financial_state,
			authority,reconciled,economic_truth,canonical_hash,sealed_record,
			provider_observed_at,captured_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT DO NOTHING
	`, store.tenantID, observation.OrganizationID, record.id, claim.reservationID,
		claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
		observation.ExternalID, observation.State, observation.Authority,
		observation.Reconciled, observation.EconomicTruth, record.hash.Digest, record.sealed,
		observation.ProviderObservedAt, observation.CapturedAt, now)
	if err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: persist preliminary observation: %w", err)
	}
	if command.RowsAffected() == 0 {
		var storedHash string
		if err := store.pool.QueryRow(ctx, `
			SELECT canonical_hash FROM workforce_financial_observations
			WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3
		`, store.tenantID, observation.OrganizationID, record.id).Scan(&storedHash); err != nil || storedHash != record.hash.Digest {
			return preparedObservation{}, ErrConflict
		}
	}
	return record, nil
}

func (store *Store) markAmbiguous(
	ctx context.Context,
	claim attemptClaim,
	request Request,
	externalID string,
	observationHash string,
	safeCode string,
	incidentKind string,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("financial adapter: begin ambiguity freeze: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(claim.organizationID, "capital")); err != nil {
		return err
	}
	if err := store.completeAttemptTx(ctx, tx, claim, "ambiguous", safeCode, externalID, observationHash, now); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_financial_reservations
		SET state='ambiguous',updated_at=$1,reconciled_at=NULL
		WHERE tenant_id=$2 AND organization_id=$3 AND reservation_id=$4
		  AND state IN ('reserved','ambiguous')
	`, now, store.tenantID, claim.organizationID, claim.reservationID)
	if err != nil || command.RowsAffected() != 1 {
		return ErrConflict
	}
	for _, scope := range []struct{ kind, key string }{
		{"organization", string(claim.organizationID)},
		{"asset", request.Amount.Asset},
		{"venue", request.Venue + "/" + request.Rail},
		{"counterparty", request.Counterparty},
		{"destination", request.DestinationHash.Digest},
	} {
		freezeDigest := digest([]byte(claim.reservationID + "|" + scope.kind + "|" + scope.key))
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_financial_scope_freezes (
				tenant_id,organization_id,freeze_id,reservation_id,scope_kind,
				scope_key,reason_code,state,created_at,resolved_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,'open',$8,NULL)
			ON CONFLICT (tenant_id,organization_id,reservation_id,scope_kind,scope_key)
			DO NOTHING
		`, store.tenantID, claim.organizationID, "finfreeze-"+freezeDigest[:32],
			claim.reservationID, scope.kind, scope.key, safeCode, now); err != nil {
			return fmt.Errorf("financial adapter: persist ambiguity freeze: %w", err)
		}
	}
	if incidentKind == "" {
		incidentKind = "financial_ambiguity"
	}
	if err := store.recordIncidentTx(ctx, tx, claim.organizationID, claim.reservationID,
		claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
		incidentKind, safeCode, now); err != nil {
		return err
	}
	if err := store.failCircuitTx(ctx, tx, claim, safeCode, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) markDispatchFailed(
	ctx context.Context,
	claim attemptClaim,
	externalID string,
	safeCode string,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.completeAttemptTx(ctx, tx, claim, "failed", safeCode, externalID, "", now); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_financial_reservations
		SET state='failed',updated_at=$1,reconciled_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND reservation_id=$4 AND state='reserved'
	`, now, store.tenantID, claim.organizationID, claim.reservationID)
	if err != nil {
		return fmt.Errorf("financial adapter: release failed reservation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := store.recordIncidentTx(ctx, tx, claim.organizationID, claim.reservationID,
		claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
		"provider_outage", safeCode, now); err != nil {
		return err
	}
	if err := store.failCircuitTx(ctx, tx, claim, safeCode, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) markProbeInconclusive(
	ctx context.Context,
	claim attemptClaim,
	externalID string,
	observationHash string,
	safeCode string,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.completeAttemptTx(ctx, tx, claim, "failed", safeCode, externalID, observationHash, now); err != nil {
		return err
	}
	if err := store.recordIncidentTx(ctx, tx, claim.organizationID, claim.reservationID,
		claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
		"provider_outage", safeCode, now); err != nil {
		return err
	}
	if err := store.failCircuitTx(ctx, tx, claim, safeCode, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) commitFinal(
	ctx context.Context,
	claim attemptClaim,
	authorized authorizedOperation,
	observation Observation,
) (preparedObservation, error) {
	record, err := store.prepareObservation(observation, claim.reservationID)
	if err != nil {
		return preparedObservation{}, err
	}
	request := authorized.envelope.Request
	outcome := observation.Outcome
	if !observation.Reconciled || !observation.EconomicTruth || outcome.RiskAfter == nil ||
		outcome.PreviousResourceVersion != request.ExpectedProviderResourceVersion ||
		outcome.RiskAfter.ResourceVersion != outcome.ResourceVersion {
		return preparedObservation{}, ErrOutOfBandChange
	}
	riskCanonical, err := contracts.EncodeCanonical(outcome.RiskAfter)
	if err != nil {
		return preparedObservation{}, err
	}
	riskHash := digest(riskCanonical)
	riskID := "finrisk-" + record.hash.Digest[:32]
	riskSealed, err := store.vault.SealRecord(store.riskAD(
		observation.OrganizationID, claim.connectionID, claim.version, riskID,
		authorized.risk.version+1,
	), riskCanonical)
	if err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: seal reconciled risk state: %w", err)
	}
	now, err := store.currentTime()
	if err != nil {
		return preparedObservation{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: begin financial reconciliation commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(observation.OrganizationID, "capital")); err != nil {
		return preparedObservation{}, err
	}
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM workforce_financial_reservations
		WHERE tenant_id=$1 AND organization_id=$2 AND reservation_id=$3 FOR UPDATE
	`, store.tenantID, observation.OrganizationID, claim.reservationID).Scan(&state); err != nil ||
		state != "reserved" && state != "ambiguous" {
		return preparedObservation{}, ErrConflict
	}
	var headVersion uint64
	var headHash, headResource string
	if err := tx.QueryRow(ctx, `
		SELECT version,canonical_hash,resource_version FROM workforce_financial_risk_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND connection_version=$4
		FOR UPDATE
	`, store.tenantID, observation.OrganizationID, claim.connectionID, claim.version).Scan(
		&headVersion, &headHash, &headResource,
	); err != nil || headVersion != authorized.risk.version || headHash != authorized.risk.hash.Digest ||
		headResource != request.ExpectedProviderResourceVersion {
		return preparedObservation{}, ErrOutOfBandChange
	}
	if err := store.insertObservationTx(ctx, tx, claim, observation, record, now); err != nil {
		return preparedObservation{}, err
	}
	riskExpires := outcome.ObservedAt.Add(authorized.connection.connection.Capital.MaxRiskStateAge)
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_risk_snapshots (
			tenant_id,organization_id,connection_id,connection_version,snapshot_id,
			version,source_kind,source_id,canonical_hash,sealed_record,resource_version,
			total_capital_microunits,available_liquidity_microunits,
			gross_exposure_microunits,drawdown_microunits,runway_microunits,
			observed_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,'provider_observation',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
	`, store.tenantID, observation.OrganizationID, claim.connectionID, claim.version,
		riskID, headVersion+1, record.id, riskHash, riskSealed, outcome.RiskAfter.ResourceVersion,
		outcome.RiskAfter.TotalCapitalMicrounits, outcome.RiskAfter.AvailableLiquidityMicrounits,
		outcome.RiskAfter.GrossExposureMicrounits, outcome.RiskAfter.DrawdownMicrounits,
		outcome.RiskAfter.RunwayMicrounits, outcome.ObservedAt, riskExpires, now); err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: persist reconciled risk state: %w", err)
	}
	headCommand, err := tx.Exec(ctx, `
		UPDATE workforce_financial_risk_heads
		SET snapshot_id=$1,version=$2,canonical_hash=$3,resource_version=$4,
			observed_at=$5,expires_at=$6,updated_at=$7
		WHERE tenant_id=$8 AND organization_id=$9 AND connection_id=$10
		  AND connection_version=$11 AND version=$12
	`, riskID, headVersion+1, riskHash, outcome.RiskAfter.ResourceVersion,
		outcome.ObservedAt, riskExpires, now, store.tenantID, observation.OrganizationID,
		claim.connectionID, claim.version, headVersion)
	if err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: advance reconciled risk head: %w", err)
	}
	if headCommand.RowsAffected() != 1 {
		return preparedObservation{}, ErrOutOfBandChange
	}
	for index, line := range outcome.Accounting {
		entryDigest := digest([]byte(record.id + "|" + strconv.Itoa(index)))
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_financial_accounting_entries (
				tenant_id,organization_id,entry_id,observation_id,reservation_id,
				initiative_id,account_id,side,currency,microunits,valuation_time,
				methodology_id,evidence_hash,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		`, store.tenantID, observation.OrganizationID, "finentry-"+entryDigest[:32],
			record.id, claim.reservationID, request.InitiativeID, line.AccountID,
			line.Side, line.Currency, line.Microunits, observation.ValuationTime,
			request.AccountingMethodologyID, outcome.SettlementEvidenceHash.Digest, now); err != nil {
			return preparedObservation{}, fmt.Errorf("financial adapter: persist balanced accounting line: %w", err)
		}
	}
	reservationState := "settled"
	if outcome.State == StateReversed {
		reservationState = "reversed"
	}
	reservationCommand, err := tx.Exec(ctx, `
		UPDATE workforce_financial_reservations
		SET state=$1,updated_at=$2,reconciled_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND reservation_id=$5
		  AND state IN ('reserved','ambiguous')
	`, reservationState, now, store.tenantID, observation.OrganizationID,
		claim.reservationID)
	if err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: finalize capital reservation: %w", err)
	}
	if reservationCommand.RowsAffected() != 1 {
		return preparedObservation{}, ErrConflict
	}
	if err := store.completeAttemptTx(ctx, tx, claim, "completed", "",
		observation.ExternalID, record.hash.Digest, now); err != nil {
		return preparedObservation{}, err
	}
	if err := store.resolveAmbiguityTx(ctx, tx, claim, now); err != nil {
		return preparedObservation{}, err
	}
	if err := store.succeedCircuitTx(ctx, tx, claim, now); err != nil {
		return preparedObservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return preparedObservation{}, fmt.Errorf("financial adapter: commit authoritative financial reconciliation: %w", err)
	}
	return record, nil
}

func (store *Store) commitDefinitiveFailure(
	ctx context.Context,
	claim attemptClaim,
	observation Observation,
) (preparedObservation, error) {
	record, err := store.prepareObservation(observation, claim.reservationID)
	if err != nil {
		return preparedObservation{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return preparedObservation{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return preparedObservation{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.insertObservationTx(ctx, tx, claim, observation, record, now); err != nil {
		return preparedObservation{}, err
	}
	state := "rejected"
	if observation.State == StateFailed {
		state = "failed"
	}
	reservationCommand, err := tx.Exec(ctx, `
		UPDATE workforce_financial_reservations SET state=$1,updated_at=$2,reconciled_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND reservation_id=$5
		  AND state IN ('reserved','ambiguous')
	`, state, now, store.tenantID, observation.OrganizationID, claim.reservationID)
	if err != nil {
		return preparedObservation{}, err
	}
	if reservationCommand.RowsAffected() != 1 {
		return preparedObservation{}, ErrConflict
	}
	if err := store.completeAttemptTx(ctx, tx, claim, "completed", "",
		observation.ExternalID, record.hash.Digest, now); err != nil {
		return preparedObservation{}, err
	}
	if err := store.resolveAmbiguityTx(ctx, tx, claim, now); err != nil {
		return preparedObservation{}, err
	}
	if err := store.succeedCircuitTx(ctx, tx, claim, now); err != nil {
		return preparedObservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return preparedObservation{}, err
	}
	return record, nil
}

func (store *Store) insertObservationTx(
	ctx context.Context,
	tx pgx.Tx,
	claim attemptClaim,
	observation Observation,
	record preparedObservation,
	now time.Time,
) error {
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_observations (
			tenant_id,organization_id,observation_id,reservation_id,connection_id,
			connection_version,operation,idempotency_key,external_id,financial_state,
			authority,reconciled,economic_truth,canonical_hash,sealed_record,
			provider_observed_at,captured_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT DO NOTHING
	`, store.tenantID, observation.OrganizationID, record.id, claim.reservationID,
		claim.connectionID, claim.version, claim.operation, claim.idempotencyKey,
		observation.ExternalID, observation.State, observation.Authority,
		observation.Reconciled, observation.EconomicTruth, record.hash.Digest, record.sealed,
		observation.ProviderObservedAt, observation.CapturedAt, now)
	if err != nil {
		return fmt.Errorf("financial adapter: persist authoritative observation: %w", err)
	}
	if command.RowsAffected() == 0 {
		var storedHash string
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash FROM workforce_financial_observations
			WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3
		`, store.tenantID, observation.OrganizationID, record.id).Scan(&storedHash); err != nil || storedHash != record.hash.Digest {
			return ErrConflict
		}
	}
	return nil
}

func (store *Store) completeAttemptTx(
	ctx context.Context,
	tx pgx.Tx,
	claim attemptClaim,
	state, safeCode, externalID, observationHash string,
	now time.Time,
) error {
	command, err := tx.Exec(ctx, `
		UPDATE workforce_financial_attempts
		SET state=$1,safe_code=NULLIF($2,''),external_id=NULLIF($3,''),
			observation_hash=NULLIF($4,''),finished_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND attempt_id=$8 AND state='in_flight'
	`, state, safeCode, externalID, observationHash, now,
		store.tenantID, claim.organizationID, claim.id)
	if err != nil {
		return fmt.Errorf("financial adapter: complete attempt: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (store *Store) resolveAmbiguityTx(ctx context.Context, tx pgx.Tx, claim attemptClaim, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_financial_scope_freezes
		SET state='reconciled',resolved_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND reservation_id=$4 AND state='open'
	`, now, store.tenantID, claim.organizationID, claim.reservationID); err != nil {
		return fmt.Errorf("financial adapter: resolve financial scope freeze: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_financial_incidents
		SET state='resolved',resolved_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND reservation_id=$4 AND state='open'
	`, now, store.tenantID, claim.organizationID, claim.reservationID); err != nil {
		return fmt.Errorf("financial adapter: resolve financial incident: %w", err)
	}
	return nil
}

func (store *Store) recordIncidentTx(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	reservationID string,
	connectionID string,
	connectionVersion uint64,
	operation string,
	idempotencyKey string,
	kind string,
	safeCode string,
	now time.Time,
) error {
	id, err := randomID("fininc-")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_financial_incidents (
			tenant_id,organization_id,incident_id,reservation_id,connection_id,
			connection_version,operation,idempotency_key,kind,safe_code,state,
			created_at,resolved_at
		) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,'open',$11,NULL)
	`, store.tenantID, organizationID, id, reservationID, connectionID,
		connectionVersion, operation, idempotencyKey, kind, safeCode, now)
	if err != nil {
		return fmt.Errorf("financial adapter: persist founder-visible incident: %w", err)
	}
	return nil
}

func (store *Store) failCircuitTx(ctx context.Context, tx pgx.Tx, claim attemptClaim, safeCode string, now time.Time) error {
	var failures uint16
	var window time.Time
	if err := tx.QueryRow(ctx, `
		SELECT failure_count,window_started_at FROM workforce_financial_operation_circuits
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5 FOR UPDATE
	`, store.tenantID, claim.organizationID, claim.connectionID, claim.version,
		claim.operation).Scan(&failures, &window); err != nil {
		return fmt.Errorf("financial adapter: load circuit failure state: %w", err)
	}
	if now.Sub(window) >= claim.policy.CircuitWindow {
		failures, window = 0, now
	}
	failures++
	state := "closed"
	var retryAt *time.Time
	if failures >= claim.policy.FailureThreshold {
		state = "open"
		value := now.Add(claim.policy.CircuitOpenDuration)
		retryAt = &value
	}
	_, err := tx.Exec(ctx, `
		UPDATE workforce_financial_operation_circuits
		SET state=$1,failure_count=$2,window_started_at=$3,retry_at=$4,
			last_safe_code=$5,updated_at=$6,version=version+1
		WHERE tenant_id=$7 AND organization_id=$8 AND connection_id=$9
		  AND connection_version=$10 AND operation=$11
	`, state, failures, window, retryAt, safeCode, now, store.tenantID,
		claim.organizationID, claim.connectionID, claim.version, claim.operation)
	return err
}

func (store *Store) succeedCircuitTx(ctx context.Context, tx pgx.Tx, claim attemptClaim, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE workforce_financial_operation_circuits
		SET state='closed',failure_count=0,window_started_at=$1,retry_at=NULL,
			last_safe_code=NULL,updated_at=$1,version=version+1
		WHERE tenant_id=$2 AND organization_id=$3 AND connection_id=$4
		  AND connection_version=$5 AND operation=$6
	`, now, store.tenantID, claim.organizationID, claim.connectionID,
		claim.version, claim.operation)
	return err
}

func (store *Store) openOutOfBandIncident(
	ctx context.Context,
	claim attemptClaim,
	request Request,
	externalID string,
	observationHash string,
) error {
	return store.markAmbiguous(ctx, claim, request, externalID, observationHash,
		"provider_resource_version_changed", "out_of_band_change")
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
