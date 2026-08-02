package productexecution

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/productcapability"
	"matrix/workforce/internal/squad"
	"matrix/workforce/internal/workorder"
)

func (store *Store) CompleteProduct(ctx context.Context, request ReceiptRequest) (View, error) {
	return store.completeReceiptStage(
		ctx, request, StageProduct, PhaseProductQueued, PhaseDesignQueued,
		StageDesign, productcapability.PhasePlanned,
	)
}

func (store *Store) CompleteBuild(ctx context.Context, request CompleteStageRequest) (View, error) {
	if err := request.ReceiptRequest.Validate(); err != nil {
		return View{}, err
	}
	start, view, receipt, err := store.prepareReceiptWithID(
		ctx, request.ExecutionID, StageBuild, request.ReceiptID,
	)
	if err != nil {
		return View{}, err
	}
	if view.Phase != PhaseBuildQueued {
		return store.receiptReplay(view, receipt)
	}
	if err := store.validateReceiptEvidence(ctx, receipt, request.GateEvidence,
		companylifecycle.RequiredEvidenceFor(
			companylifecycle.StateDesign, companylifecycle.StateBuild,
			companylifecycle.DecisionAdvance, "",
		)); err != nil {
		return View{}, err
	}
	buildBinding := bindingFor(view, StageBuild)
	transitionID, correction, err := store.advanceLifecycle(
		ctx, start, buildBinding, companylifecycle.StateDesign,
		companylifecycle.StateBuild, request.GateEvidence, nil,
	)
	if err != nil {
		return View{}, err
	}
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseImplemented, nil, nil,
	); err != nil {
		return View{}, err
	}
	return store.commitReceiptProgress(ctx, progressCommit{
		ExecutionID: request.ExecutionID, Expected: PhaseBuildQueued,
		Next: PhaseVerifyQueued, Receipt: receipt, NextStage: StageVerification,
		EventKey: request.IdempotencyKey, TransitionID: transitionID,
		Correction: &correction,
	})
}

func (store *Store) CompleteVerification(ctx context.Context, request CompleteStageRequest) (View, error) {
	if err := request.ReceiptRequest.Validate(); err != nil {
		return View{}, err
	}
	start, view, receipt, err := store.prepareReceiptWithID(
		ctx, request.ExecutionID, StageVerification, request.ReceiptID,
	)
	if err != nil {
		return View{}, err
	}
	if view.Phase != PhaseVerifyQueued {
		return store.receiptReplay(view, receipt)
	}
	if err := store.validateReceiptEvidence(ctx, receipt, request.GateEvidence,
		companylifecycle.RequiredEvidenceFor(
			companylifecycle.StateBuild, companylifecycle.StateVerify,
			companylifecycle.DecisionAdvance, "",
		)); err != nil {
		return View{}, err
	}
	// The resulting VERIFY checkpoint is deliberately held by the exact
	// deployment seat, because lifecycle effect preparation requires that same
	// fenced seat and lease. Independent verification remains bound in evidence.
	deploymentBinding := bindingFor(view, StageDeployment)
	transitionID, correction, err := store.advanceLifecycle(
		ctx, start, deploymentBinding, companylifecycle.StateBuild,
		companylifecycle.StateVerify, request.GateEvidence, nil,
	)
	if err != nil {
		return View{}, err
	}
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseVerified, nil, nil,
	); err != nil {
		return View{}, err
	}
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseReleaseReady, nil, nil,
	); err != nil {
		return View{}, err
	}
	return store.commitReceiptProgress(ctx, progressCommit{
		ExecutionID: request.ExecutionID, Expected: PhaseVerifyQueued,
		Next: PhaseDeploymentQueued, Receipt: receipt, NextStage: StageDeployment,
		EventKey: request.IdempotencyKey, TransitionID: transitionID,
		Correction: &correction,
	})
}

func (store *Store) CompleteDeploymentPreparation(
	ctx context.Context,
	request ReceiptRequest,
) (View, error) {
	if err := request.Validate(); err != nil {
		return View{}, err
	}
	start, view, receipt, err := store.prepareReceiptWithID(
		ctx, request.ExecutionID, StageDeployment, request.ReceiptID,
	)
	if err != nil {
		return View{}, err
	}
	if view.Phase != PhaseDeploymentQueued {
		return store.receiptReplay(view, receipt)
	}
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseDeploymentPending, nil, nil,
	); err != nil {
		return View{}, err
	}
	return store.commitReceiptProgress(ctx, progressCommit{
		ExecutionID: request.ExecutionID, Expected: PhaseDeploymentQueued,
		Next: PhaseDeploymentPending, Receipt: receipt,
		EventKey: request.IdempotencyKey,
	})
}

