package productexecution

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"centra/workforce/internal/companylifecycle"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/productcapability"
)

func (store *Store) CompleteLaunch(ctx context.Context, request CompleteLaunchRequest) (View, error) {
	if err := request.ReceiptRequest.Validate(); err != nil || request.CrossAudit.Validate() != nil {
		return View{}, fmt.Errorf("product execution: launch request is invalid")
	}
	start, view, telemetryReceipt, err := store.prepareReceiptWithID(
		ctx, request.ExecutionID, StageTelemetry, request.ReceiptID,
	)
	if err != nil {
		return View{}, err
	}
	if view.Phase == PhaseLaunched {
		return store.receiptReplay(view, telemetryReceipt)
	}
	if view.Phase != PhaseTelemetryQueued || view.DeploymentEffectID == nil {
		return View{}, ErrInvalidPhase
	}
	now, err := store.currentTime()
	if err != nil || request.Record.ValidateAt(now) != nil {
		return View{}, fmt.Errorf("product execution: engineering result is not current and verified")
	}
	body := request.Record.Record.Body
	buildBinding := bindingFor(view, StageBuild)
	verificationBinding := bindingFor(view, StageVerification)
	if body.Kind != productcapability.RecordEngineeringResult || body.Engineering == nil ||
		body.OrganizationID != store.organizationID ||
		string(body.InitiativeID) != string(start.Request.InitiativeID) ||
		body.ProjectID != start.Request.ProjectID || body.WorkspaceID != start.Request.WorkspaceID ||
		body.AuthorSeatID != buildBinding.SeatID ||
		request.Record.Verification.VerifierSeatID != verificationBinding.SeatID ||
		body.Engineering.HandoffID != start.Request.HandoffID ||
		body.Engineering.DeveloperIntentID != buildBinding.IntentID {
		return View{}, ErrUnauthorized
	}
	assessment, err := productcapability.EvaluateLaunch(*body.Engineering, now)
	if err != nil || assessment.State != productcapability.LaunchReady {
		return View{}, fmt.Errorf("product execution: independent launch assessment is not ready")
	}
	if err := store.validateLaunchArtifacts(ctx, view, telemetryReceipt, *body.Engineering); err != nil {
		return View{}, err
	}
	if err := store.validateRecordEvidence(request.Record, request.GateEvidence,
		companylifecycle.RequiredEvidenceFor(
			companylifecycle.StateVerify, companylifecycle.StateLaunch,
			companylifecycle.DecisionAdvance, "",
		)); err != nil {
		return View{}, err
	}
	if err := store.validateCrossAudit(ctx, start, view, request.CrossAudit); err != nil {
		return View{}, err
	}
	if _, err := store.products.Commit(ctx, request.Record); err != nil {
		return View{}, fmt.Errorf("product execution: commit engineering result: %w", err)
	}
	transitionID, correction, err := store.advanceLifecycle(
		ctx, start, verificationBinding, companylifecycle.StateVerify,
		companylifecycle.StateLaunch, request.GateEvidence,
		[]companylifecycle.EffectID{*view.DeploymentEffectID},
	)
	if err != nil {
		return View{}, err
	}
	recordID := body.ID
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseObserved,
		[]productcapability.RecordID{recordID}, []string{string(*view.DeploymentEffectID)},
	); err != nil {
		return View{}, err
	}
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseClosed,
		[]productcapability.RecordID{recordID}, []string{string(*view.DeploymentEffectID)},
	); err != nil {
		return View{}, err
	}
	return store.commitLaunch(
		ctx, request, telemetryReceipt, recordID, transitionID, correction,
	)
}

