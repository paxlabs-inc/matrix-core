// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/neo/internal/runtime/records"
)

type compatibilityIdentity struct {
	ActorID     string `json:"actor_id"`
	Surface     string `json:"surface,omitempty"`
	Origin      string `json:"origin,omitempty"`
	Status      Status `json:"status"`
	FailureCode string `json:"failure_code,omitempty"`
}

type compatibilityCycle struct {
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	Recovery   *Recovery   `json:"recovery,omitempty"`
}

func canonicalFromCompatibility(state TurnState) (records.TurnRecord, error) {
	identity, err := json.Marshal(compatibilityIdentity{
		ActorID: state.ActorID, Surface: state.Surface, Origin: state.Origin,
		Status: state.Status, FailureCode: state.FailureCode,
	})
	if err != nil {
		return records.TurnRecord{}, err
	}
	record := records.TurnRecord{
		LogicalTurnID: state.TurnID, ConversationID: state.SessionID,
		RequestIdentity: string(identity), Objective: state.Content,
		LatestGenuineMessageID: state.TurnID + ":request",
		CurrentState:           records.StateAccepted,
	}
	return record, record.Validate()
}

func (store *Store) compatibilityFromCanonical(ctx context.Context, record records.TurnRecord) (TurnState, error) {
	identity := compatibilityIdentity{ActorID: record.RequestIdentity, Status: compatibilityStatus(record.CurrentState)}
	_ = json.Unmarshal([]byte(record.RequestIdentity), &identity)
	state := TurnState{
		TurnID: record.LogicalTurnID, ActorID: identity.ActorID,
		SessionID: record.ConversationID, Content: record.Objective,
		Surface: identity.Surface, Origin: identity.Origin,
		Status: identity.Status, FailureCode: identity.FailureCode,
	}
	if state.Status == "" {
		state.Status = compatibilityStatus(record.CurrentState)
	}
	cycle, updatedAt, err := store.loadLatestCompatibilityCycle(ctx, record.LogicalTurnID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TurnState{}, err
	}
	if err == nil {
		state.Checkpoint = cycle.Checkpoint
		state.Recovery = cycle.Recovery
		state.UpdatedAt = updatedAt
	} else {
		state.UpdatedAt, err = store.canonicalUpdatedAt(ctx, record.LogicalTurnID, records.KindTurn, "current")
		if err != nil {
			return TurnState{}, err
		}
	}
	return state, state.Validate()
}

func (store *Store) saveCompatibilityCycle(ctx context.Context, state TurnState) error {
	payload, err := json.Marshal(compatibilityCycle{Checkpoint: state.Checkpoint, Recovery: state.Recovery})
	if err != nil {
		return fmt.Errorf("turnstate: encode compatibility cycle: %w", err)
	}
	generation := uint64(0)
	if state.Checkpoint != nil && state.Checkpoint.Step > 0 {
		generation = uint64(state.Checkpoint.Step)
	}
	record, loadErr := store.LoadCycleRecord(ctx, state.TurnID, generation)
	if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
		return loadErr
	}
	if errors.Is(loadErr, sql.ErrNoRows) {
		record = records.CycleRecord{GenerationNumber: generation}
	}
	record.ProviderRequest = payload
	record.NextIntendedAction = compatibilityNextAction(state)
	if record.ContextManifest.Entries == nil {
		record.ContextManifest = records.ContextManifest{}
	}
	if record.StreamedOutputState == nil {
		record.StreamedOutputState = []records.StreamedOutput{}
	}
	if record.ProposedToolCalls == nil {
		record.ProposedToolCalls = []records.ProposedToolCall{}
	}
	if record.ObservedToolOutcomes == nil {
		record.ObservedToolOutcomes = []records.ToolOutcome{}
	}
	return store.SaveCycleRecord(ctx, state.TurnID, record)
}

func (store *Store) loadLatestCompatibilityCycle(ctx context.Context, logicalTurnID string) (compatibilityCycle, time.Time, error) {
	var key string
	var sealed []byte
	var updatedMicros int64
	err := store.readDB.QueryRowContext(ctx,
		`SELECT record_key, state, updated_at FROM canonical_records
		 WHERE logical_turn_id = ? AND record_type = ?
		 ORDER BY CAST(record_key AS INTEGER) DESC, updated_at DESC LIMIT 1`,
		logicalTurnID, string(records.KindCycle),
	).Scan(&key, &sealed, &updatedMicros)
	if err != nil {
		return compatibilityCycle{}, time.Time{}, err
	}
	plaintext, err := store.openCanonical(logicalTurnID, records.KindCycle, key, sealed)
	if err != nil {
		return compatibilityCycle{}, time.Time{}, err
	}
	cycle, err := records.DecodeCycle(plaintext)
	if err != nil {
		return compatibilityCycle{}, time.Time{}, err
	}
	var compatibility compatibilityCycle
	if err := json.Unmarshal(cycle.ProviderRequest, &compatibility); err != nil {
		var checkpoint Checkpoint
		if checkpointErr := json.Unmarshal(cycle.ProviderRequest, &checkpoint); checkpointErr != nil {
			return compatibilityCycle{}, time.Time{}, fmt.Errorf("turnstate: decode compatibility cycle: %w", err)
		}
		compatibility.Checkpoint = &checkpoint
	}
	return compatibility, time.UnixMicro(updatedMicros).UTC(), nil
}

