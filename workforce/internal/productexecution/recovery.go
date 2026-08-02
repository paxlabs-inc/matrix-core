package productexecution

import (
	"context"
	"fmt"
	"slices"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/productcapability"
)

// Recover authenticates every durable checkpoint after restart. It never
// redispatches an effect: PREPARED and ambiguous identities are returned for
// explicit provider reconciliation.
func (store *Store) Recover(ctx context.Context, request ResumeRequest) (Recovery, error) {
	if validateToken("execution id", string(request.ExecutionID)) != nil ||
		request.Source.Validate() != nil || request.Brain.Validate() != nil {
		return Recovery{}, fmt.Errorf("product execution: recovery request is invalid")
	}
	start, err := store.loadStart(ctx, request.ExecutionID)
	if err != nil {
		return Recovery{}, err
	}
	view, err := store.Load(ctx, request.ExecutionID)
	if err != nil {
		return Recovery{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return Recovery{}, err
	}
	checkpoint, err := store.products.LoadCheckpoint(
		ctx, productcapability.InitiativeID(start.Request.InitiativeID),
	)
	if err != nil {
		return Recovery{}, err
	}
	if err := productcapability.ValidateResume(checkpoint, request.Source, request.Brain, now); err != nil {
		return Recovery{}, err
	}
	lifecycle, err := store.lifecycle.Recover(ctx, store.organizationID, start.Request.InitiativeID)
	if err != nil {
		return Recovery{}, err
	}
	squadState, err := store.squads.AssignmentState(ctx, start.Assignment.ID, now)
	if err != nil {
		return Recovery{}, err
	}
	reconcile := make([]companylifecycle.EffectID, 0)
	for _, pending := range lifecycle.Effects {
		if pending.Status == companylifecycle.EffectPrepared {
			reconcile = append(reconcile, pending.Request.ID)
		}
	}
	for _, pending := range view.Effects {
		if pending.State == "ambiguous" || pending.State == "prepared" {
			reconcile = append(reconcile, pending.EffectID)
		}
	}
	slices.Sort(reconcile)
	reconcile = slices.Compact(reconcile)
	return Recovery{
		Execution: view, ProductCheckpoint: checkpoint, Lifecycle: lifecycle,
		SquadState: squadState, RequiresReconcile: reconcile,
	}, nil
}

// LoadByIntent is the stable WorkPacket integration seam. The immutable row
// prevents current product stage ownership from being inferred from mutable
// wake or graph state.
func (store *Store) LoadByIntent(
	ctx context.Context,
	intentID contracts.IntentID,
) (View, StageBinding, error) {
	if validateToken("intent id", string(intentID)) != nil {
		return View{}, StageBinding{}, ErrConflict
	}
	var executionID ExecutionID
	var stage Stage
	if err := store.pool.QueryRow(ctx, `
		SELECT execution_id,stage
		FROM workforce_product_execution_stage_bindings
		WHERE tenant_id=$1 AND organization_id=$2 AND intent_id=$3
	`, store.tenantID, store.organizationID, intentID).Scan(&executionID, &stage); err != nil {
		return View{}, StageBinding{}, err
	}
	view, err := store.Load(ctx, executionID)
	if err != nil {
		return View{}, StageBinding{}, err
	}
	binding := bindingFor(view, stage)
	if !binding.Stage.Valid() {
		return View{}, StageBinding{}, ErrIntegrity
	}
	return view, binding, nil
}
