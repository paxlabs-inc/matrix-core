// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package belief

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"matrix/cortexclient"
	"matrix/neo/internal/runtime/liveness"
	runtimeloop "matrix/neo/internal/runtime/loop"
)

type PremiseStatus string

const (
	PremiseAssumption PremiseStatus = "assumption"
	PremiseCited      PremiseStatus = "cited"
	PremiseRefuted    PremiseStatus = "refuted"
)

var (
	ErrCitationRequired     = errors.New("runtime belief: verified citation required")
	ErrPremiseBlocked       = errors.New("runtime belief: refuted or unverified premise blocks action")
	ErrEvidenceInsufficient = errors.New("runtime belief: evidence cannot complete subgoal")
)

type Premise struct {
	ID        string                          `json:"id"`
	Statement string                          `json:"statement"`
	Status    PremiseStatus                   `json:"status"`
	Citation  *cortexclient.ToolEventCitation `json:"citation,omitempty"`
}

type Prediction struct {
	ToolName string                         `json:"tool_name"`
	Expect   string                         `json:"expect"`
	Verdict  string                         `json:"verdict"`
	Citation cortexclient.ToolEventCitation `json:"citation"`
}

type Subgoal struct {
	ID        string                           `json:"id"`
	Completed bool                             `json:"completed"`
	Evidence  []cortexclient.ToolEventCitation `json:"evidence,omitempty"`
}

type Capability struct {
	ToolName          string `json:"tool_name"`
	VerifiedSuccesses int    `json:"verified_successes"`
}

type DurableState struct {
	Premises     map[string]Premise    `json:"premises"`
	Predictions  []Prediction          `json:"predictions"`
	Subgoals     map[string]Subgoal    `json:"subgoals"`
	Capabilities map[string]Capability `json:"capabilities"`
}

type CitationVerifier interface {
	VerifyToolEventCitation(
		cortexclient.ToolEventCitation,
	) (cortexclient.ToolEventPayload, error)
}

type CognitionStore interface {
	SaveCognitionState(context.Context, string, json.RawMessage) error
	LoadCognitionState(context.Context, string) (json.RawMessage, error)
}

type State struct {
	mu        sync.Mutex
	sessionID string
	verifier  CitationVerifier
	store     CognitionStore
	durable   DurableState
}

func New(
	sessionID string,
	verifier CitationVerifier,
	store CognitionStore,
) (*State, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || verifier == nil || store == nil {
		return nil, fmt.Errorf(
			"runtime belief: session, citation verifier, and store are required",
		)
	}
	return &State{
		sessionID: sessionID,
		verifier:  verifier,
		store:     store,
		durable:   emptyState(),
	}, nil
}

func (state *State) Restore(ctx context.Context) error {
	raw, err := state.store.LoadCognitionState(ctx, state.sessionID)
	if err != nil {
		return err
	}
	var restored DurableState
	if err := json.Unmarshal(raw, &restored); err != nil {
		return fmt.Errorf("runtime belief: decode cognition state: %w", err)
	}
	normalizeState(&restored)
	state.mu.Lock()
	state.durable = restored
	state.mu.Unlock()
	return nil
}

func (state *State) AddPremise(
	ctx context.Context,
	id string,
	statement string,
) error {
	id = strings.TrimSpace(id)
	statement = strings.TrimSpace(statement)
	if id == "" || statement == "" {
		return fmt.Errorf("runtime belief: premise ID and statement are required")
	}
	return state.mutate(ctx, func(next *DurableState) error {
		if _, exists := next.Premises[id]; exists {
			return fmt.Errorf("runtime belief: premise %s already exists", id)
		}
		next.Premises[id] = Premise{
			ID: id, Statement: statement, Status: PremiseAssumption,
		}
		return nil
	})
}

func (state *State) CitePremise(
	ctx context.Context,
	id string,
	citation cortexclient.ToolEventCitation,
) error {
	payload, err := state.verifier.VerifyToolEventCitation(citation)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCitationRequired, err)
	}
	if payload.Error != "" ||
		payload.MatchVerdict != cortexclient.ToolMatchMatched {
		return ErrCitationRequired
	}
	return state.mutate(ctx, func(next *DurableState) error {
		premise, ok := next.Premises[id]
		if !ok {
			return fmt.Errorf("runtime belief: premise %s not found", id)
		}
		premise.Status = PremiseCited
		premise.Citation = &citation
		next.Premises[id] = premise
		return nil
	})
}

func (state *State) RefutePremise(
	ctx context.Context,
	id string,
	citation cortexclient.ToolEventCitation,
) error {
	payload, err := state.verifier.VerifyToolEventCitation(citation)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCitationRequired, err)
	}
	if payload.Error == "" &&
		payload.MatchVerdict != cortexclient.ToolMatchMismatched {
		return ErrCitationRequired
	}
	return state.mutate(ctx, func(next *DurableState) error {
		premise, ok := next.Premises[id]
		if !ok {
			return fmt.Errorf("runtime belief: premise %s not found", id)
		}
		premise.Status = PremiseRefuted
		premise.Citation = &citation
		next.Premises[id] = premise
		return nil
	})
}

