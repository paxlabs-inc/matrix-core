package controlapi

import (
	"context"
	"fmt"

	"matrix/workforce/internal/autonomouscompany"
)

type AutonomousPropertyResult struct {
	Property autonomouscompany.PropertySnapshot `json:"property"`
	Replayed bool                               `json:"replayed"`
}

type AutonomousNextCycleResult struct {
	Event    autonomouscompany.NextCycleEvent `json:"event"`
	Replayed bool                             `json:"replayed"`
}

func (service *Service) authorizedAutonomousCompany(
	ctx context.Context,
	principal Principal,
) (*autonomouscompany.Store, *autonomouscompany.Coordinator, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, nil, err
	}
	service.operatingStoresMu.RLock()
	store := service.autonomousCompanyStore
	coordinator := service.autonomousCoordinator
	service.operatingStoresMu.RUnlock()
	if store == nil || coordinator == nil {
		return nil, nil, fmt.Errorf("controlapi: autonomous company runtime is unavailable")
	}
	return store, coordinator, nil
}

func (service *Service) CommitAutonomousProperty(
	ctx context.Context,
	principal Principal,
	draft autonomouscompany.PropertyDraft,
) (AutonomousPropertyResult, error) {
	store, _, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return AutonomousPropertyResult{}, err
	}
	if draft.OrganizationID != principal.OrganizationID {
		return AutonomousPropertyResult{}, ErrUnauthorized
	}
	snapshot, replayed, err := store.CommitProperty(ctx, draft)
	if err != nil {
		return AutonomousPropertyResult{}, err
	}
	result := AutonomousPropertyResult{Property: snapshot, Replayed: replayed}
	return result, service.publishAutonomousProperty(ctx, principal, result)
}

func (service *Service) CurrentAutonomousProperty(
	ctx context.Context,
	principal Principal,
	kind autonomouscompany.PropertyKind,
	initiativeID string,
) (autonomouscompany.PropertySnapshot, error) {
	store, _, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return autonomouscompany.PropertySnapshot{}, err
	}
	return store.CurrentProperty(ctx, kind, initiativeID)
}

func (service *Service) ListAutonomousProperties(
	ctx context.Context,
	principal Principal,
	limit int,
) ([]autonomouscompany.PropertySnapshot, error) {
	store, _, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return nil, err
	}
	return store.ListCurrentProperties(ctx, limit)
}

func (service *Service) CurrentAutonomousRelease(
	ctx context.Context,
	principal Principal,
	initiativeID string,
) (autonomouscompany.ReleasePropertySet, error) {
	store, _, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return autonomouscompany.ReleasePropertySet{}, err
	}
	return store.CurrentReleaseProperties(ctx, initiativeID)
}

func (service *Service) RecordAutonomousNextCycle(
	ctx context.Context,
	principal Principal,
	update autonomouscompany.NextCycleUpdate,
) (AutonomousNextCycleResult, error) {
	store, _, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return AutonomousNextCycleResult{}, err
	}
	event, replayed, err := store.RecordNextCycleUpdate(ctx, update)
	if err != nil {
		return AutonomousNextCycleResult{}, err
	}
	result := AutonomousNextCycleResult{Event: event, Replayed: replayed}
	return result, service.publishAutonomousNextCycle(ctx, principal, result)
}

func (service *Service) ListActiveAutonomousNextCycles(
	ctx context.Context,
	principal Principal,
	limit int,
) ([]autonomouscompany.NextCycleSnapshot, error) {
	store, _, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return nil, err
	}
	return store.ListActiveNextCycles(ctx, limit)
}

func (service *Service) RunAutonomousNextCycles(
	ctx context.Context,
	principal Principal,
	limit int,
) (autonomouscompany.RunResult, error) {
	_, coordinator, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return autonomouscompany.RunResult{}, err
	}
	result, err := coordinator.RunDue(ctx, limit)
	if err != nil {
		return autonomouscompany.RunResult{}, err
	}
	for _, update := range result.Updates {
		if err := service.publishAutonomousNextCycle(ctx, principal, AutonomousNextCycleResult{
			Event: update.Event, Replayed: update.Replay,
		}); err != nil {
			return autonomouscompany.RunResult{}, err
		}
	}
	return result, nil
}

func (service *Service) ReconcileAutonomousNextCycles(
	ctx context.Context,
	principal Principal,
	limit int,
) ([]autonomouscompany.CoordinatedUpdate, error) {
	_, coordinator, err := service.authorizedAutonomousCompany(ctx, principal)
	if err != nil {
		return nil, err
	}
	result, err := coordinator.ReconcileActive(ctx, limit)
	if err != nil {
		return nil, err
	}
	for _, update := range result {
		if err := service.publishAutonomousNextCycle(ctx, principal, AutonomousNextCycleResult{
			Event: update.Event, Replayed: update.Replay,
		}); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (service *Service) publishAutonomousProperty(
	ctx context.Context,
	principal Principal,
	result AutonomousPropertyResult,
) error {
	record := result.Property.Record
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 fmt.Sprintf("event:autonomous-property:%s:%d", record.ID, record.Version),
		OrganizationID:     principal.OrganizationID,
		Type:               "autonomous-company.property." + string(record.State),
		ResourceKind:       "autonomous-company-property",
		ResourceID:         record.ID,
		ResourceVersion:    record.Version,
		VerifiedCompletion: record.State == autonomouscompany.StatePassed,
		Fields: map[string]any{
			"property_kind":  record.Kind,
			"state":          record.State,
			"initiative_id":  record.InitiativeID,
			"evidence_count": len(record.Evidence),
			"process_count":  len(record.Processes),
			"lineage_count":  len(record.Lineage),
			"reason_codes":   record.ReasonCodes,
			"replayed":       result.Replayed,
		},
	})
	return err
}

func (service *Service) publishAutonomousNextCycle(
	ctx context.Context,
	principal Principal,
	result AutonomousNextCycleResult,
) error {
	event := result.Event
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:autonomous-next-cycle:" + event.ID,
		OrganizationID:     principal.OrganizationID,
		Type:               "autonomous-company.next-cycle." + string(event.State),
		ResourceKind:       "autonomous-company-next-cycle",
		ResourceID:         event.PlanID,
		ResourceVersion:    event.Sequence,
		VerifiedCompletion: event.State == autonomouscompany.NextCyclePassed,
		Fields: map[string]any{
			"event_id":       event.ID,
			"state":          event.State,
			"initiative_id":  event.InitiativeID,
			"evidence_count": len(event.Evidence),
			"reason_codes":   event.ReasonCodes,
			"replayed":       result.Replayed,
		},
	})
	return err
}
