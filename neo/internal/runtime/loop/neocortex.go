// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"matrix/cortex"
	"matrix/cortex/journal"
	"matrix/cortexclient"
	"matrix/neo/internal/consolidation"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
)

// NeocortexAdapter implements the resurrection loop seam roles (activation
// source, turn recorder, evidence journal) over one cortexd conversation
// seam. Tool calls and results are recorded exclusively by
// CommitToolExecution as typed write-ahead evidence, so RecordTool is a
// no-op: the tool_call/tool_result events already carry that turn content.
type NeocortexAdapter struct {
	seam *cortexclient.LoopSeam

	mu             sync.Mutex
	recordError    error
	toolExecutions map[cortex.ToolEventCitation]journal.ToolEventPayload
}

func NewNeocortexAdapter(
	seam *cortexclient.LoopSeam,
) (*NeocortexAdapter, error) {
	if seam == nil {
		return nil, fmt.Errorf(
			"runtime loop: neocortex adapter requires a loop seam",
		)
	}
	return &NeocortexAdapter{
		seam:           seam,
		toolExecutions: make(map[cortex.ToolEventCitation]journal.ToolEventPayload),
	}, nil
}

func (adapter *NeocortexAdapter) Activate(
	ctx context.Context,
	request ActivationRequest,
) (string, error) {
	return adapter.seam.Activate(ctx, cortexclient.ActivationQuery{
		Query:    request.Query,
		Premises: request.Premises,
	})
}

func (adapter *NeocortexAdapter) RecordUser(content string) {
	adapter.seam.RecordUser(strings.TrimSpace(content))
}

func (adapter *NeocortexAdapter) RecordAssistant(message protocol.Message) {
	adapter.seam.RecordAssistantWorking(strings.TrimSpace(message.Content))
}

func (adapter *NeocortexAdapter) RecordTool(protocol.Message) {}

func (adapter *NeocortexAdapter) RecordDelivery(content string) {
	adapter.seam.RecordDelivery(strings.TrimSpace(content))
}

// RecordError surfaces asynchronous TurnRecorder failures and synchronous
// consolidation failures through the runtime's existing honest-failure path.
func (adapter *NeocortexAdapter) RecordError() error {
	adapter.mu.Lock()
	err := adapter.recordError
	adapter.mu.Unlock()
	if err != nil {
		return err
	}
	return adapter.seam.RecordError()
}

func (adapter *NeocortexAdapter) ProvenanceRange() (string, uint64, uint64) {
	return adapter.seam.ProvenanceRange()
}

func (adapter *NeocortexAdapter) CommitToolExecution(
	ctx context.Context,
	execution ToolExecution,
) (cortex.ToolEventCitation, error) {
	callID := strings.TrimSpace(execution.Call.ID)
	if callID == "" {
		callID = execution.IdempotencyKey
	}
	citation, err := adapter.seam.CommitToolExecution(ctx, cortexclient.ToolExecution{
		CallID:         callID,
		ToolName:       execution.Call.Name,
		Arguments:      execution.Call.Arguments,
		Result:         execution.Result,
		Error:          execution.Error,
		Expect:         execution.Expect,
		IdempotencyKey: execution.IdempotencyKey,
		MatchVerdict:   execution.MatchVerdict,
		SubgoalID:      execution.SubgoalID,
	})
	if err != nil {
		return cortex.ToolEventCitation{}, err
	}
	payload := journal.ToolEventPayload{
		SchemaVersion:  1,
		CallID:         execution.Call.ID,
		ToolName:       execution.Call.Name,
		Arguments:      append([]byte(nil), execution.Call.Arguments...),
		ResultDigest:   sha256.Sum256(execution.Result),
		Error:          execution.Error,
		Expect:         execution.Expect,
		MatchVerdict:   execution.MatchVerdict,
		SubgoalID:      execution.SubgoalID,
		IdempotencyKey: execution.IdempotencyKey,
	}
	adapter.mu.Lock()
	adapter.toolExecutions[citation] = payload
	adapter.mu.Unlock()
	return citation, nil
}

// VerifyToolEventCitation verifies live cortexd evidence against the exact
// execution that produced the acknowledged MMR coordinates. The legacy Cortex
// journal has a different coordinate system and must never be consulted for a
// cortexd citation.
func (adapter *NeocortexAdapter) VerifyToolEventCitation(
	citation cortex.ToolEventCitation,
) (journal.ToolEventPayload, error) {
	adapter.mu.Lock()
	payload, ok := adapter.toolExecutions[citation]
	adapter.mu.Unlock()
	if !ok {
		return journal.ToolEventPayload{}, cortex.ErrToolCitationMismatch
	}
	payload.Arguments = append([]byte(nil), payload.Arguments...)
	return payload, nil
}

// Consolidate implements the loop's existing consolidation sink. The Go side
// turns the structured delivery job into a bounded, deterministic assertion;
// cortexd remains mechanism-only and applies it through the consolidation gate.
func (adapter *NeocortexAdapter) Consolidate(job consolidation.Job) {
	assertion, err := neocortexConsolidationAssertion(job)
	if err == nil {
		_, err = adapter.seam.Consolidate(
			context.Background(), []cortexclient.Assertion{assertion},
		)
	}
	if err != nil {
		adapter.mu.Lock()
		if adapter.recordError == nil {
			adapter.recordError = fmt.Errorf(
				"runtime loop: neocortex consolidation: %w", err,
			)
		}
		adapter.mu.Unlock()
	}
}

