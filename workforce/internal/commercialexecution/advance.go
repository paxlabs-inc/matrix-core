package commercialexecution

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"centra/workforce/internal/contracts"
)

func (store *Store) RecordEvidence(ctx context.Context, evidence Evidence) (Snapshot, bool, error) {
	if store == nil || evidence.Body.OrganizationID != store.organizationID {
		return Snapshot{}, false, ErrUnauthorized
	}
	issuer, ok := store.issuers[evidence.Signature.KeyID]
	if !ok || !phaseAllowed(issuer.phases, evidence.Body.Phase) ||
		VerifyEvidence(evidence, evidence.Signature.KeyID, issuer.publicKey) != nil {
		return Snapshot{}, false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return Snapshot{}, false, err
	}
	if evidence.Body.CapturedAt.After(now.Add(5 * time.Minute)) {
		return Snapshot{}, false, ErrUnauthorized
	}
	if err := store.authorize(ctx, evidence.Body.Authority, nil); err != nil {
		return Snapshot{}, false, err
	}
	current, err := store.Load(ctx, evidence.Body.ExecutionID)
	if err != nil || current.Plan.Body.InitiativeID != evidence.Body.InitiativeID ||
		current.Plan.Body.WorkOrderID != evidence.Body.WorkOrderID ||
		current.Plan.Body.WorkOrderHash != evidence.Body.WorkOrderHash ||
		evidence.Body.SubjectHash != current.Plan.Body.Scope.AudienceHash {
		return Snapshot{}, false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&evidence)
	if err != nil {
		return Snapshot{}, false, err
	}
	hash, err := EvidenceHash(evidence)
	if err != nil {
		return Snapshot{}, false, err
	}
	sealed, err := store.vault.SealRecord(store.evidenceAD(evidence.Body.ExecutionID, evidence.Body.ID), canonical)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: seal evidence: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: begin evidence commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(evidence.Body.ExecutionID)); err != nil {
		return Snapshot{}, false, err
	}
	var replayID, replayHash string
	err = tx.QueryRow(ctx, `
		SELECT evidence_id,canonical_hash FROM workforce_commercial_execution_evidence
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		  AND (evidence_id=$4 OR idempotency_key=$5)
	`, store.tenantID, store.organizationID, evidence.Body.ExecutionID,
		evidence.Body.ID, evidence.Body.IdempotencyKey).Scan(&replayID, &replayHash)
	if err == nil {
		if replayID != string(evidence.Body.ID) || replayHash != hash.Digest {
			return Snapshot{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Snapshot{}, false, err
		}
		view, err := store.Load(ctx, evidence.Body.ExecutionID)
		if err != nil {
			return Snapshot{}, false, err
		}
		return view, false, stateError(view.State)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, false, err
	}
	var initiativeID, workOrderID, workOrderHash string
	var executionState State
	var currentPhase Phase
	var executionVersion uint64
	var deadline time.Time
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,work_order_id,work_order_hash,state,current_phase,version,deadline
		FROM workforce_commercial_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, evidence.Body.ExecutionID).Scan(
		&initiativeID, &workOrderID, &workOrderHash, &executionState,
		&currentPhase, &executionVersion, &deadline,
	); err != nil {
		return Snapshot{}, false, ErrConflict
	}
	if initiativeID != evidence.Body.InitiativeID || workOrderID != string(evidence.Body.WorkOrderID) ||
		workOrderHash != evidence.Body.WorkOrderHash.Digest || currentPhase != evidence.Body.Phase ||
		executionState == StateCompleted || executionState == StateFailed || !deadline.After(now) {
		return Snapshot{}, false, ErrOutOfOrder
	}
	var stepState State
	var attempt uint32
	var activeEvidence *string
	if err := tx.QueryRow(ctx, `
		SELECT state,attempt,active_evidence_id FROM workforce_commercial_execution_steps
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 AND phase=$4 FOR UPDATE
	`, store.tenantID, store.organizationID, evidence.Body.ExecutionID, evidence.Body.Phase).Scan(
		&stepState, &attempt, &activeEvidence,
	); err != nil || attempt != evidence.Body.Attempt {
		return Snapshot{}, false, ErrOutOfOrder
	}
	if err := store.verifyPreviousEvidence(ctx, tx, evidence.Body, activeEvidence); err != nil {
		return Snapshot{}, false, err
	}
	if err := store.verifyEvidenceSources(ctx, tx, evidence.Body, current.Plan.Body.Scope, now); err != nil {
		return Snapshot{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_commercial_execution_evidence (
			tenant_id,organization_id,evidence_id,execution_id,phase,attempt,disposition,
			subject_hash,canonical_hash,issuer_key_id,sealed_record,reason_code,
			idempotency_key,observed_at,captured_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,$15,$16)
	`, store.tenantID, store.organizationID, evidence.Body.ID, evidence.Body.ExecutionID,
		evidence.Body.Phase, evidence.Body.Attempt, evidence.Body.Disposition,
		evidence.Body.SubjectHash.Digest, hash.Digest, evidence.Signature.KeyID, sealed,
		evidence.Body.ReasonCode, evidence.Body.IdempotencyKey, evidence.Body.ObservedAt,
		evidence.Body.CapturedAt, now); err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: persist evidence: %w", err)
	}
	for _, source := range evidence.Body.Sources {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_commercial_execution_evidence_sources (
				tenant_id,organization_id,evidence_id,role,source_kind,source_id_hash,
				source_version,content_hash,operation,provider,account_ref_hash,
				external_ref_hash,related_id_hash,valuation_time,source_state,authority,observed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),
			          NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),$14,$15,$16,$17)
		`, store.tenantID, store.organizationID, evidence.Body.ID, source.Role, source.Kind,
			identifierHash(source.RecordID), source.Version, source.Hash.Digest, source.Operation,
			source.Provider, identifierHash(source.AccountRef), identifierHash(source.ExternalRef),
			identifierHash(source.RelatedID), source.ValuationTime, source.State,
			source.Authority, source.ObservedAt); err != nil {
			return Snapshot{}, false, fmt.Errorf("commercial execution: persist evidence source: %w", err)
		}
	}
	newExecutionState := evidence.Body.Disposition
	newCurrentPhase := evidence.Body.Phase
	completedAt := any(nil)
	if evidence.Body.Disposition == StateCompleted {
		completedAt = now
		if next, found := nextPhase(evidence.Body.Phase); found {
			newCurrentPhase = next
			newExecutionState = StatePendingExternal
			if evidence.Body.Phase == PhaseFinancialIntent {
				newExecutionState = StateReconciling
				if _, err := tx.Exec(ctx, `
					UPDATE workforce_commercial_execution_steps SET state='reconciling',updated_at=$1
					WHERE tenant_id=$2 AND organization_id=$3 AND execution_id=$4 AND phase=$5
				`, now, store.tenantID, store.organizationID, evidence.Body.ExecutionID, next); err != nil {
					return Snapshot{}, false, err
				}
			}
		}
	}
	stepCommand, err := tx.Exec(ctx, `
		UPDATE workforce_commercial_execution_steps
		SET state=$1,active_evidence_id=$2,safe_code=NULLIF($3,''),updated_at=$4,completed_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND execution_id=$8 AND phase=$9 AND attempt=$10
	`, evidence.Body.Disposition, evidence.Body.ID, evidence.Body.ReasonCode, now, completedAt,
		store.tenantID, store.organizationID, evidence.Body.ExecutionID, evidence.Body.Phase,
		evidence.Body.Attempt)
	if err != nil || stepCommand.RowsAffected() != 1 {
		return Snapshot{}, false, ErrConflict
	}
	executionCommand, err := tx.Exec(ctx, `
		UPDATE workforce_commercial_executions
		SET state=$1,current_phase=$2,version=version+1,updated_at=$3
		WHERE tenant_id=$4 AND organization_id=$5 AND execution_id=$6 AND version=$7
	`, newExecutionState, newCurrentPhase, now, store.tenantID, store.organizationID,
		evidence.Body.ExecutionID, executionVersion)
	if err != nil || executionCommand.RowsAffected() != 1 {
		return Snapshot{}, false, ErrConflict
	}
	if evidence.Body.Disposition == StateReconciling || evidence.Body.Disposition == StateFailed {
		kind := "external_ambiguity"
		if evidence.Body.Disposition == StateFailed {
			kind = "commercial_step_failed"
		}
		if err := store.recordIncident(ctx, tx, evidence.Body.ExecutionID, evidence.Body.Phase,
			kind, evidence.Body.ReasonCode, now); err != nil {
			return Snapshot{}, false, err
		}
	}
	if evidence.Body.Disposition == StateCompleted {
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_commercial_execution_incidents
			SET state='resolved',resolved_at=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND execution_id=$4 AND phase=$5 AND state='open'
		`, now, store.tenantID, store.organizationID, evidence.Body.ExecutionID,
			evidence.Body.Phase); err != nil {
			return Snapshot{}, false, err
		}
	}
	recoveryState := "in_progress"
	if evidence.Body.Disposition == StateCompleted {
		recoveryState = "completed"
	} else if evidence.Body.Disposition == StateFailed {
		recoveryState = "failed"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_commercial_execution_recoveries
		SET state=$1,resolved_at=CASE WHEN $1 IN ('completed','failed') THEN $2 ELSE NULL END
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5 AND target_phase=$6
		  AND state='in_progress'
	`, recoveryState, now, store.tenantID, store.organizationID,
		evidence.Body.ExecutionID, evidence.Body.Phase); err != nil {
		return Snapshot{}, false, err
	}
	if err := store.insertTransition(ctx, tx, evidence.Body.ExecutionID, evidence.Body.Phase,
		"evidence_"+string(evidence.Body.Disposition), executionState, newExecutionState,
		evidence.Body.ID, evidence.Body.Authority, evidence.Body.IdempotencyKey, now); err != nil {
		return Snapshot{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, false, fmt.Errorf("commercial execution: commit evidence: %w", err)
	}
	view, err := store.Load(ctx, evidence.Body.ExecutionID)
	if err != nil {
		return Snapshot{}, false, err
	}
	return view, true, stateError(view.State)
}

