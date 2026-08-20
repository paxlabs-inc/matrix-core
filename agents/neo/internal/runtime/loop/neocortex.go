// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"centra/core/cortexclient"
	"centra/agents/neo/internal/consolidation"
	"centra/agents/neo/internal/runtime/protocol"
	"centra/agents/neo/internal/runtime/records"
	"centra/agents/neo/internal/runtime/turnstate"
)

// NeocortexAdapter implements the resurrection loop seam roles (activation
// source, turn recorder, evidence journal) over one cortexd conversation
// seam. Working assistant text remains in the authoritative turn checkpoint;
// only genuine user input and delivered answers enter semantic conversation
// memory. Tool calls/results are typed execution evidence and never duplicated.
type NeocortexAdapter struct {
	seam *cortexclient.LoopSeam

	mu             sync.Mutex
	recordError    error
	toolExecutions map[cortexclient.ToolEventCitation]cortexclient.ToolEventPayload
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
		toolExecutions: make(map[cortexclient.ToolEventCitation]cortexclient.ToolEventPayload),
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

func (adapter *NeocortexAdapter) RecordAssistant(protocol.Message) {}

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
) (cortexclient.ToolEventCitation, error) {
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
		return cortexclient.ToolEventCitation{}, err
	}
	payload := cortexclient.ToolEventPayload{
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
// execution that produced the acknowledged MMR coordinates. The legacy Neocortex
// journal has a different coordinate system and must never be consulted for a
// cortexd citation.
func (adapter *NeocortexAdapter) VerifyToolEventCitation(
	citation cortexclient.ToolEventCitation,
) (cortexclient.ToolEventPayload, error) {
	adapter.mu.Lock()
	payload, ok := adapter.toolExecutions[citation]
	adapter.mu.Unlock()
	if !ok {
		return cortexclient.ToolEventPayload{}, cortexclient.ErrToolCitationMismatch
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

// NeocortexCheckpointStore keeps operational recovery state in turnstate,
// outside the semantic Neocortex event log and every cognitive projection.
// Seam remains available for semantic recording; checkpoints never use it.
type NeocortexCheckpointStore struct {
	Seam  *cortexclient.LoopSeam
	Turns CheckpointStore
}

func (store *NeocortexCheckpointStore) SaveTurnCheckpoint(
	ctx context.Context,
	turnID string,
	checkpoint turnstate.Checkpoint,
) error {
	return store.Turns.SaveTurnCheckpoint(ctx, turnID, checkpoint)
}

func (store *NeocortexCheckpointStore) SaveContextManifest(ctx context.Context, turnID string, generation uint64, manifest records.ContextManifest) error {
	target, ok := store.Turns.(ContextManifestStore)
	if !ok {
		return nil
	}
	return target.SaveContextManifest(ctx, turnID, generation, manifest)
}

func (store *NeocortexCheckpointStore) SaveConvergenceRecord(ctx context.Context, turnID string, record records.ConvergenceRecord) error {
	target, ok := store.Turns.(ConvergenceStore)
	if !ok {
		return nil
	}
	return target.SaveConvergenceRecord(ctx, turnID, record)
}

func (store *NeocortexCheckpointStore) LoadConvergenceRecord(ctx context.Context, turnID string) (records.ConvergenceRecord, error) {
	target, ok := store.Turns.(ConvergenceStore)
	if !ok {
		return records.ConvergenceRecord{}, sql.ErrNoRows
	}
	return target.LoadConvergenceRecord(ctx, turnID)
}

func (store *NeocortexCheckpointStore) SavePendingEffect(
	ctx context.Context,
	turnID string,
	checkpoint turnstate.Checkpoint,
	idempotencyKey string,
	toolName string,
	arguments json.RawMessage,
	retrySafe bool,
) error {
	atomicStore, ok := store.Turns.(PendingEffectStore)
	if !ok {
		return fmt.Errorf(
			"runtime loop: operational store lacks atomic pending-effect support",
		)
	}
	return atomicStore.SavePendingEffect(
		ctx, turnID, checkpoint, idempotencyKey, toolName, arguments, retrySafe,
	)
}

// LoadTurnState restores operational state from its dedicated store; semantic
// activation has no path to these bytes.
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
	return loader.LoadTurnState(ctx, turnID)
}

func (store *NeocortexCheckpointStore) LoadTurnRecord(
	ctx context.Context,
	turnID string,
) (records.TurnRecord, error) {
	target, ok := store.Turns.(SynthesisDebtStore)
	if !ok {
		return records.TurnRecord{}, fmt.Errorf(
			"runtime loop: operational store cannot load canonical turns",
		)
	}
	return target.LoadTurnRecord(ctx, turnID)
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

func (store *NeocortexCheckpointStore) SaveAnswerRecord(
	ctx context.Context,
	turnID string,
	answerIdentity string,
	record records.AnswerRecord,
) error {
	target, ok := store.Turns.(AnswerStateStore)
	if !ok {
		return fmt.Errorf(
			"runtime loop: operational store lacks durable answer support",
		)
	}
	return target.SaveAnswerRecord(ctx, turnID, answerIdentity, record)
}

func (store *NeocortexCheckpointStore) LoadAnswerRecord(
	ctx context.Context,
	turnID string,
	answerIdentity string,
) (records.AnswerRecord, error) {
	target, err := store.deliveryStateStore()
	if err != nil {
		return records.AnswerRecord{}, err
	}
	return target.LoadAnswerRecord(ctx, turnID, answerIdentity)
}

func (store *NeocortexCheckpointStore) SaveDeliveryRecord(
	ctx context.Context,
	turnID string,
	deliveryIdentity string,
	record records.DeliveryRecord,
) error {
	target, err := store.deliveryStateStore()
	if err != nil {
		return err
	}
	return target.SaveDeliveryRecord(ctx, turnID, deliveryIdentity, record)
}

func (store *NeocortexCheckpointStore) LoadDeliveryRecord(
	ctx context.Context,
	turnID string,
	deliveryIdentity string,
) (records.DeliveryRecord, error) {
	target, err := store.deliveryStateStore()
	if err != nil {
		return records.DeliveryRecord{}, err
	}
	return target.LoadDeliveryRecord(ctx, turnID, deliveryIdentity)
}

func (store *NeocortexCheckpointStore) MarkAnswerReady(
	ctx context.Context,
	turnID string,
	answerIdentity string,
) error {
	target, err := store.deliveryStateStore()
	if err != nil {
		return err
	}
	return target.MarkAnswerReady(ctx, turnID, answerIdentity)
}

func (store *NeocortexCheckpointStore) MarkDelivering(
	ctx context.Context,
	turnID string,
	deliveryIdentity string,
) error {
	target, err := store.deliveryStateStore()
	if err != nil {
		return err
	}
	return target.MarkDelivering(ctx, turnID, deliveryIdentity)
}

func (store *NeocortexCheckpointStore) MarkDeliveryRetry(
	ctx context.Context,
	turnID string,
) error {
	target, err := store.deliveryStateStore()
	if err != nil {
		return err
	}
	return target.MarkDeliveryRetry(ctx, turnID)
}

func (store *NeocortexCheckpointStore) MarkDelivered(
	ctx context.Context,
	turnID string,
) error {
	target, err := store.deliveryStateStore()
	if err != nil {
		return err
	}
	return target.MarkDelivered(ctx, turnID)
}

func (store *NeocortexCheckpointStore) deliveryStateStore() (
	DeliveryStateStore,
	error,
) {
	target, ok := store.Turns.(DeliveryStateStore)
	if !ok {
		return nil, fmt.Errorf(
			"runtime loop: operational store lacks durable delivery support",
		)
	}
	return target, nil
}

var _ PendingEffectStore = (*NeocortexCheckpointStore)(nil)
var _ DeliveryStateStore = (*NeocortexCheckpointStore)(nil)
var _ SynthesisDebtStore = (*NeocortexCheckpointStore)(nil)