func (store *Store) CompleteDesign(ctx context.Context, request CompleteDesignRequest) (View, error) {
	if err := request.ReceiptRequest.Validate(); err != nil {
		return View{}, err
	}
	start, view, receipt, err := store.prepareReceiptWithID(
		ctx, request.ExecutionID, StageDesign, request.ReceiptID,
	)
	if err != nil {
		return View{}, err
	}
	if view.Phase != PhaseDesignQueued {
		return store.receiptReplay(view, receipt)
	}
	now, err := store.currentTime()
	if err != nil || request.Record.ValidateAt(now) != nil {
		return View{}, fmt.Errorf("product execution: Product and Design record is not current")
	}
	body := request.Record.Record.Body
	designBinding := bindingFor(view, StageDesign)
	verificationBinding := bindingFor(view, StageVerification)
	buildBinding := bindingFor(view, StageBuild)
	if body.Kind != productcapability.RecordProductDesignHandoff || body.Handoff == nil ||
		body.OrganizationID != store.organizationID ||
		string(body.InitiativeID) != string(start.Request.InitiativeID) ||
		body.ProjectID != start.Request.ProjectID || body.WorkspaceID != start.Request.WorkspaceID ||
		body.AuthorSeatID != designBinding.SeatID ||
		request.Record.Verification.VerifierSeatID != verificationBinding.SeatID ||
		body.Handoff.ID != start.Request.HandoffID ||
		body.Handoff.DeveloperIntentID != buildBinding.IntentID {
		return View{}, ErrUnauthorized
	}
	if err := store.validateRecordEvidence(request.Record, request.GateEvidence,
		companylifecycle.RequiredEvidenceFor(
			companylifecycle.StateFund, companylifecycle.StateDesign,
			companylifecycle.DecisionAdvance, "",
		)); err != nil {
		return View{}, err
	}
	if _, err := store.products.Commit(ctx, request.Record); err != nil {
		return View{}, fmt.Errorf("product execution: commit verified Product and Design handoff: %w", err)
	}
	transitionID, correction, err := store.advanceLifecycle(
		ctx, start, designBinding, companylifecycle.StateFund,
		companylifecycle.StateDesign, request.GateEvidence, nil,
	)
	if err != nil {
		return View{}, err
	}
	recordID := body.ID
	if _, err := store.advanceProductCheckpoint(
		ctx, start, productcapability.PhaseImplementing,
		[]productcapability.RecordID{recordID}, nil,
	); err != nil {
		return View{}, err
	}
	return store.commitReceiptProgress(ctx, progressCommit{
		ExecutionID: request.ExecutionID, Expected: PhaseDesignQueued,
		Next: PhaseBuildQueued, Receipt: receipt, NextStage: StageBuild,
		ProductRecordID: &recordID, EventKey: request.IdempotencyKey,
		TransitionID: transitionID, Correction: &correction,
	})
}

func (store *Store) completeReceiptStage(
	ctx context.Context,
	request ReceiptRequest,
	stage Stage,
	expected, next Phase,
	nextStage Stage,
	checkpointPhase productcapability.ExecutionPhase,
) (View, error) {
	if err := request.Validate(); err != nil {
		return View{}, err
	}
	start, view, receipt, err := store.prepareReceiptWithID(
		ctx, request.ExecutionID, stage, request.ReceiptID,
	)
	if err != nil {
		return View{}, err
	}
	if view.Phase != expected {
		return store.receiptReplay(view, receipt)
	}
	if _, err := store.advanceProductCheckpoint(ctx, start, checkpointPhase, nil, nil); err != nil {
		return View{}, err
	}
	return store.commitReceiptProgress(ctx, progressCommit{
		ExecutionID: request.ExecutionID, Expected: expected, Next: next,
		Receipt: receipt, NextStage: nextStage, EventKey: request.IdempotencyKey,
	})
}

func (store *Store) prepareReceipt(
	ctx context.Context,
	id ExecutionID,
	stage Stage,
) (StartRecord, View, StageReceipt, error) {
	start, err := store.loadStart(ctx, id)
	if err != nil {
		return StartRecord{}, View{}, StageReceipt{}, err
	}
	view, err := store.Load(ctx, id)
	if err != nil {
		return StartRecord{}, View{}, StageReceipt{}, err
	}
	binding := bindingFor(view, stage)
	if !binding.Stage.Valid() {
		return StartRecord{}, View{}, StageReceipt{}, ErrIntegrity
	}
	return start, view, StageReceipt{Stage: stage}, nil
}

