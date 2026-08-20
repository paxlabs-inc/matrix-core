package controlapi

import (
	"context"
	"fmt"

	"centra/workforce/internal/commercialexecution"
)

type CommercialExecutionResult struct {
	Execution commercialexecution.Snapshot `json:"execution"`
	Changed   bool                         `json:"changed"`
}

func (service *Service) authorizedCommercialExecution(
	ctx context.Context,
	principal Principal,
) (*commercialexecution.Store, *commercialexecution.Coordinator, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, nil, err
	}
	service.operatingStoresMu.RLock()
	store := service.commercialExecutionStore
	coordinator := service.commercialCoordinator
	service.operatingStoresMu.RUnlock()
	if store == nil || coordinator == nil {
		return nil, nil, fmt.Errorf("controlapi: commercial execution runtime is unavailable")
	}
	return store, coordinator, nil
}

func (service *Service) StartCommercialExecution(
	ctx context.Context,
	principal Principal,
	value commercialexecution.Plan,
) (CommercialExecutionResult, error) {
	store, _, err := service.authorizedCommercialExecution(ctx, principal)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return CommercialExecutionResult{}, ErrUnauthorized
	}
	snapshot, changed, err := store.Start(ctx, value)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	result := CommercialExecutionResult{Execution: snapshot, Changed: changed}
	return result, service.publishCommercialExecution(ctx, principal, result, "commercial.execution.started")
}

func (service *Service) RecordCommercialEvidence(
	ctx context.Context,
	principal Principal,
	value commercialexecution.Evidence,
) (CommercialExecutionResult, error) {
	store, _, err := service.authorizedCommercialExecution(ctx, principal)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return CommercialExecutionResult{}, ErrUnauthorized
	}
	snapshot, changed, err := store.RecordEvidence(ctx, value)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	result := CommercialExecutionResult{Execution: snapshot, Changed: changed}
	return result, service.publishCommercialExecution(ctx, principal, result, "commercial.execution.evidence.recorded")
}

func (service *Service) CorrectCommercialExecution(
	ctx context.Context,
	principal Principal,
	value commercialexecution.Correction,
) (CommercialExecutionResult, error) {
	store, _, err := service.authorizedCommercialExecution(ctx, principal)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return CommercialExecutionResult{}, ErrUnauthorized
	}
	snapshot, changed, err := store.ApplyCorrection(ctx, value)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	result := CommercialExecutionResult{Execution: snapshot, Changed: changed}
	return result, service.publishCommercialExecution(ctx, principal, result, "commercial.execution.corrected")
}

func (service *Service) RecoverCommercialExecution(
	ctx context.Context,
	principal Principal,
	value commercialexecution.Recovery,
) (CommercialExecutionResult, error) {
	store, _, err := service.authorizedCommercialExecution(ctx, principal)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return CommercialExecutionResult{}, ErrUnauthorized
	}
	snapshot, changed, err := store.Recover(ctx, value)
	if err != nil {
		return CommercialExecutionResult{}, err
	}
	result := CommercialExecutionResult{Execution: snapshot, Changed: changed}
	return result, service.publishCommercialExecution(ctx, principal, result, "commercial.execution.recovered")
}

func (service *Service) CommercialMeasurement(
	ctx context.Context,
	principal Principal,
	executionID commercialexecution.ExecutionID,
) (commercialexecution.MeasurementProof, error) {
	_, coordinator, err := service.authorizedCommercialExecution(ctx, principal)
	if err != nil {
		return commercialexecution.MeasurementProof{}, err
	}
	return coordinator.EvaluateMeasurement(ctx, executionID)
}

func (service *Service) CommercialExecution(
	ctx context.Context,
	principal Principal,
	executionID commercialexecution.ExecutionID,
) (commercialexecution.Snapshot, error) {
	store, _, err := service.authorizedCommercialExecution(ctx, principal)
	if err != nil {
		return commercialexecution.Snapshot{}, err
	}
	return store.Load(ctx, executionID)
}

func (service *Service) CommercialIncidents(
	ctx context.Context,
	principal Principal,
) ([]commercialexecution.IncidentView, error) {
	store, _, err := service.authorizedCommercialExecution(ctx, principal)
	if err != nil {
		return nil, err
	}
	return store.ListOpenIncidents(ctx)
}

func (service *Service) publishCommercialExecution(
	ctx context.Context,
	principal Principal,
	result CommercialExecutionResult,
	kind string,
) error {
	snapshot := result.Execution
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID: fmt.Sprintf(
			"event:%s:%s:%d", kind, snapshot.Plan.Body.ID, snapshot.Version,
		),
		OrganizationID:  principal.OrganizationID,
		Type:            kind,
		ResourceKind:    "commercial-execution",
		ResourceID:      string(snapshot.Plan.Body.ID),
		ResourceVersion: snapshot.Version,
		VerifiedCompletion: snapshot.State == commercialexecution.StateCompleted &&
			snapshot.CurrentPhase == commercialexecution.PhaseMeasurement,
		Fields: map[string]any{
			"state":         snapshot.State,
			"phase":         snapshot.CurrentPhase,
			"initiative_id": snapshot.Plan.Body.InitiativeID,
			"work_order_id": snapshot.Plan.Body.WorkOrderID,
			"changed":       result.Changed,
		},
	})
	return err
}
