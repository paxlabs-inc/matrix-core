package productexecution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/productcapability"
)

func (store *Store) ExecuteDeployment(ctx context.Context, request DeploymentRequest) (View, error) {
	if err := request.Validate(); err != nil {
		return View{}, err
	}
	current, err := store.Load(ctx, request.ExecutionID)
	if err != nil {
		return View{}, err
	}
	if current.Phase == PhaseTelemetryQueued || current.Phase == PhaseLaunchReady || current.Phase == PhaseLaunched {
		return store.deploymentReplay(current, request)
	}
	start, view, lifecycleRequest, err := store.prepareDeployment(ctx, request)
	if err != nil {
		return View{}, err
	}
	if view.Phase != PhaseDeploymentPending {
		return View{}, ErrInvalidPhase
	}
	if _, err := store.lifecycle.PrepareEffect(ctx, lifecycleRequest); err != nil {
		return View{}, err
	}
	if err := store.recordPreparedEffect(ctx, request, lifecycleRequest); err != nil {
		return View{}, err
	}
	result, executeErr := store.effects.Execute(ctx, request.Proposal)
	return store.finishDeploymentResult(ctx, start, request, lifecycleRequest, result, executeErr)
}

func (store *Store) ReconcileDeployment(ctx context.Context, request DeploymentRequest) (View, error) {
	if err := request.Validate(); err != nil {
		return View{}, err
	}
	current, err := store.Load(ctx, request.ExecutionID)
	if err != nil {
		return View{}, err
	}
	if current.Phase == PhaseTelemetryQueued || current.Phase == PhaseLaunchReady || current.Phase == PhaseLaunched {
		return store.deploymentReplay(current, request)
	}
	start, view, lifecycleRequest, err := store.prepareDeployment(ctx, request)
	if err != nil {
		return View{}, err
	}
	if view.Phase != PhaseDeploymentAmbiguous && view.Phase != PhaseDeploymentPending {
		return View{}, ErrInvalidPhase
	}
	recovery, err := store.lifecycle.Recover(ctx, store.organizationID, start.Request.InitiativeID)
	if err != nil {
		return View{}, err
	}
	found := false
	for _, pending := range recovery.Effects {
		if pending.Request.ID == lifecycleRequest.ID {
			found = true
			if pending.Status == companylifecycle.EffectCommitted && pending.Commit != nil {
				if strings.HasPrefix(pending.Commit.ExternalReceiptID, "gateway:") &&
					strings.HasSuffix(pending.Commit.ExternalReceiptID, ":failed") {
					if err := store.markDeploymentState(
						ctx, request.ExecutionID, lifecycleRequest.ID, "failed", PhaseFailed,
						pending.Commit.ExternalReceiptID, &pending.Commit.ExternalReceiptHash,
						"deployment_failed", request.IdempotencyKey+":failed",
					); err != nil {
						return View{}, err
					}
					return View{}, fmt.Errorf("product execution: deployment failed definitively")
				}
				return store.finalizeDeployment(ctx, start, request, lifecycleRequest, effect.Result{
					ProposalID: request.Proposal.ID, State: effect.StateSucceeded,
					ExternalID:   pending.Commit.ExternalReceiptID,
					EvidenceHash: pending.Commit.ExternalReceiptHash,
					ObservedAt:   pending.Commit.ExternalCommittedAt, Deduplicated: true,
				})
			}
			break
		}
	}
	if !found {
		return View{}, ErrConflict
	}
	result, reconcileErr := store.effects.Reconcile(ctx, request.Proposal)
	return store.finishDeploymentResult(ctx, start, request, lifecycleRequest, result, reconcileErr)
}

func (store *Store) deploymentReplay(view View, request DeploymentRequest) (View, error) {
	hash := effect.ProposalHash(request.Proposal)
	for _, recorded := range view.Effects {
		if recorded.ProposalID == request.Proposal.ID && recorded.ProposalHash.Digest == hash {
			return view, nil
		}
	}
	return View{}, ErrConflict
}