func (store *Store) validateStageReceipt(
	ctx context.Context,
	view View,
	stage Stage,
	id contracts.ReceiptID,
) (StageReceipt, error) {
	binding := bindingFor(view, stage)
	receipt, err := store.receipts.OpenReceipt(ctx, store.organizationID, id)
	if err != nil {
		return StageReceipt{}, err
	}
	if receipt.Disposition != contracts.DispositionGoalCompleted || receipt.VerdictID == nil ||
		receipt.WakeID != binding.WakeID || receipt.ParentIntentID != binding.IntentID ||
		receipt.SeatID != binding.SeatID || receipt.DepartmentID != binding.DepartmentID ||
		receipt.MandateID != binding.MandateID || receipt.MandateVersion != binding.MandateVersion {
		return StageReceipt{}, ErrUnauthorized
	}
	var intentID, executingSeat, auditorSeat, outcome string
	var verdictHash string
	err = store.pool.QueryRow(ctx, `
		SELECT intent_id,executing_seat_id,auditor_seat_id,outcome,verdict_hash
		FROM workforce_verdict_records
		WHERE tenant_id=$1 AND organization_id=$2 AND verdict_id=$3
	`, store.tenantID, store.organizationID, *receipt.VerdictID).Scan(
		&intentID, &executingSeat, &auditorSeat, &outcome, &verdictHash,
	)
	if err != nil || intentID != string(binding.IntentID) || executingSeat != string(binding.SeatID) ||
		auditorSeat == executingSeat || outcome != string(contracts.VerdictPass) || verdictHash == "" {
		return StageReceipt{}, ErrUnauthorized
	}
	return StageReceipt{
		Stage: stage, ReceiptID: receipt.ID, ReceiptHash: receipt.ContentHash,
		VerdictID: *receipt.VerdictID, SeatID: receipt.SeatID,
		IntentID: receipt.ParentIntentID, AcceptedAt: receipt.CreatedAt,
	}, nil
}

func (store *Store) prepareReceiptWithID(
	ctx context.Context,
	id ExecutionID,
	stage Stage,
	receiptID contracts.ReceiptID,
) (StartRecord, View, StageReceipt, error) {
	start, view, _, err := store.prepareReceipt(ctx, id, stage)
	if err != nil {
		return StartRecord{}, View{}, StageReceipt{}, err
	}
	receipt, err := store.validateStageReceipt(ctx, view, stage, receiptID)
	if err != nil {
		return StartRecord{}, View{}, StageReceipt{}, err
	}
	for _, existing := range view.Receipts {
		if existing.Stage == receipt.Stage && existing.ReceiptID == receipt.ReceiptID &&
			existing.ReceiptHash == receipt.ReceiptHash {
			return start, view, receipt, nil
		}
	}
	now, err := store.currentTime()
	if err != nil {
		return StartRecord{}, View{}, StageReceipt{}, err
	}
	state, err := store.squads.AssignmentState(ctx, start.Assignment.ID, now)
	if err != nil || state != squad.AssignmentActive {
		return StartRecord{}, View{}, StageReceipt{}, ErrUnauthorized
	}
	return start, view, receipt, nil
}

type progressCommit struct {
	ExecutionID         ExecutionID
	Expected            Phase
	Next                Phase
	Receipt             StageReceipt
	NextStage           Stage
	ProductRecordID     *productcapability.RecordID
	EngineeringRecordID *productcapability.RecordID
	DeploymentEffectID  *companylifecycle.EffectID
	LaunchTransitionID  companylifecycle.TransitionID
	EventKey            string
	TransitionID        companylifecycle.TransitionID
	Correction          *companylifecycle.CorrectionBinding
}