func neocortexConsolidationAssertion(
	job consolidation.Job,
) (cortexclient.Assertion, error) {
	conversation := strings.TrimSpace(job.Conv)
	if conversation == "" || !job.HaveSeq || job.SeqLo == 0 ||
		job.SeqHi < job.SeqLo {
		return cortexclient.Assertion{}, fmt.Errorf(
			"delivery consolidation requires acknowledged provenance",
		)
	}
	identitySeed := strings.TrimSpace(job.IntentID)
	if identitySeed == "" {
		identitySeed = strings.TrimSpace(job.Objective)
	}
	if identitySeed == "" {
		return cortexclient.Assertion{}, fmt.Errorf(
			"delivery consolidation requires an intent identity",
		)
	}
	value, err := json.Marshal(struct {
		IntentID  string                   `json:"intent_id"`
		Attempt   int                      `json:"attempt"`
		Terminal  bool                     `json:"terminal"`
		Objective string                   `json:"objective,omitempty"`
		Evidence  []consolidation.Evidence `json:"evidence,omitempty"`
		Outcome   string                   `json:"outcome,omitempty"`
	}{
		IntentID: strings.TrimSpace(job.IntentID), Attempt: job.Attempt,
		Terminal: job.Terminal, Objective: strings.TrimSpace(job.Objective),
		Evidence: job.Evidence, Outcome: strings.TrimSpace(job.Outcome),
	})
	if err != nil {
		return cortexclient.Assertion{}, err
	}
	identity := sha256.Sum256([]byte(
		"neo-consolidation-v1\x00" + conversation + "\x00" + identitySeed,
	))
	return cortexclient.Assertion{
		BeliefID:          append([]byte(nil), identity[:16]...),
		Type:              0,
		CanonicalIdentity: fmt.Sprintf("neo-intent:%x", identity[:]),
		Value:             value,
		ValidFromNs:       1,
		Provenance:        [][2]uint64{{job.SeqLo, job.SeqHi}},
	}, nil
}

// NeocortexCheckpointStore serves the turnstate role on the neocortex
// substrate: turn checkpoints become durable checkpoint events in cortexd
// (fail-closed) and are mirrored into the local turnstate store so resume
// paths that still read it keep working side by side until the wave-7
// consumer re-point. Status and recovery transitions are process-local turn
// lifecycle, not memory, and stay in the turnstate store.
type NeocortexCheckpointStore struct {
	Seam  *cortexclient.LoopSeam
	Turns CheckpointStore
}

func (store *NeocortexCheckpointStore) SaveTurnCheckpoint(
	ctx context.Context,
	turnID string,
	checkpoint turnstate.Checkpoint,
) error {
	blob, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf(
			"runtime loop: encode neocortex checkpoint: %w", err,
		)
	}
	if _, err := store.Seam.SaveTurnCheckpoint(ctx, turnID, blob); err != nil {
		return err
	}
	return store.Turns.SaveTurnCheckpoint(ctx, turnID, checkpoint)
}

// LoadTurnState keeps process-local lifecycle metadata in turnstate but always
// replaces its checkpoint with the latest cortexd projection. Missing,
// malformed, or unavailable cortexd state is an error; local checkpoint bytes
// are never a recovery fallback on the neocortex substrate.
func (store *NeocortexCheckpointStore) LoadTurnState(
	ctx context.Context,
	turnID string,
) (turnstate.TurnState, error) {
	loader, ok := store.Turns.(interface {
		LoadTurnState(context.Context, string) (turnstate.TurnState, error)
	})
	if !ok {
		return turnstate.TurnState{}, fmt.Errorf(
			"runtime loop: neocortex checkpoint metadata store cannot load turns",
		)
	}
	state, err := loader.LoadTurnState(ctx, turnID)
	if err != nil {
		return turnstate.TurnState{}, err
	}
	blob, _, err := store.Seam.LatestTurnCheckpoint(ctx, turnID)
	if err != nil {
		return turnstate.TurnState{}, fmt.Errorf(
			"runtime loop: load neocortex checkpoint: %w", err,
		)
	}
	var checkpoint turnstate.Checkpoint
	if err := json.Unmarshal(blob, &checkpoint); err != nil {
		return turnstate.TurnState{}, fmt.Errorf(
			"runtime loop: decode neocortex checkpoint: %w", err,
		)
	}
	state.Checkpoint = &checkpoint
	return state, nil
}

func (store *NeocortexCheckpointStore) SetTurnRecovery(
	ctx context.Context,
	turnID string,
	status turnstate.Status,
	recovery turnstate.Recovery,
) error {
	return store.Turns.SetTurnRecovery(ctx, turnID, status, recovery)
}

func (store *NeocortexCheckpointStore) SetTurnStatus(
	ctx context.Context,
	turnID string,
	status turnstate.Status,
) error {
	return store.Turns.SetTurnStatus(ctx, turnID, status)
}