func (store *Store) prepareDeployment(
	ctx context.Context,
	request DeploymentRequest,
) (StartRecord, View, companylifecycle.EffectRequest, error) {
	if err := request.Validate(); err != nil {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, err
	}
	start, err := store.loadStart(ctx, request.ExecutionID)
	if err != nil {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, err
	}
	view, err := store.Load(ctx, request.ExecutionID)
	if err != nil {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, err
	}
	binding := bindingFor(view, StageDeployment)
	proposal := request.Proposal
	if proposal.OrganizationID != store.organizationID || proposal.IntentID != binding.IntentID ||
		proposal.NodeID != dependency.NodeID(binding.IntentID) || proposal.SeatID != binding.SeatID {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, ErrUnauthorized
	}
	order, err := store.currentStageOrder(ctx, start, binding)
	if err != nil || !slices.Contains(order.Binding.EffectIdentities, proposal.ID) {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, ErrUnauthorized
	}
	checkpoint, err := store.lifecycle.Load(ctx, store.organizationID, start.Request.InitiativeID)
	if err != nil || checkpoint.State != companylifecycle.StateVerify ||
		checkpoint.Authority.RequestedBySeatID != binding.SeatID {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, ErrInvalidPhase
	}
	receipt, err := store.lifecycle.LoadReceipt(
		ctx, store.organizationID, start.Request.InitiativeID, checkpoint.LastReceiptID,
	)
	if err != nil {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, err
	}
	proposalHash := contracts.ContentHash{Algorithm: "sha256", Digest: effect.ProposalHash(proposal)}
	lifecycleRequest := companylifecycle.EffectRequest{
		SchemaVersion: companylifecycle.EffectSchemaVersion,
		ID:            companylifecycle.EffectID(proposal.ID), OrganizationID: store.organizationID,
		InitiativeID:             start.Request.InitiativeID,
		ExpectedLifecycleVersion: checkpoint.Version,
		ExternalSystem:           proposal.Provider, Operation: proposal.Operation,
		RequestHash: proposalHash, ExternalIdempotencyKey: proposal.IdempotencyKey,
		AuthorizationReceiptID:    checkpoint.LastReceiptID,
		AuthorizationDecisionHash: receipt.Verification.PolicyDecisionHash,
		PreparedBySeatID:          binding.SeatID, LeaseID: proposal.LeaseID,
		Fence: proposal.Fence, LeaseExpiresAt: proposal.Deadline,
	}
	if err := lifecycleRequest.Validate(); err != nil {
		return StartRecord{}, View{}, companylifecycle.EffectRequest{}, err
	}
	return start, view, lifecycleRequest, nil
}