func (store *Store) commitReceiptProgress(ctx context.Context, value progressCommit) (View, error) {
	start, err := store.loadStart(ctx, value.ExecutionID)
	if err != nil {
		return View{}, err
	}
	view, err := store.Load(ctx, value.ExecutionID)
	if err != nil {
		return View{}, err
	}
	var nextBinding StageBinding
	var nextOrder workorder.CompanyOrder
	if value.NextStage.Valid() {
		nextBinding = bindingFor(view, value.NextStage)
		nextOrder, err = store.currentStageOrder(ctx, start, nextBinding)
		if err != nil {
			return View{}, err
		}
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
	`, store.tenantID, store.organizationID, value.ExecutionID).Scan(&phase, &version); err != nil {
		return View{}, err
	}
	if phase != value.Expected {
		if phase == value.Next {
			if err := tx.Commit(ctx); err != nil {
				return View{}, err
			}
			return store.Load(ctx, value.ExecutionID)
		}
		return View{}, ErrInvalidPhase
	}
	if err := store.insertStageReceiptTx(ctx, tx, value.ExecutionID, value.Receipt); err != nil {
		return View{}, err
	}
	if value.NextStage.Valid() {
		if err := store.dispatchStageTx(ctx, tx, start, nextBinding, nextOrder, now); err != nil {
			return View{}, err
		}
	}
	if value.Correction != nil && value.TransitionID != "" {
		if err := store.insertCorrectionTx(ctx, tx, value.ExecutionID, value.TransitionID, *value.Correction); err != nil {
			return View{}, err
		}
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_product_executions
		SET phase=$1,version=version+1,
		    product_record_id=COALESCE($2,product_record_id),
		    engineering_record_id=COALESCE($3,engineering_record_id),
		    deployment_effect_id=COALESCE($4,deployment_effect_id),
		    launch_transition_id=COALESCE(NULLIF($5,''),launch_transition_id),
		    updated_at=$6
		WHERE tenant_id=$7 AND organization_id=$8 AND execution_id=$9 AND version=$10
	`, value.Next, nullableString(value.ProductRecordID), nullableString(value.EngineeringRecordID),
		nullableString(value.DeploymentEffectID), value.LaunchTransitionID, now,
		store.tenantID, store.organizationID, value.ExecutionID, version)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return View{}, err
	}
	eventStage := value.Receipt.Stage
	sourceID := string(value.Receipt.ReceiptID)
	if err := store.appendEventTx(ctx, tx, value.ExecutionID, value.Next,
		"stage_completed", &eventStage, sourceID, value.EventKey, now); err != nil {
		return View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, err
	}
	return store.Load(ctx, value.ExecutionID)
}

