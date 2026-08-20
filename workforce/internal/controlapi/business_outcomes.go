package controlapi

import (
	"context"
	"fmt"

	"centra/workforce/internal/businessoutcome"
)

func (service *Service) authorizedBusinessOutcomeStore(
	ctx context.Context,
	principal Principal,
) (*businessoutcome.Store, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, err
	}
	service.operatingStoresMu.RLock()
	store := service.businessOutcomeStore
	service.operatingStoresMu.RUnlock()
	if store == nil {
		return nil, fmt.Errorf("controlapi: business outcome authority is unavailable")
	}
	return store, nil
}

func (service *Service) RegisterBusinessMetric(
	ctx context.Context,
	principal Principal,
	value businessoutcome.MetricDefinition,
) (businessoutcome.MetricDefinition, error) {
	store, err := service.authorizedBusinessOutcomeStore(ctx, principal)
	if err != nil {
		return businessoutcome.MetricDefinition{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return businessoutcome.MetricDefinition{}, ErrUnauthorized
	}
	if _, err := store.RegisterMetric(ctx, value); err != nil {
		return businessoutcome.MetricDefinition{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:business-metric:%s:%d", value.Body.ID, value.Body.Version),
		OrganizationID: principal.OrganizationID, Type: "business.metric.registered",
		ResourceKind: "business-metric", ResourceID: string(value.Body.ID),
		ResourceVersion: value.Body.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"initiative_id": value.Body.InitiativeID, "outcome_kind": value.Body.OutcomeKind,
			"effective_at": value.Body.EffectiveAt, "expires_at": value.Body.ExpiresAt,
			"state": "registered",
		},
	})
	return value, err
}

func (service *Service) CommitBusinessObservation(
	ctx context.Context,
	principal Principal,
	value businessoutcome.Observation,
) (businessoutcome.Observation, error) {
	store, err := service.authorizedBusinessOutcomeStore(ctx, principal)
	if err != nil {
		return businessoutcome.Observation{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return businessoutcome.Observation{}, ErrUnauthorized
	}
	if _, err := store.CommitObservation(ctx, value); err != nil {
		return businessoutcome.Observation{}, err
	}
	verified := value.Body.Status == businessoutcome.MeasurementObserved ||
		value.Body.Status == businessoutcome.MeasurementReconciled
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:business-observation:" + string(value.Body.ID),
		OrganizationID: principal.OrganizationID, Type: "business.observation.committed",
		ResourceKind: "business-observation", ResourceID: string(value.Body.ID),
		ResourceVersion: 1, VerifiedCompletion: verified,
		Fields: map[string]any{
			"initiative_id": value.Body.InitiativeID, "metric_id": value.Body.Metric.ID,
			"outcome_kind": value.Body.OutcomeKind, "measurement_state": value.Body.Status,
			"reconciliation_state": value.Body.Reconciliation.State,
			"observed_at":          value.Body.ObservedAt,
		},
	})
	return value, err
}

func (service *Service) CommitBusinessOutcome(
	ctx context.Context,
	principal Principal,
	value businessoutcome.VerifiedOutcome,
) (businessoutcome.VerifiedOutcome, error) {
	store, err := service.authorizedBusinessOutcomeStore(ctx, principal)
	if err != nil {
		return businessoutcome.VerifiedOutcome{}, err
	}
	if value.Record.Body.OrganizationID != principal.OrganizationID {
		return businessoutcome.VerifiedOutcome{}, ErrUnauthorized
	}
	if _, err := store.CommitOutcome(ctx, value); err != nil {
		return businessoutcome.VerifiedOutcome{}, err
	}
	body := value.Record.Body
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:business-outcome:" + string(body.ID),
		OrganizationID: principal.OrganizationID, Type: "business.outcome.committed",
		ResourceKind: "business-outcome", ResourceID: string(body.ID),
		ResourceVersion: body.Version, VerifiedCompletion: true,
		Fields: map[string]any{
			"initiative_id": body.InitiativeID, "metric_id": body.Metric.ID,
			"outcome_kind": body.Kind, "threshold_result": body.ThresholdResult,
			"fresh_until": body.FreshUntil, "state": "measured",
		},
	})
	return value, err
}