func (store *Store) ApplyCorrection(ctx context.Context, correction Correction) (Snapshot, bool, error) {
	if store == nil || correction.Body.OrganizationID != store.organizationID ||
		VerifyCorrection(correction, store.controllerKey, store.controllerPub) != nil {
		return Snapshot{}, false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return Snapshot{}, false, err
	}
	if correction.Body.IssuedAt.After(now) ||
		store.authorize(ctx, correction.Body.Authority, nil) != nil {
		return Snapshot{}, false, ErrUnauthorized
	}
	current, err := store.Load(ctx, correction.Body.ExecutionID)
	if err != nil || current.Plan.Body.InitiativeID != correction.Body.InitiativeID {
		return Snapshot{}, false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&correction)
	if err != nil {
		return Snapshot{}, false, err
	}
	hash, err := CorrectionHash(correction)
	if err != nil {
		return Snapshot{}, false, err
	}
	sealed, err := store.vault.SealRecord(store.correctionAD(correction.Body.ExecutionID, correction.Body.ID), canonical)
	if err != nil {
		return Snapshot{}, false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Snapshot{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(correction.Body.ExecutionID)); err != nil {
		return Snapshot{}, false, err
	}
	var replayID, replayHash string
	err = tx.QueryRow(ctx, `
		SELECT correction_id,canonical_hash FROM workforce_commercial_execution_corrections
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		  AND (correction_id=$4 OR idempotency_key=$5)
	`, store.tenantID, store.organizationID, correction.Body.ExecutionID,
		correction.Body.ID, correction.Body.IdempotencyKey).Scan(&replayID, &replayHash)
	if err == nil {
		if replayID != string(correction.Body.ID) || replayHash != hash.Digest {
			return Snapshot{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Snapshot{}, false, err
		}
		view, err := store.Load(ctx, correction.Body.ExecutionID)
		return view, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, false, err
	}
	var initiativeID string
	var oldState State
	var version uint64
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,state,version FROM workforce_commercial_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, correction.Body.ExecutionID).Scan(
		&initiativeID, &oldState, &version,
	); err != nil || initiativeID != correction.Body.InitiativeID {
		return Snapshot{}, false, ErrConflict
	}
	var evidenceHash string
	if err := tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_commercial_execution_evidence
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		  AND evidence_id=$4 AND phase=$5
	`, store.tenantID, store.organizationID, correction.Body.ExecutionID,
		correction.Body.TargetEvidenceID, correction.Body.TargetPhase).Scan(&evidenceHash); err != nil ||
		evidenceHash != correction.Body.TargetHash.Digest {
		return Snapshot{}, false, ErrConflict
	}
	for _, source := range correction.Body.Sources {
		if err := store.verifySource(ctx, tx, correction.Body.InitiativeID, correction.Body.TargetPhase,
			SourceRef{Role: RoleCorrectionEvidence, Kind: source.Kind, RecordID: source.RecordID,
				Version: source.Version, Hash: source.Hash, Operation: source.Operation,
				Provider: source.Provider, AccountRef: source.AccountRef, ExternalRef: source.ExternalRef,
				RelatedID: source.RelatedID, ValuationTime: source.ValuationTime,
				State: source.State, Authority: source.Authority, ObservedAt: source.ObservedAt},
			current.Plan.Body.Scope, now); err != nil {
			return Snapshot{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_commercial_execution_corrections (
			tenant_id,organization_id,correction_id,execution_id,target_phase,
			target_evidence_id,target_hash,kind,canonical_hash,controller_key_id,
			sealed_record,reason,idempotency_key,issued_at,applied_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`, store.tenantID, store.organizationID, correction.Body.ID, correction.Body.ExecutionID,
		correction.Body.TargetPhase, correction.Body.TargetEvidenceID,
		correction.Body.TargetHash.Digest, correction.Body.Kind, hash.Digest,
		store.controllerKey, sealed, correction.Body.Reason, correction.Body.IdempotencyKey,
		correction.Body.IssuedAt, now); err != nil {
		return Snapshot{}, false, err
	}
	ordinal := phaseOrdinal(correction.Body.TargetPhase) + 1
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_commercial_execution_correction_impacts (
			tenant_id,organization_id,correction_id,execution_id,phase,evidence_id,evidence_hash,created_at
		)
		SELECT $1,$2,$3,step.execution_id,step.phase,step.active_evidence_id,evidence.canonical_hash,$4
		FROM workforce_commercial_execution_steps step
		JOIN workforce_commercial_execution_evidence evidence
		  ON evidence.tenant_id=step.tenant_id AND evidence.organization_id=step.organization_id
		 AND evidence.execution_id=step.execution_id AND evidence.evidence_id=step.active_evidence_id
		WHERE step.tenant_id=$1 AND step.organization_id=$2 AND step.execution_id=$5
		  AND step.ordinal >= $6 AND step.active_evidence_id IS NOT NULL
	`, store.tenantID, store.organizationID, correction.Body.ID, now,
		correction.Body.ExecutionID, ordinal); err != nil {
		return Snapshot{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_commercial_execution_steps
		SET state='pending_external',attempt=attempt+1,active_evidence_id=NULL,
			safe_code='corrected_evidence_invalidated',updated_at=$1,completed_at=NULL
		WHERE tenant_id=$2 AND organization_id=$3 AND execution_id=$4 AND ordinal >= $5
	`, now, store.tenantID, store.organizationID, correction.Body.ExecutionID, ordinal); err != nil {
		return Snapshot{}, false, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_commercial_executions
		SET state='reconciling',current_phase=$1,version=version+1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5 AND version=$6
	`, correction.Body.TargetPhase, now, store.tenantID, store.organizationID,
		correction.Body.ExecutionID, version)
	if err != nil || command.RowsAffected() != 1 {
		return Snapshot{}, false, ErrConflict
	}
	if err := store.recordIncident(ctx, tx, correction.Body.ExecutionID, correction.Body.TargetPhase,
		"correction_required", "commercial_lineage_corrected", now); err != nil {
		return Snapshot{}, false, err
	}
	if err := store.insertTransition(ctx, tx, correction.Body.ExecutionID, correction.Body.TargetPhase,
		"correction_applied", oldState, StateReconciling, "", correction.Body.Authority,
		correction.Body.IdempotencyKey, now); err != nil {
		return Snapshot{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, false, err
	}
	view, err := store.Load(ctx, correction.Body.ExecutionID)
	return view, true, err
}

func (store *Store) Recover(ctx context.Context, recovery Recovery) (Snapshot, bool, error) {
	if store == nil || recovery.Body.OrganizationID != store.organizationID ||
		VerifyRecovery(recovery, store.controllerKey, store.controllerPub) != nil {
		return Snapshot{}, false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return Snapshot{}, false, err
	}
	if recovery.Body.IssuedAt.After(now) ||
		store.authorize(ctx, recovery.Body.Authority, nil) != nil {
		return Snapshot{}, false, ErrUnauthorized
	}
	canonical, err := contracts.EncodeCanonical(&recovery)
	if err != nil {
		return Snapshot{}, false, err
	}
	hash, err := RecoveryHash(recovery)
	if err != nil {
		return Snapshot{}, false, err
	}
	sealed, err := store.vault.SealRecord(store.recoveryAD(recovery.Body.ExecutionID, recovery.Body.ID), canonical)
	if err != nil {
		return Snapshot{}, false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Snapshot{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(recovery.Body.ExecutionID)); err != nil {
		return Snapshot{}, false, err
	}
	var replayID, replayHash string
	err = tx.QueryRow(ctx, `
		SELECT recovery_id,canonical_hash FROM workforce_commercial_execution_recoveries
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		  AND (recovery_id=$4 OR idempotency_key=$5)
	`, store.tenantID, store.organizationID, recovery.Body.ExecutionID,
		recovery.Body.ID, recovery.Body.IdempotencyKey).Scan(&replayID, &replayHash)
	if err == nil {
		if replayID != string(recovery.Body.ID) || replayHash != hash.Digest {
			return Snapshot{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Snapshot{}, false, err
		}
		view, err := store.Load(ctx, recovery.Body.ExecutionID)
		return view, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, false, err
	}
	var initiativeID string
	var oldState State
	var currentPhase Phase
	var version uint64
	if err := tx.QueryRow(ctx, `
		SELECT initiative_id,state,current_phase,version FROM workforce_commercial_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, recovery.Body.ExecutionID).Scan(
		&initiativeID, &oldState, &currentPhase, &version,
	); err != nil || initiativeID != recovery.Body.InitiativeID || currentPhase != recovery.Body.TargetPhase ||
		oldState != StateFailed && oldState != StateReconciling {
		return Snapshot{}, false, ErrOutOfOrder
	}
	var correctionExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_commercial_execution_corrections
			WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
			  AND correction_id=$4 AND target_phase=$5
		)
	`, store.tenantID, store.organizationID, recovery.Body.ExecutionID,
		recovery.Body.CorrectionID, recovery.Body.TargetPhase).Scan(&correctionExists); err != nil || !correctionExists {
		return Snapshot{}, false, ErrUnauthorized
	}
	newState := StatePendingExternal
	if recovery.Body.Strategy == RecoveryReconcile {
		newState = StateReconciling
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_commercial_execution_recoveries (
			tenant_id,organization_id,recovery_id,execution_id,target_phase,correction_id,
			strategy,state,canonical_hash,controller_key_id,sealed_record,idempotency_key,
			issued_at,started_at,resolved_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'in_progress',$8,$9,$10,$11,$12,$13,NULL)
	`, store.tenantID, store.organizationID, recovery.Body.ID, recovery.Body.ExecutionID,
		recovery.Body.TargetPhase, recovery.Body.CorrectionID, recovery.Body.Strategy,
		hash.Digest, store.controllerKey, sealed, recovery.Body.IdempotencyKey,
		recovery.Body.IssuedAt, now); err != nil {
		return Snapshot{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_commercial_execution_steps
		SET state=$1,attempt=attempt+1,active_evidence_id=NULL,
			safe_code='recovery_in_progress',updated_at=$2,completed_at=NULL
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5 AND phase=$6
	`, newState, now, store.tenantID, store.organizationID,
		recovery.Body.ExecutionID, recovery.Body.TargetPhase); err != nil {
		return Snapshot{}, false, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_commercial_executions
		SET state=$1,version=version+1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5 AND version=$6
	`, newState, now, store.tenantID, store.organizationID, recovery.Body.ExecutionID, version)
	if err != nil || command.RowsAffected() != 1 {
		return Snapshot{}, false, ErrConflict
	}
	if err := store.insertTransition(ctx, tx, recovery.Body.ExecutionID, recovery.Body.TargetPhase,
		"recovery_started", oldState, newState, "", recovery.Body.Authority,
		recovery.Body.IdempotencyKey, now); err != nil {
		return Snapshot{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, false, err
	}
	view, err := store.Load(ctx, recovery.Body.ExecutionID)
	return view, true, err
}

func (store *Store) verifyPreviousEvidence(
	ctx context.Context,
	tx pgx.Tx,
	body EvidenceBody,
	activeEvidence *string,
) error {
	if body.PreviousEvidenceID == nil {
		if activeEvidence != nil {
			return ErrConflict
		}
		return nil
	}
	if activeEvidence == nil || *activeEvidence != string(*body.PreviousEvidenceID) {
		return ErrConflict
	}
	var hash string
	if err := tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_commercial_execution_evidence
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 AND evidence_id=$4
	`, store.tenantID, store.organizationID, body.ExecutionID, *body.PreviousEvidenceID).Scan(&hash); err != nil ||
		hash != body.PreviousHash.Digest {
		return ErrConflict
	}
	return nil
}

func phaseAllowed(phases []Phase, phase Phase) bool {
	return slices.Contains(phases, phase)
}

func stateError(state State) error {
	switch state {
	case StatePendingExternal:
		return ErrPending
	case StateReconciling:
		return ErrReconciling
	case StateFailed:
		return ErrFailed
	default:
		return nil
	}
}