func (store *Store) insertStageReceiptTx(
	ctx context.Context,
	tx pgx.Tx,
	id ExecutionID,
	receipt StageReceipt,
) error {
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_product_execution_stage_receipts (
			tenant_id,organization_id,execution_id,stage,receipt_id,receipt_hash,
			verdict_id,seat_id,intent_id,accepted_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id,execution_id,stage) DO NOTHING
	`, store.tenantID, store.organizationID, id, receipt.Stage, receipt.ReceiptID,
		receipt.ReceiptHash.Digest, receipt.VerdictID, receipt.SeatID,
		receipt.IntentID, receipt.AcceptedAt)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var receiptID, hash, verdictID string
		if err := tx.QueryRow(ctx, `
			SELECT receipt_id,receipt_hash,verdict_id
			FROM workforce_product_execution_stage_receipts
			WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3 AND stage=$4
		`, store.tenantID, store.organizationID, id, receipt.Stage).Scan(
			&receiptID, &hash, &verdictID,
		); err != nil || receiptID != string(receipt.ReceiptID) ||
			hash != receipt.ReceiptHash.Digest || verdictID != string(receipt.VerdictID) {
			return ErrConflict
		}
	}
	return nil
}

func (store *Store) receiptReplay(view View, receipt StageReceipt) (View, error) {
	for _, existing := range view.Receipts {
		if existing.Stage == receipt.Stage && existing.ReceiptID == receipt.ReceiptID &&
			existing.ReceiptHash == receipt.ReceiptHash {
			return view, nil
		}
	}
	return View{}, ErrInvalidPhase
}

func bindingFor(view View, stage Stage) StageBinding {
	for _, binding := range view.Stages {
		if binding.Stage == stage {
			return binding
		}
	}
	return StageBinding{}
}

func (store *Store) currentStageOrder(
	ctx context.Context,
	start StartRecord,
	binding StageBinding,
) (workorder.CompanyOrder, error) {
	now, err := store.currentTime()
	if err != nil {
		return workorder.CompanyOrder{}, err
	}
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil || !current.Executable(now) {
		return workorder.CompanyOrder{}, ErrUnauthorized
	}
	authority := start.Request.CompanyAuthority
	authority.Policy = current.Authority.IssuerPolicy
	authority.CurrentMissionVersion = current.Authority.Mission.Version
	authority.CurrentConstitutionVersion = current.Authority.Constitution.Version
	authority.CurrentCapitalEnvelopeVersion = current.Authority.Capital.Version
	authority.At = now
	plan, err := store.plans.LoadCurrent(ctx, string(start.Request.InitiativeID), authority)
	if err != nil || plan.State != "active" || plan.Plan.ID != start.PlanID ||
		plan.Plan.Version != start.PlanVersion || plan.Plan.Hash != start.PlanHash {
		return workorder.CompanyOrder{}, ErrUnauthorized
	}
	for _, node := range plan.Plan.Nodes {
		if node.Template.ID == binding.PlanNodeID && node.Order != nil &&
			node.Order.ID == binding.WorkOrderID {
			return *node.Order, nil
		}
	}
	return workorder.CompanyOrder{}, ErrIntegrity
}

func (store *Store) advanceProductCheckpoint(
	ctx context.Context,
	start StartRecord,
	target productcapability.ExecutionPhase,
	recordIDs []productcapability.RecordID,
	effectIDs []string,
) (productcapability.Checkpoint, error) {
	checkpoint, err := store.products.LoadCheckpoint(
		ctx, productcapability.InitiativeID(start.Request.InitiativeID),
	)
	if err != nil {
		return productcapability.Checkpoint{}, err
	}
	if checkpoint.Phase == target {
		return checkpoint, nil
	}
	if executionPhaseRank(checkpoint.Phase) > executionPhaseRank(target) {
		return checkpoint, nil
	}
	next := checkpoint
	next.Version++
	next.Phase = target
	next.CompletedRecordIDs = uniqueRecordIDs(next.CompletedRecordIDs, recordIDs)
	next.CommittedEffectIDs = uniqueStrings(next.CommittedEffectIDs, effectIDs)
	next.ReconciledEffectIDs = uniqueStrings(next.ReconciledEffectIDs, effectIDs)
	next.IdempotencyKey = fmt.Sprintf("product-execution:%s:checkpoint:%s", start.Request.ID, target)
	now, err := store.currentTime()
	if err != nil {
		return productcapability.Checkpoint{}, err
	}
	next.UpdatedAt = now
	current, _, err := store.products.AdvanceCheckpoint(ctx, checkpoint.Version, next)
	if err != nil {
		return productcapability.Checkpoint{}, err
	}
	_, _ = store.pool.Exec(ctx, `
		UPDATE workforce_product_executions SET checkpoint_version=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND execution_id=$5
		  AND checkpoint_version<$1
	`, current.Version, now, store.tenantID, store.organizationID, start.Request.ID)
	return current, nil
}

func executionPhaseRank(value productcapability.ExecutionPhase) int {
	switch value {
	case productcapability.PhaseIntake:
		return 1
	case productcapability.PhasePlanned:
		return 2
	case productcapability.PhaseImplementing:
		return 3
	case productcapability.PhaseImplemented:
		return 4
	case productcapability.PhaseVerified:
		return 5
	case productcapability.PhaseReleaseReady:
		return 6
	case productcapability.PhaseDeploymentPending:
		return 7
	case productcapability.PhaseDeployed:
		return 8
	case productcapability.PhaseObserved:
		return 9
	case productcapability.PhaseClosed:
		return 10
	default:
		return 0
	}
}

func uniqueRecordIDs(current, added []productcapability.RecordID) []productcapability.RecordID {
	seen := make(map[productcapability.RecordID]bool, len(current)+len(added))
	result := make([]productcapability.RecordID, 0, len(current)+len(added))
	for _, values := range [][]productcapability.RecordID{current, added} {
		for _, value := range values {
			if !seen[value] {
				seen[value] = true
				result = append(result, value)
			}
		}
	}
	slices.Sort(result)
	return result
}

func uniqueStrings(current, added []string) []string {
	result := append(append([]string(nil), current...), added...)
	slices.Sort(result)
	return slices.Compact(result)
}

func (store *Store) advanceLifecycle(
	ctx context.Context,
	start StartRecord,
	binding StageBinding,
	from, to companylifecycle.State,
	evidence []companylifecycle.EvidenceBinding,
	effectIDs []companylifecycle.EffectID,
) (companylifecycle.TransitionID, companylifecycle.CorrectionBinding, error) {
	transitionID := companylifecycle.TransitionID(
		stableStageID("transition-"+strings.ToLower(string(to)), start.Request.ID, binding.Stage),
	)
	checkpoint, err := store.lifecycle.Load(ctx, store.organizationID, start.Request.InitiativeID)
	if err != nil {
		return "", companylifecycle.CorrectionBinding{}, err
	}
	if checkpoint.State == to && checkpoint.LastTransitionID == transitionID {
		correction, err := store.runtime.SnapshotCorrections(ctx)
		if err == nil && (correction.UnresolvedMaterialCount != 0 ||
			correction.UnresolvedContaminatedCount != 0) {
			blockedID := companylifecycle.TransitionID(fmt.Sprintf(
				"%s:blocked:%d", transitionID, correction.Version,
			))
			_ = store.persistCorrection(ctx, start.Request.ID, blockedID, correction)
			return "", correction, ErrCorrectionBlocked
		}
		return transitionID, correction, err
	}
	if checkpoint.State != from {
		return "", companylifecycle.CorrectionBinding{}, ErrInvalidPhase
	}
	correction, err := store.runtime.SnapshotCorrections(ctx)
	if err != nil {
		return "", companylifecycle.CorrectionBinding{}, err
	}
	if correction.UnresolvedMaterialCount != 0 || correction.UnresolvedContaminatedCount != 0 {
		blockedID := companylifecycle.TransitionID(fmt.Sprintf(
			"%s:blocked:%d", transitionID, correction.Version,
		))
		_ = store.persistCorrection(ctx, start.Request.ID, blockedID, correction)
		return "", correction, ErrCorrectionBlocked
	}
	companyState, err := store.runtime.BindCompanyState(ctx, string(checkpoint.CompanyState.ID))
	if err != nil {
		return "", correction, err
	}
	authority, err := store.authorityForBinding(ctx, start, binding, checkpoint.Authority.ClauseID)
	if err != nil {
		return "", correction, err
	}
	evidence = append([]companylifecycle.EvidenceBinding(nil), evidence...)
	sort.Slice(evidence, func(left, right int) bool {
		if evidence[left].Kind == evidence[right].Kind {
			return evidence[left].ID < evidence[right].ID
		}
		return evidence[left].Kind < evidence[right].Kind
	})
	slices.Sort(effectIDs)
	request := companylifecycle.TransitionRequest{
		SchemaVersion: companylifecycle.TransitionSchemaVersion,
		TransitionID:  transitionID,
		ReceiptID: companylifecycle.DecisionReceiptID(
			stableStageID("lifecycle-receipt-"+strings.ToLower(string(to)), start.Request.ID, binding.Stage),
		),
		OrganizationID: store.organizationID, InitiativeID: start.Request.InitiativeID,
		ExpectedVersion: checkpoint.Version, FromState: from, ToState: to,
		Decision: companylifecycle.DecisionAdvance, Authority: authority,
		CompanyState: companyState, Correction: correction, Evidence: evidence,
		CapitalImpact: companylifecycle.CapitalImpact{
			SchemaVersion: contracts.SchemaVersionV1, Currency: checkpoint.Capital.Currency,
			CapitalEnvelopeVersion:     checkpoint.Capital.CapitalEnvelopeVersion,
			CapitalEnvelopeHash:        checkpoint.Capital.CapitalEnvelopeHash,
			TransitionBudgetMicrounits: 1,
		},
		EffectIDs:      effectIDs,
		IdempotencyKey: "product-execution:" + string(start.Request.ID) + ":lifecycle:" + strings.ToLower(string(to)),
	}
	requestHash, err := contracts.HashCanonical(request)
	if err != nil {
		return "", correction, err
	}
	if _, err := store.runtime.AuthorizeGate(
		ctx, transitionID, start.Request.InitiativeID, requestHash,
		start.Request.PortfolioDecision, authority.ClauseID,
	); err != nil {
		return "", correction, err
	}
	if _, err := store.lifecycle.Transition(ctx, request); err != nil {
		return "", correction, err
	}
	return transitionID, correction, nil
}

func (store *Store) authorityForBinding(
	ctx context.Context,
	start StartRecord,
	binding StageBinding,
	clauseID string,
) (companylifecycle.AuthorityBinding, error) {
	configuration, err := store.runtime.LoadCurrent(ctx)
	if err != nil {
		return companylifecycle.AuthorityBinding{}, err
	}
	var mandateID, mandateHash string
	var mandateVersion uint64
	var active bool
	if err := store.pool.QueryRow(ctx, `
		SELECT seat.mandate_id,seat.mandate_version,record.canonical_hash,seat.active
		FROM workforce_organization_seats seat
		JOIN workforce_authority_records record
		  ON record.tenant_id=seat.tenant_id AND record.organization_id=seat.organization_id
		 AND record.authority_kind='mandate' AND record.authority_id=seat.mandate_id
		 AND record.version=seat.mandate_version
		WHERE seat.tenant_id=$1 AND seat.organization_id=$2 AND seat.seat_id=$3
	`, store.tenantID, store.organizationID, binding.SeatID).Scan(
		&mandateID, &mandateVersion, &mandateHash, &active,
	); err != nil || !active || mandateID != string(binding.MandateID) ||
		mandateVersion != binding.MandateVersion || mandateHash != binding.MandateDigest.Digest {
		return companylifecycle.AuthorityBinding{}, ErrUnauthorized
	}
	expiresAt := configuration.ExpiresAt
	if start.Assignment.ExpiresAt.Before(expiresAt) {
		expiresAt = start.Assignment.ExpiresAt
	}
	if start.Request.PortfolioDecision.NextReviewAt.Before(expiresAt) {
		expiresAt = start.Request.PortfolioDecision.NextReviewAt
	}
	return companylifecycle.AuthorityBinding{
		SchemaVersion: contracts.SchemaVersionV1, OrganizationID: store.organizationID,
		MissionVersion: configuration.MissionVersion, MissionHash: configuration.MissionHash,
		ConstitutionVersion:    configuration.ConstitutionVersion,
		ConstitutionHash:       configuration.ConstitutionHash,
		CapitalEnvelopeVersion: configuration.CapitalEnvelopeVersion,
		CapitalEnvelopeHash:    configuration.CapitalEnvelopeHash,
		IssuerPolicyVersion:    configuration.IssuerPolicyVersion,
		IssuerPolicyHash:       configuration.IssuerPolicyHash,
		MandateID:              contracts.MandateID(mandateID), MandateVersion: mandateVersion,
		MandateHash:       contracts.ContentHash{Algorithm: "sha256", Digest: mandateHash},
		RequestedBySeatID: binding.SeatID, ClauseID: clauseID, ExpiresAt: expiresAt,
	}, nil
}

func (store *Store) persistCorrection(
	ctx context.Context,
	id ExecutionID,
	transitionID companylifecycle.TransitionID,
	correction companylifecycle.CorrectionBinding,
) error {
	_, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_product_execution_correction_bindings (
			tenant_id,organization_id,execution_id,transition_id,snapshot_id,
			snapshot_version,snapshot_hash,unresolved_material_count,
			unresolved_contaminated_count,checked_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id,execution_id,transition_id) DO NOTHING
	`, store.tenantID, store.organizationID, id, transitionID, correction.SnapshotID,
		correction.Version, correction.Hash.Digest, correction.UnresolvedMaterialCount,
		correction.UnresolvedContaminatedCount, correction.CheckedAt)
	return err
}