func (store *Store) recordPreparedEffect(
	ctx context.Context,
	request DeploymentRequest,
	lifecycleRequest companylifecycle.EffectRequest,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_product_execution_effects (
			tenant_id,organization_id,execution_id,effect_id,proposal_id,proposal_hash,
			operation,state,prepared_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'prepared',$8,$8)
		ON CONFLICT (tenant_id,organization_id,execution_id,effect_id) DO NOTHING
	`, store.tenantID, store.organizationID, request.ExecutionID, lifecycleRequest.ID,
		request.Proposal.ID, lifecycleRequest.RequestHash.Digest,
		request.Proposal.Operation, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var proposalID, hash, operation string
		if err := store.pool.QueryRow(ctx, `
			SELECT proposal_id,proposal_hash,operation
			FROM workforce_product_execution_effects
			WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 AND effect_id=$4
		`, store.tenantID, store.organizationID, request.ExecutionID, lifecycleRequest.ID).Scan(
			&proposalID, &hash, &operation,
		); err != nil || proposalID != request.Proposal.ID ||
			hash != lifecycleRequest.RequestHash.Digest || operation != request.Proposal.Operation {
			return ErrConflict
		}
	}
	return nil
}

func (store *Store) finishDeploymentResult(
	ctx context.Context,
	start StartRecord,
	request DeploymentRequest,
	lifecycleRequest companylifecycle.EffectRequest,
	result effect.Result,
	resultErr error,
) (View, error) {
	switch result.State {
	case effect.StateSucceeded:
		if resultErr != nil {
			return View{}, resultErr
		}
		return store.finalizeDeployment(ctx, start, request, lifecycleRequest, result)
	case effect.StateExternallyAmbiguous, effect.StateDispatching:
		if err := store.markDeploymentState(
			ctx, request.ExecutionID, lifecycleRequest.ID, "ambiguous",
			PhaseDeploymentAmbiguous, result.ExternalID, nil,
			"deployment_ambiguous", request.IdempotencyKey,
		); err != nil {
			return View{}, err
		}
		return View{}, ErrAmbiguousEffect
	case effect.StateFailed:
		if err := store.commitFailedEffect(ctx, request, lifecycleRequest, result); err != nil {
			return View{}, err
		}
		if resultErr != nil {
			return View{}, fmt.Errorf("product execution: deployment failed definitively: %w", resultErr)
		}
		return View{}, fmt.Errorf("product execution: deployment failed definitively")
	default:
		if resultErr != nil {
			_ = store.markDeploymentState(
				ctx, request.ExecutionID, lifecycleRequest.ID, "prepared", PhaseFailed,
				"", nil, "deployment_rejected", request.IdempotencyKey,
			)
			return View{}, resultErr
		}
		return View{}, ErrAmbiguousEffect
	}
}

func (store *Store) finalizeDeployment(
	ctx context.Context,
	start StartRecord,
	request DeploymentRequest,
	lifecycleRequest companylifecycle.EffectRequest,
	result effect.Result,
) (View, error) {
	if strings.TrimSpace(result.ExternalID) == "" || result.EvidenceHash.Validate() != nil ||
		!validUTC(result.ObservedAt) {
		return View{}, ErrIntegrity
	}
	commit := companylifecycle.EffectCommit{
		SchemaVersion: companylifecycle.EffectSchemaVersion,
		EffectID:      lifecycleRequest.ID, OrganizationID: store.organizationID,
		InitiativeID: start.Request.InitiativeID, RequestHash: lifecycleRequest.RequestHash,
		OutcomeHash: result.EvidenceHash, ExternalReceiptID: result.ExternalID,
		ExternalReceiptHash: result.EvidenceHash, ExternalCommittedAt: result.ObservedAt,
		IdempotencyKey: "product-execution:" + string(request.ExecutionID) + ":effect-commit",
	}
	if _, err := store.lifecycle.CommitEffect(ctx, commit); err != nil {
		return View{}, err
	}
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseDeployed, nil, []string{string(lifecycleRequest.ID)},
	); err != nil {
		return View{}, err
	}
	view, err := store.Load(ctx, request.ExecutionID)
	if err != nil {
		return View{}, err
	}
	binding := bindingFor(view, StageTelemetry)
	order, err := store.currentStageOrder(ctx, start, binding)
	if err != nil {
		return View{}, err
	}
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
	if err := tx.QueryRow(ctx, `
		SELECT phase,version FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, request.ExecutionID).Scan(&phase, &version); err != nil {
		return View{}, err
	}
	if phase == PhaseTelemetryQueued || phase == PhaseLaunchReady || phase == PhaseLaunched {
		if err := tx.Commit(ctx); err != nil {
			return View{}, err
		}
		return store.Load(ctx, request.ExecutionID)
	}
	if phase != PhaseDeploymentPending && phase != PhaseDeploymentAmbiguous {
		return View{}, ErrInvalidPhase
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_product_execution_effects
		SET state='committed',external_id=$1,evidence_hash=$2,reconciled_at=$3,updated_at=$3
		WHERE tenant_id=$4 AND organization_id=$5 AND execution_id=$6 AND effect_id=$7
		  AND state IN ('prepared','ambiguous')
	`, result.ExternalID, result.EvidenceHash.Digest, result.ObservedAt,
		store.tenantID, store.organizationID, request.ExecutionID, lifecycleRequest.ID); err != nil {
		return View{}, err
	}
	if err := store.dispatchStageTx(ctx, tx, start, binding, order, now); err != nil {
		return View{}, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_product_executions
		SET phase='telemetry_queued',version=version+1,deployment_effect_id=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5 AND version=$6
	`, lifecycleRequest.ID, now, store.tenantID, store.organizationID,
		request.ExecutionID, version)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return View{}, err
	}
	stage := StageDeployment
	if err := store.appendEventTx(ctx, tx, request.ExecutionID, PhaseTelemetryQueued,
		"deployment_committed", &stage, string(lifecycleRequest.ID),
		request.IdempotencyKey+":committed", now); err != nil {
		return View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, err
	}
	return store.Load(ctx, request.ExecutionID)
}