func (store *Store) updateCompatibilityIdentity(ctx context.Context, turnID string, update func(*compatibilityIdentity)) error {
	return store.UpdateTurnRecord(ctx, turnID, func(record *records.TurnRecord) error {
		identity := compatibilityIdentity{ActorID: record.RequestIdentity, Status: compatibilityStatus(record.CurrentState)}
		_ = json.Unmarshal([]byte(record.RequestIdentity), &identity)
		update(&identity)
		encoded, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		record.RequestIdentity = string(encoded)
		return nil
	})
}

func (store *Store) canonicalUpdatedAt(ctx context.Context, logicalTurnID string, kind records.Kind, key string) (time.Time, error) {
	var micros int64
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT updated_at FROM canonical_records
		 WHERE logical_turn_id = ? AND record_type = ? AND record_key = ?`,
		logicalTurnID, string(kind), key,
	).Scan(&micros); err != nil {
		return time.Time{}, err
	}
	return time.UnixMicro(micros).UTC(), nil
}

func compatibilityStatus(state records.TurnState) Status {
	switch state {
	case records.StateDelivered:
		return StatusCompleted
	case records.StateRetryingGeneration, records.StateReconciliationRequired:
		return StatusRecovering
	case records.StateBlockedAwaitingPerson:
		return StatusIncomplete
	default:
		return StatusRunning
	}
}

func compatibilityNextAction(state TurnState) string {
	if state.Recovery != nil {
		return state.Recovery.Advice
	}
	if state.Checkpoint != nil && state.Checkpoint.Coding != nil {
		return state.Checkpoint.Coding.NextAction
	}
	return "continue logical turn"
}

func (store *Store) legacyTurnState(ctx context.Context, turnID string) (TurnState, error) {
	var envelope []byte
	if err := store.readDB.QueryRowContext(ctx,
		`SELECT state FROM turn_state WHERE turn_id = ?`, turnID,
	).Scan(&envelope); err != nil {
		return TurnState{}, fmt.Errorf("turnstate: load legacy turn: %w", err)
	}
	return store.openTurn(turnID, envelope)
}

func compatibilityStatePath(from, to records.TurnState) ([]records.TurnState, error) {
	if from == to {
		return nil, nil
	}
	states := []records.TurnState{
		records.StateAccepted, records.StatePreparing, records.StateGenerating,
		records.StateAwaitingTools, records.StateExecutingTools, records.StateSynthesisOwed,
		records.StateAnswerReady, records.StateDelivering, records.StateDelivered,
		records.StateReconciliationRequired, records.StateRetryingGeneration,
		records.StateBlockedAwaitingPerson, records.StateDeliveryRetry,
	}
	type node struct {
		state records.TurnState
		path  []records.TurnState
	}
	queue := []node{{state: from}}
	seen := map[records.TurnState]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, candidate := range states {
			if seen[candidate] || !canonicalTransitionAllowed(current.state, candidate) {
				continue
			}
			path := append(append([]records.TurnState(nil), current.path...), candidate)
			if candidate == to {
				return path, nil
			}
			seen[candidate] = true
			queue = append(queue, node{state: candidate, path: path})
		}
	}
	return nil, fmt.Errorf("turnstate: no compatibility path %s -> %s", from, to)
}

func (store *Store) transitionCompatibilityStatus(ctx context.Context, turnID string, status Status) error {
	record, err := store.LoadTurnRecord(ctx, turnID)
	if err != nil {
		return err
	}
	target := record.CurrentState
	switch status {
	case StatusRunning:
		if target == records.StateAccepted || target == records.StatePreparing {
			target = records.StateGenerating
		}
	case StatusRecovering, StatusInterrupted:
		target = records.StateRetryingGeneration
	case StatusIncomplete, StatusFailed, StatusCancelled:
		target = records.StateBlockedAwaitingPerson
	case StatusCompleted:
		target = records.StateDelivered
	}
	path, err := compatibilityStatePath(record.CurrentState, target)
	if err != nil {
		return err
	}
	for _, next := range path {
		if err := store.TransitionTurn(ctx, turnID, next, nil); err != nil {
			return err
		}
	}
	return nil
}

func normalizeCompatibilityIdentity(value string) string {
	return strings.TrimSpace(value)
}