func (store *Store) validateLaunchArtifacts(
	ctx context.Context,
	view View,
	telemetryReceipt StageReceipt,
	result productcapability.EngineeringResult,
) error {
	byKind := make(map[productcapability.ArtifactKind]productcapability.Artifact, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		byKind[artifact.Kind] = artifact
	}
	for _, required := range []productcapability.ArtifactKind{
		productcapability.ArtifactCapacityEvidence,
		productcapability.ArtifactObservabilityEvidence,
		productcapability.ArtifactTelemetryEvidence,
		productcapability.ArtifactCustomerEvidence,
	} {
		if _, exists := byKind[required]; !exists {
			return fmt.Errorf("product execution: launch record is missing %s", required)
		}
	}
	for artifactKind, evidenceKinds := range map[productcapability.ArtifactKind][]string{
		productcapability.ArtifactRollbackEvidence: {
			"backup_restore",
		},
		productcapability.ArtifactOperationsReadiness: {
			"resource_qualification", "shutdown_qualification",
		},
		productcapability.ArtifactCustomerEvidence: {
			"browser_runtime_e2e", "browser_ui_receipt",
		},
	} {
		artifact, exists := byKind[artifactKind]
		if !exists {
			return fmt.Errorf("product execution: launch record is missing %s", artifactKind)
		}
		for _, kind := range evidenceKinds {
			if !artifactHasEvidenceKind(artifact, kind) {
				return fmt.Errorf("product execution: %s lacks %s evidence", artifactKind, kind)
			}
		}
	}
	openedReceipt, err := store.receipts.OpenReceipt(
		ctx, store.organizationID, telemetryReceipt.ReceiptID,
	)
	if err != nil {
		return err
	}
	telemetry := byKind[productcapability.ArtifactTelemetryEvidence]
	receiptBound := false
	for _, receiptEvidence := range openedReceipt.Evidence {
		for _, artifactEvidence := range telemetry.Evidence {
			if artifactEvidence.Hash == receiptEvidence.Hash {
				receiptBound = true
				break
			}
		}
	}
	if !receiptBound {
		return fmt.Errorf("product execution: telemetry evidence is not bound to the telemetry wake receipt")
	}
	var deploymentEvidence contracts.ContentHash
	for _, item := range view.Effects {
		if view.DeploymentEffectID != nil && item.EffectID == *view.DeploymentEffectID &&
			item.State == "committed" && item.EvidenceHash != nil && item.ReconciledAt != nil {
			deploymentEvidence = *item.EvidenceHash
			break
		}
	}
	if deploymentEvidence.Validate() != nil {
		return ErrAmbiguousEffect
	}
	deployment := byKind[productcapability.ArtifactDeploymentState]
	if deployment.Artifact.Hash != deploymentEvidence {
		found := false
		for _, evidence := range deployment.Evidence {
			if evidence.Hash == deploymentEvidence {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("product execution: deployment artifact does not bind the reconciled provider receipt")
		}
	}
	return nil
}

func artifactHasEvidenceKind(artifact productcapability.Artifact, kind string) bool {
	for _, evidence := range artifact.Evidence {
		if evidence.Kind == kind {
			return true
		}
	}
	return false
}

func (store *Store) validateCrossAudit(
	ctx context.Context,
	start StartRecord,
	view View,
	proof CrossAuditProof,
) error {
	verificationReceipt := StageReceipt{}
	for _, receipt := range view.Receipts {
		if receipt.Stage == StageVerification {
			verificationReceipt = receipt
			break
		}
	}
	if verificationReceipt.VerdictID == "" ||
		proof.OriginalVerdictID != verificationReceipt.VerdictID {
		return ErrUnauthorized
	}
	var reauditVerdict, originalOutcome, reauditOutcome, reauditorSeat, executingSeat string
	var disagreement bool
	err := store.pool.QueryRow(ctx, `
		SELECT result.reaudit_verdict_id,result.original_outcome,result.reaudit_outcome,
		       result.disagreement,selection.reauditor_seat_id,original.executing_seat_id
		FROM workforce_cross_audit_results result
		JOIN workforce_cross_audit_selections selection
		  ON selection.tenant_id=result.tenant_id
		 AND selection.organization_id=result.organization_id
		 AND selection.epoch_id=result.epoch_id
		 AND selection.verdict_id=result.original_verdict_id
		JOIN workforce_verdict_records original
		  ON original.tenant_id=result.tenant_id
		 AND original.organization_id=result.organization_id
		 AND original.verdict_id=result.original_verdict_id
		WHERE result.tenant_id=$1 AND result.organization_id=$2
		  AND result.epoch_id=$3 AND result.original_verdict_id=$4
	`, store.tenantID, store.organizationID, proof.EpochID, proof.OriginalVerdictID).Scan(
		&reauditVerdict, &originalOutcome, &reauditOutcome, &disagreement,
		&reauditorSeat, &executingSeat,
	)
	if err != nil || reauditVerdict != string(proof.ReauditVerdictID) || disagreement ||
		originalOutcome != string(contracts.VerdictPass) ||
		reauditOutcome != string(contracts.VerdictPass) || reauditorSeat == executingSeat {
		return fmt.Errorf("product execution: cross-audit did not independently reconfirm verification")
	}
	member := false
	for _, candidate := range start.Assignment.Members {
		if string(candidate.SeatID) == reauditorSeat && candidate.Role == contracts.SeatAuditor {
			member = true
			break
		}
	}
	if !member {
		return fmt.Errorf("product execution: cross-auditor is outside the dynamically assigned squad")
	}
	return nil
}

func (store *Store) commitLaunch(
	ctx context.Context,
	request CompleteLaunchRequest,
	receipt StageReceipt,
	recordID productcapability.RecordID,
	transitionID companylifecycle.TransitionID,
	correction companylifecycle.CorrectionBinding,
) (View, error) {
	now, err := store.currentTime()
	if err != nil {
		return View{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return View{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var phase Phase
	var version uint64
	var effectID companylifecycle.EffectID
	if err := tx.QueryRow(ctx, `
		SELECT phase,version,deployment_effect_id
		FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, request.ExecutionID).Scan(
		&phase, &version, &effectID,
	); err != nil {
		return View{}, err
	}
	if phase == PhaseLaunched {
		if err := tx.Commit(ctx); err != nil {
			return View{}, err
		}
		return store.Load(ctx, request.ExecutionID)
	}
	if phase != PhaseTelemetryQueued {
		return View{}, ErrInvalidPhase
	}
	if err := store.insertStageReceiptTx(ctx, tx, request.ExecutionID, receipt); err != nil {
		return View{}, err
	}
	if err := store.insertCorrectionTx(ctx, tx, request.ExecutionID, transitionID, correction); err != nil {
		return View{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_product_execution_cross_audits (
			tenant_id,organization_id,execution_id,epoch_id,original_verdict_id,
			reaudit_verdict_id,disagreement,recorded_at
		) VALUES ($1,$2,$3,$4,$5,$6,FALSE,$7)
		ON CONFLICT (tenant_id,organization_id,execution_id,epoch_id,original_verdict_id)
		DO NOTHING
	`, store.tenantID, store.organizationID, request.ExecutionID,
		request.CrossAudit.EpochID, request.CrossAudit.OriginalVerdictID,
		request.CrossAudit.ReauditVerdictID, now); err != nil {
		return View{}, err
	}
	effectCommand, err := tx.Exec(ctx, `
		UPDATE workforce_product_execution_effects SET state='consumed',updated_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND execution_id=$4 AND effect_id=$5
		  AND state='committed'
	`, now, store.tenantID, store.organizationID, request.ExecutionID, effectID)
	if err != nil || effectCommand.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return View{}, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_product_executions
		SET phase='launched',version=version+1,engineering_record_id=$1,
		    launch_transition_id=$2,checkpoint_version=(
		      SELECT version FROM workforce_product_capability_checkpoint_heads
		      WHERE tenant_id=$3 AND organization_id=$4 AND initiative_id=(
		        SELECT initiative_id FROM workforce_product_executions
		        WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5
		      )
		    ),updated_at=$6
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5 AND version=$7
	`, recordID, transitionID, store.tenantID, store.organizationID,
		request.ExecutionID, now, version)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return View{}, err
	}
	stage := StageTelemetry
	if err := store.appendEventTx(ctx, tx, request.ExecutionID, PhaseLaunched,
		"launch_committed", &stage, string(recordID), request.IdempotencyKey, now); err != nil {
		return View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, err
	}
	return store.Load(ctx, request.ExecutionID)
}