func (store *Store) insertCorrectionTx(
	ctx context.Context,
	tx pgx.Tx,
	id ExecutionID,
	transitionID companylifecycle.TransitionID,
	correction companylifecycle.CorrectionBinding,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_product_execution_correction_bindings (
			tenant_id,organization_id,execution_id,transition_id,snapshot_id,
			snapshot_version,snapshot_hash,unresolved_material_count,
			unresolved_contaminated_count,checked_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id,execution_id,transition_id) DO NOTHING
	`, store.tenantID, store.organizationID, id, transitionID, correction.SnapshotID,
		correction.Version, correction.Hash.Digest, correction.UnresolvedMaterialCount,
		correction.UnresolvedContaminatedCount, correction.CheckedAt)
	return err
}

func (store *Store) validateReceiptEvidence(
	ctx context.Context,
	receipt StageReceipt,
	evidence []companylifecycle.EvidenceBinding,
	required []companylifecycle.EvidenceKind,
) error {
	opened, err := store.receipts.OpenReceipt(ctx, store.organizationID, receipt.ReceiptID)
	if err != nil {
		return err
	}
	var verdictHash string
	if err := store.pool.QueryRow(ctx, `
		SELECT verdict_hash FROM workforce_verdict_records
		WHERE tenant_id=$1 AND organization_id=$2 AND verdict_id=$3
	`, store.tenantID, store.organizationID, receipt.VerdictID).Scan(&verdictHash); err != nil {
		return err
	}
	byKind := make(map[companylifecycle.EvidenceKind]contracts.EvidenceRef)
	for _, item := range opened.Evidence {
		kind := companylifecycle.EvidenceKind(item.Kind)
		if kind.Valid() {
			byKind[kind] = item
		}
	}
	if len(evidence) != len(required) {
		return fmt.Errorf("product execution: lifecycle evidence set is incomplete")
	}
	for _, requiredKind := range required {
		var binding *companylifecycle.EvidenceBinding
		for index := range evidence {
			if evidence[index].Kind == requiredKind {
				binding = &evidence[index]
				break
			}
		}
		reference, exists := byKind[requiredKind]
		if binding == nil || !exists || string(binding.ID) != string(reference.ID) ||
			binding.EvidenceHash != reference.Hash || binding.IndependentVerdictID == nil ||
			*binding.IndependentVerdictID != receipt.VerdictID ||
			binding.IndependentVerdictHash == nil ||
			binding.IndependentVerdictHash.Digest != verdictHash {
			return fmt.Errorf("product execution: %s evidence is not bound to the accepted receipt", requiredKind)
		}
	}
	return nil
}

func (store *Store) validateRecordEvidence(
	record productcapability.VerifiedRecord,
	evidence []companylifecycle.EvidenceBinding,
	required []companylifecycle.EvidenceKind,
) error {
	if len(evidence) != len(required) {
		return fmt.Errorf("product execution: lifecycle evidence set is incomplete")
	}
	verificationHash, err := contracts.HashCanonical(record.Verification)
	if err != nil {
		return err
	}
	artifacts := make(map[companylifecycle.EvidenceKind]productcapability.Artifact)
	var recordArtifacts []productcapability.Artifact
	switch {
	case record.Record.Body.Handoff != nil:
		recordArtifacts = record.Record.Body.Handoff.Artifacts
	case record.Record.Body.Engineering != nil:
		recordArtifacts = record.Record.Body.Engineering.Artifacts
	default:
		return fmt.Errorf("product execution: record carries no product execution artifacts")
	}
	for _, artifact := range recordArtifacts {
		if kind, ok := lifecycleKindForArtifact(artifact.Kind); ok {
			artifacts[kind] = artifact
		}
	}
	for _, requiredKind := range required {
		artifact, exists := artifacts[requiredKind]
		var binding *companylifecycle.EvidenceBinding
		for index := range evidence {
			if evidence[index].Kind == requiredKind {
				binding = &evidence[index]
				break
			}
		}
		if !exists || binding == nil || string(binding.ID) != artifact.ID ||
			binding.EvidenceHash != artifact.Artifact.Hash ||
			binding.IndependentVerdictID == nil ||
			*binding.IndependentVerdictID != record.Verification.ID ||
			binding.IndependentVerdictHash == nil ||
			*binding.IndependentVerdictHash != verificationHash {
			return fmt.Errorf("product execution: %s evidence is not bound to the verified record", requiredKind)
		}
	}
	return nil
}

func lifecycleKindForArtifact(kind productcapability.ArtifactKind) (companylifecycle.EvidenceKind, bool) {
	switch kind {
	case productcapability.ArtifactCustomerProblem:
		return companylifecycle.EvidenceCustomerProblem, true
	case productcapability.ArtifactRequirements:
		return companylifecycle.EvidenceRequirements, true
	case productcapability.ArtifactUserJourney:
		return companylifecycle.EvidenceUserJourney, true
	case productcapability.ArtifactImplementationPlan:
		return companylifecycle.EvidenceImplementationPlan, true
	case productcapability.ArtifactSourceState:
		return companylifecycle.EvidenceSourceState, true
	case productcapability.ArtifactDeploymentState:
		return companylifecycle.EvidenceDeploymentState, true
	case productcapability.ArtifactQualityEvidence:
		return companylifecycle.EvidenceQuality, true
	case productcapability.ArtifactSecurityEvidence:
		return companylifecycle.EvidenceSecurity, true
	case productcapability.ArtifactOperationsReadiness:
		return companylifecycle.EvidenceOperationsReadiness, true
	case productcapability.ArtifactClaimsEvidence:
		return companylifecycle.EvidenceClaims, true
	case productcapability.ArtifactLegalEvidence:
		return companylifecycle.EvidenceLegal, true
	case productcapability.ArtifactPricingEvidence:
		return companylifecycle.EvidencePricing, true
	case productcapability.ArtifactLaunchReadiness:
		return companylifecycle.EvidenceLaunchReadiness, true
	case productcapability.ArtifactIndependentReview:
		return companylifecycle.EvidenceIndependentReview, true
	default:
		return "", false
	}
}
