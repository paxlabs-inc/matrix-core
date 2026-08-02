package autonomouscompany

import (
	"context"
	"fmt"
	"time"

	"matrix/workforce/internal/learning"
)

type NextCycleExecutor interface {
	DispatchNextCycle(context.Context, NextCycleSnapshot) (NextCycleUpdate, error)
	ReconcileNextCycle(context.Context, NextCycleSnapshot) (NextCycleUpdate, error)
}

type CoordinatedUpdate struct {
	Snapshot NextCycleSnapshot `json:"snapshot"`
	Event    NextCycleEvent    `json:"event"`
	Replay   bool              `json:"replay"`
}

type RunResult struct {
	Claimed []NextCycleSnapshot `json:"claimed"`
	Updates []CoordinatedUpdate `json:"updates"`
}

type Coordinator struct {
	store    *Store
	executor NextCycleExecutor
}

func NewCoordinator(store *Store, executor NextCycleExecutor) (*Coordinator, error) {
	if store == nil || executor == nil {
		return nil, fmt.Errorf("autonomous company: coordinator dependencies are required")
	}
	return &Coordinator{store: store, executor: executor}, nil
}

func (coordinator *Coordinator) Store() *Store { return coordinator.store }

func (coordinator *Coordinator) RunDue(
	ctx context.Context,
	limit int,
) (RunResult, error) {
	if limit <= 0 || limit > 100 {
		return RunResult{}, fmt.Errorf("autonomous company: next-cycle run limit is invalid")
	}
	result := RunResult{}
	planned, err := coordinator.store.ListNextCycles(
		ctx, []NextCycleState{NextCyclePlanned}, limit,
	)
	if err != nil {
		return result, err
	}
	if len(planned) < limit {
		claimed, err := coordinator.store.ClaimDueNextCycles(ctx, limit-len(planned))
		if err != nil {
			return result, err
		}
		result.Claimed = append(result.Claimed, claimed...)
		planned = append(planned, claimed...)
	}
	for _, snapshot := range planned {
		update, err := coordinator.dispatch(ctx, snapshot)
		if err != nil {
			return result, err
		}
		event, replay, err := coordinator.store.RecordNextCycleUpdate(ctx, update)
		if err != nil {
			return result, err
		}
		result.Updates = append(result.Updates, CoordinatedUpdate{
			Snapshot: snapshot, Event: event, Replay: replay,
		})
	}
	return result, nil
}

func (coordinator *Coordinator) DispatchPlanned(
	ctx context.Context,
	limit int,
) ([]CoordinatedUpdate, error) {
	planned, err := coordinator.store.ListNextCycles(
		ctx, []NextCycleState{NextCyclePlanned}, limit,
	)
	if err != nil {
		return nil, err
	}
	result := make([]CoordinatedUpdate, 0, len(planned))
	for _, snapshot := range planned {
		update, err := coordinator.dispatch(ctx, snapshot)
		if err != nil {
			return result, err
		}
		event, replay, err := coordinator.store.RecordNextCycleUpdate(ctx, update)
		if err != nil {
			return result, err
		}
		result = append(result, CoordinatedUpdate{
			Snapshot: snapshot, Event: event, Replay: replay,
		})
	}
	return result, nil
}

func (coordinator *Coordinator) ReconcileActive(
	ctx context.Context,
	limit int,
) ([]CoordinatedUpdate, error) {
	active, err := coordinator.store.ListActiveNextCycles(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]CoordinatedUpdate, 0, len(active))
	for _, snapshot := range active {
		update, err := coordinator.executor.ReconcileNextCycle(ctx, snapshot)
		if err != nil {
			return result, err
		}
		event, replay, err := coordinator.store.RecordNextCycleUpdate(ctx, update)
		if err != nil {
			return result, err
		}
		result = append(result, CoordinatedUpdate{
			Snapshot: snapshot, Event: event, Replay: replay,
		})
	}
	return result, nil
}

func (coordinator *Coordinator) dispatch(
	ctx context.Context,
	snapshot NextCycleSnapshot,
) (NextCycleUpdate, error) {
	if snapshot.Plan.SelectedAction != learning.ActionHumanReview {
		return coordinator.executor.DispatchNextCycle(ctx, snapshot)
	}
	now, err := coordinator.store.currentTime()
	if err != nil {
		return NextCycleUpdate{}, err
	}
	return NextCycleUpdate{
		PlanID:      snapshot.Plan.ID,
		PlanHash:    snapshot.CanonicalHash,
		State:       NextCycleBlocked,
		ReasonCodes: []string{"founder_required"},
		OccurredAt:  now,
	}, nil
}

func NewNextCycleUpdate(
	snapshot NextCycleSnapshot,
	state NextCycleState,
	evidence []EvidenceBinding,
	reasonCodes []string,
	occurredAt time.Time,
) NextCycleUpdate {
	return NextCycleUpdate{
		PlanID:      snapshot.Plan.ID,
		PlanHash:    snapshot.CanonicalHash,
		State:       state,
		Evidence:    append([]EvidenceBinding(nil), evidence...),
		ReasonCodes: append([]string(nil), reasonCodes...),
		OccurredAt:  occurredAt,
	}
}