func (service *Service) EvaluateBusinessGate(
	ctx context.Context,
	principal Principal,
	value businessoutcome.GateRequirement,
) (businessoutcome.GateDecision, error) {
	store, err := service.authorizedBusinessOutcomeStore(ctx, principal)
	if err != nil {
		return businessoutcome.GateDecision{}, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return businessoutcome.GateDecision{}, ErrUnauthorized
	}
	decision, _, err := store.EvaluateGate(ctx, value)
	if err != nil {
		return businessoutcome.GateDecision{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:business-gate:" + string(value.ID) + ":" + decision.DecisionHash.Digest,
		OrganizationID: principal.OrganizationID, Type: "business.gate.evaluated",
		ResourceKind: "business-gate", ResourceID: string(value.ID),
		ResourceVersion: 1, VerifiedCompletion: decision.State == businessoutcome.GateSatisfied,
		Fields: map[string]any{
			"initiative_id": value.InitiativeID, "purpose": value.Purpose,
			"outcome_id": value.OutcomeID, "state": decision.State,
			"reasons": decision.Reasons,
		},
	})
	return decision, err
}

func (service *Service) CommitBusinessLineage(
	ctx context.Context,
	principal Principal,
	value businessoutcome.LineageEdge,
) (businessoutcome.LineageEdge, error) {
	store, err := service.authorizedBusinessOutcomeStore(ctx, principal)
	if err != nil {
		return businessoutcome.LineageEdge{}, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return businessoutcome.LineageEdge{}, ErrUnauthorized
	}
	if _, err := store.CommitLineageEdge(ctx, value); err != nil {
		return businessoutcome.LineageEdge{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:business-lineage:" + string(value.ID),
		OrganizationID: principal.OrganizationID, Type: "business.lineage.committed",
		ResourceKind: "business-lineage", ResourceID: string(value.ID),
		ResourceVersion: 1, VerifiedCompletion: true,
		Fields: map[string]any{
			"initiative_id": value.InitiativeID, "source_kind": value.Source.Kind,
			"consumer_kind": value.Consumer.Kind, "relation": value.Relation,
		},
	})
	return value, err
}

func (service *Service) ApplyBusinessCorrection(
	ctx context.Context,
	principal Principal,
	value businessoutcome.Correction,
) (businessoutcome.Correction, error) {
	store, err := service.authorizedBusinessOutcomeStore(ctx, principal)
	if err != nil {
		return businessoutcome.Correction{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return businessoutcome.Correction{}, ErrUnauthorized
	}
	if _, err := store.ApplyCorrection(ctx, value); err != nil {
		return businessoutcome.Correction{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:business-correction:" + string(value.Body.ID),
		OrganizationID: principal.OrganizationID, Type: "business.correction.applied",
		ResourceKind: "business-correction", ResourceID: string(value.Body.ID),
		ResourceVersion: 1, VerifiedCompletion: false,
		Fields: map[string]any{
			"initiative_id": value.Body.InitiativeID, "target_kind": value.Body.Target.Kind,
			"target_id": value.Body.Target.ID, "material": value.Body.Material,
			"state": "open",
		},
	})
	return value, err
}

func (service *Service) ResolveBusinessCorrection(
	ctx context.Context,
	principal Principal,
	value businessoutcome.CorrectionResolution,
) (businessoutcome.CorrectionResolution, error) {
	store, err := service.authorizedBusinessOutcomeStore(ctx, principal)
	if err != nil {
		return businessoutcome.CorrectionResolution{}, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return businessoutcome.CorrectionResolution{}, ErrUnauthorized
	}
	if _, err := store.ResolveCorrection(ctx, value); err != nil {
		return businessoutcome.CorrectionResolution{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:business-correction-resolved:" + value.Body.ID,
		OrganizationID: principal.OrganizationID, Type: "business.correction.resolved",
		ResourceKind: "business-correction", ResourceID: string(value.Body.CorrectionID),
		ResourceVersion: 1, VerifiedCompletion: true,
		Fields: map[string]any{
			"resolution_id": value.Body.ID, "replacement_kind": value.Body.Replacement.Kind,
			"replacement_id": value.Body.Replacement.ID, "state": "resolved",
		},
	})
	return value, err
}