func (store *Store) commitFailedEffect(
	ctx context.Context,
	request DeploymentRequest,
	lifecycleRequest companylifecycle.EffectRequest,
	result effect.Result,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	observedAt := result.ObservedAt
	if !validUTC(observedAt) {
		observedAt = now
	}
	hash := failureHash(lifecycleRequest.RequestHash, result.SafeErrorCode)
	externalID := result.ExternalID
	if externalID == "" {
		externalID = "gateway:" + request.Proposal.ID + ":failed"
	}
	start, err := store.loadStart(ctx, request.ExecutionID)
	if err != nil {
		return err
	}
	commit := companylifecycle.EffectCommit{
		SchemaVersion: companylifecycle.EffectSchemaVersion,
		EffectID:      lifecycleRequest.ID, OrganizationID: store.organizationID,
		InitiativeID: start.Request.InitiativeID, RequestHash: lifecycleRequest.RequestHash,
		OutcomeHash: hash, ExternalReceiptID: externalID, ExternalReceiptHash: hash,
		ExternalCommittedAt: observedAt,
		IdempotencyKey:      "product-execution:" + string(request.ExecutionID) + ":effect-failure",
	}
	if _, err := store.lifecycle.CommitEffect(ctx, commit); err != nil {
		return err
	}
	return store.markDeploymentState(
		ctx, request.ExecutionID, lifecycleRequest.ID, "failed", PhaseFailed,
		externalID, &hash, "deployment_failed", request.IdempotencyKey+":failed",
	)
}

func (store *Store) markDeploymentState(
	ctx context.Context,
	id ExecutionID,
	effectID companylifecycle.EffectID,
	effectState string,
	phase Phase,
	externalID string,
	evidenceHash *contracts.ContentHash,
	eventKind, eventKey string,
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
	var current Phase
	if err := tx.QueryRow(ctx, `
		SELECT phase FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, id).Scan(&current); err != nil {
		return err
	}
	var reconciledAt any
	var digest any
	if evidenceHash != nil {
		reconciledAt = now
		digest = evidenceHash.Digest
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_product_execution_effects
		SET state=$1,external_id=NULLIF($2,''),evidence_hash=$3,reconciled_at=$4,updated_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND execution_id=$8 AND effect_id=$9
	`, effectState, externalID, digest, reconciledAt, now,
		store.tenantID, store.organizationID, id, effectID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_product_executions
		SET phase=$1,version=version+1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5 AND phase<>$1
	`, phase, now, store.tenantID, store.organizationID, id); err != nil {
		return err
	}
	stage := StageDeployment
	if err := store.appendEventTx(ctx, tx, id, phase, eventKind, &stage,
		string(effectID), eventKey, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func failureHash(requestHash contracts.ContentHash, code string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(requestHash.Digest + "|failed|" + code))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}