func (state *State) CanAct(premiseIDs ...string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, id := range premiseIDs {
		premise, ok := state.durable.Premises[id]
		if !ok || premise.Status != PremiseCited {
			return fmt.Errorf("%w: %s", ErrPremiseBlocked, id)
		}
	}
	return nil
}

func (state *State) ObserveToolExecution(
	ctx context.Context,
	execution runtimeloop.ToolExecution,
) error {
	if execution.Citation == nil {
		return ErrCitationRequired
	}
	payload, err := state.verifier.VerifyToolEventCitation(
		*execution.Citation,
	)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCitationRequired, err)
	}
	if err := executionMatchesPayload(execution, payload); err != nil {
		return err
	}
	return state.mutate(ctx, func(next *DurableState) error {
		citation := *execution.Citation
		next.Predictions = append(next.Predictions, Prediction{
			ToolName: execution.Call.Name,
			Expect:   execution.Expect,
			Verdict:  execution.MatchVerdict,
			Citation: citation,
		})
		subgoal := next.Subgoals[execution.SubgoalID]
		subgoal.ID = execution.SubgoalID
		if execution.Error == "" &&
			execution.MatchVerdict == cortexclient.ToolMatchMatched {
			subgoal.Completed = true
			subgoal.Evidence = append(subgoal.Evidence, citation)
			capability := next.Capabilities[execution.Call.Name]
			capability.ToolName = execution.Call.Name
			capability.VerifiedSuccesses++
			next.Capabilities[execution.Call.Name] = capability
		} else if execution.MatchVerdict == cortexclient.ToolMatchMismatched ||
			execution.Error != "" {
			id := "prediction:" + execution.Call.ID
			next.Premises[id] = Premise{
				ID:        id,
				Statement: execution.Expect,
				Status:    PremiseRefuted,
				Citation:  &citation,
			}
		}
		next.Subgoals[execution.SubgoalID] = subgoal
		return nil
	})
}

// MeasuredContext is the loop's LivenessSource adapter: it reports counters,
// never premise text. UnsupportedPremises counts every premise the belief state
// does not hold a verified citation for — assumptions and refutations alike,
// which is what raises verification depth. ActionsWithoutGrowth counts the
// trailing run of prediction records that produced no matched evidence, which
// is the runtime's no-progress read.
func (state *State) MeasuredContext() liveness.MeasuredContext {
	state.mu.Lock()
	defer state.mu.Unlock()
	measured := liveness.MeasuredContext{}
	for _, premise := range state.durable.Premises {
		if premise.Status != PremiseCited {
			measured.UnsupportedPremises++
		}
	}
	for index := len(state.durable.Predictions) - 1; index >= 0; index-- {
		if state.durable.Predictions[index].Verdict ==
			cortexclient.ToolMatchMatched {
			break
		}
		measured.ActionsWithoutGrowth++
	}
	return measured
}

func (state *State) Snapshot() DurableState {
	state.mu.Lock()
	defer state.mu.Unlock()
	return cloneState(state.durable)
}

func (state *State) mutate(
	ctx context.Context,
	apply func(*DurableState) error,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	next := cloneState(state.durable)
	if err := apply(&next); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := state.store.SaveCognitionState(
		ctx, state.sessionID, raw,
	); err != nil {
		return fmt.Errorf("runtime belief: persist cognition state: %w", err)
	}
	state.durable = next
	return nil
}

func executionMatchesPayload(
	execution runtimeloop.ToolExecution,
	payload cortexclient.ToolEventPayload,
) error {
	if payload.CallID != execution.Call.ID ||
		payload.ToolName != execution.Call.Name ||
		string(payload.Arguments) != string(execution.Call.Arguments) ||
		payload.ResultDigest != sha256.Sum256(execution.Result) ||
		payload.Error != execution.Error ||
		payload.Expect != execution.Expect ||
		payload.MatchVerdict != execution.MatchVerdict ||
		payload.SubgoalID != execution.SubgoalID ||
		payload.IdempotencyKey != execution.IdempotencyKey {
		return ErrCitationRequired
	}
	return nil
}

func emptyState() DurableState {
	return DurableState{
		Premises:     make(map[string]Premise),
		Subgoals:     make(map[string]Subgoal),
		Capabilities: make(map[string]Capability),
	}
}

func normalizeState(state *DurableState) {
	if state.Premises == nil {
		state.Premises = make(map[string]Premise)
	}
	if state.Subgoals == nil {
		state.Subgoals = make(map[string]Subgoal)
	}
	if state.Capabilities == nil {
		state.Capabilities = make(map[string]Capability)
	}
}

func cloneState(source DurableState) DurableState {
	raw, _ := json.Marshal(source)
	var cloned DurableState
	_ = json.Unmarshal(raw, &cloned)
	normalizeState(&cloned)
	return cloned
}
