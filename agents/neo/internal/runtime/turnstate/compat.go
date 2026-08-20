// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"centra/agents/neo/internal/runtime/records"
	"centra/agents/neo/internal/sessionjournal"
)

type LegacyJournalReader interface {
	Read(context.Context, string, int64, int) ([]sessionjournal.Event, error)
}

type CanonicalSnapshot struct {
	Turn        records.TurnRecord
	Cycles      []records.CycleRecord
	Effects     map[string]records.EffectRecord
	Convergence records.ConvergenceRecord
	Answers     map[string]records.AnswerRecord
	Deliveries  map[string]records.DeliveryRecord
}

func ReadLegacyJournal(
	ctx context.Context,
	reader LegacyJournalReader,
	conversationID string,
	logicalTurnID string,
	requestIdentity string,
) (CanonicalSnapshot, error) {
	if reader == nil {
		return CanonicalSnapshot{}, fmt.Errorf("turnstate: legacy journal reader is required")
	}
	events, err := reader.Read(ctx, conversationID, 1, 0)
	if err != nil {
		return CanonicalSnapshot{}, err
	}
	return ImportLegacyJournal(events, conversationID, logicalTurnID, requestIdentity)
}

func ImportLegacyJournal(
	events []sessionjournal.Event,
	conversationID string,
	logicalTurnID string,
	requestIdentity string,
) (CanonicalSnapshot, error) {
	conversationID = strings.TrimSpace(conversationID)
	logicalTurnID = strings.TrimSpace(logicalTurnID)
	requestIdentity = strings.TrimSpace(requestIdentity)
	if conversationID == "" || logicalTurnID == "" || requestIdentity == "" {
		return CanonicalSnapshot{}, fmt.Errorf("turnstate: legacy import identities are required")
	}
	snapshot := CanonicalSnapshot{
		Effects:    make(map[string]records.EffectRecord),
		Answers:    make(map[string]records.AnswerRecord),
		Deliveries: make(map[string]records.DeliveryRecord),
	}
	state := records.StateAccepted
	objective := "legacy journal recovery"
	latestMessageID := "legacy:0"
	generation := uint64(1)
	cycle := records.CycleRecord{
		GenerationNumber:     generation,
		ContextManifest:      records.ContextManifest{},
		StreamedOutputState:  []records.StreamedOutput{},
		ProposedToolCalls:    []records.ProposedToolCall{},
		ObservedToolOutcomes: []records.ToolOutcome{},
		NextIntendedAction:   "resume imported legacy turn",
	}
	callStateChanging := make(map[string]bool)
	callKeys := make(map[string]string)
	for _, event := range events {
		if event.ConversationID != conversationID {
			return CanonicalSnapshot{}, fmt.Errorf("turnstate: legacy journal crossed conversation boundary")
		}
		if event.Attempt > 0 && uint64(event.Attempt) > generation {
			generation = uint64(event.Attempt)
			cycle.GenerationNumber = generation
		}
		switch event.Kind {
		case sessionjournal.KindUserMessage:
			objective = event.DisplayContent
			latestMessageID = fmt.Sprintf("legacy:%d", event.Sequence)
			state = records.StatePreparing
		case sessionjournal.KindAssistantMessage:
			status := records.StreamCommitted
			if event.Message.Partial {
				status = records.StreamProvisional
				state = records.StateRetryingGeneration
			} else {
				state = records.StateDelivered
				answerID := fmt.Sprintf("legacy-answer:%d", event.Sequence)
				deliveryID := fmt.Sprintf("legacy-delivery:%d", event.Sequence)
				snapshot.Answers[answerID] = records.AnswerRecord{
					GeneratedAnswer:      event.DisplayContent,
					CompletionAssessment: records.CompletionAssessment{Ready: true},
					StreamCommitState:    records.StreamCommitted,
				}
				snapshot.Deliveries[deliveryID] = records.DeliveryRecord{
					Channel:         "legacy-session-journal",
					Attempts:        []records.DeliveryAttempt{{Number: 1}},
					Acknowledgement: "imported-delivery",
				}
			}
			cycle.StreamedOutputState = append(cycle.StreamedOutputState, records.StreamedOutput{
				AttemptID: fmt.Sprintf("legacy:%d", event.Attempt), Sequence: uint64(event.Sequence),
				Channel: records.StreamAnswer, Status: status, Content: event.DisplayContent,
			})
		case sessionjournal.KindReasoning:
			cycle.StreamedOutputState = append(cycle.StreamedOutputState, records.StreamedOutput{
				AttemptID: fmt.Sprintf("legacy:%d", event.Attempt), Sequence: uint64(event.Sequence),
				Channel: records.StreamReasoning, Status: records.StreamCommitted, Content: event.Reasoning.Text,
			})
		case sessionjournal.KindToolCall:
			arguments := json.RawMessage(event.ToolCall.Arguments)
			if len(arguments) == 0 || !json.Valid(arguments) {
				arguments = json.RawMessage(`{}`)
			}
			cycle.ProposedToolCalls = append(cycle.ProposedToolCalls, records.ProposedToolCall{
				CallID: event.ToolCall.CallID, Operation: event.ToolCall.Name, NormalizedArguments: arguments,
			})
			callStateChanging[event.ToolCall.CallID] = event.ToolCall.StateChanging
			callKeys[event.ToolCall.CallID] = event.ToolCall.IdempotencyKey
			state = records.StateExecutingTools
		case sessionjournal.KindToolResult:
			outcome := records.ToolOutcome{FailureLayer: records.FailureNone, EffectStatus: records.EffectCompleted}
			if event.ToolResult.IsError {
				outcome.FailureLayer = records.FailureTool
				outcome.EffectStatus = records.EffectFailed
				outcome.NormalizedCause = event.ToolResult.FailureClass
			}
			cycle.ObservedToolOutcomes = append(cycle.ObservedToolOutcomes, outcome)
			class := records.SideEffectReadOnly
			if callStateChanging[event.ToolResult.CallID] {
				class = records.SideEffectNonIdempotentUnreconciliable
				if callKeys[event.ToolResult.CallID] != "" {
					class = records.SideEffectConditionallyIdempotent
				}
			}
			snapshot.Effects[event.ToolResult.CallID] = records.EffectRecord{
				Operation: event.ToolResult.Name, NormalizedArguments: json.RawMessage(`{}`),
				SideEffectClass: class, IdempotencyStrategy: callKeys[event.ToolResult.CallID],
				ReconciliationStrategy: "legacy-journal-result", EffectState: outcome.EffectStatus,
				Result: &outcome,
			}
			state = records.StateSynthesisOwed
		case sessionjournal.KindUncertainEffect:
			snapshot.Effects[event.UncertainEffect.CallID] = records.EffectRecord{
				Operation: event.UncertainEffect.ToolName, NormalizedArguments: json.RawMessage(`{}`),
				SideEffectClass:        records.SideEffectNonIdempotentReconciliable,
				IdempotencyStrategy:    event.UncertainEffect.IdempotencyKey,
				ReconciliationStrategy: "legacy-authoritative-reconciliation",
				EffectState:            records.EffectUnknown, UnknownEffectStatus: event.UncertainEffect.Status,
			}
			state = records.StateReconciliationRequired
		}
	}
	request, err := json.Marshal(events)
	if err != nil {
		return CanonicalSnapshot{}, fmt.Errorf("turnstate: encode legacy provider request: %w", err)
	}
	cycle.ProviderRequest = request
	snapshot.Cycles = []records.CycleRecord{cycle}
	debt := records.SynthesisDebt{}
	if state == records.StateSynthesisOwed {
		debt.Owed = true
		for callID := range snapshot.Effects {
			debt.UnconsumedEvidence = append(debt.UnconsumedEvidence, "legacy-tool-result:"+callID)
		}
	}
	snapshot.Turn = records.TurnRecord{
		LogicalTurnID: logicalTurnID, ConversationID: conversationID,
		RequestIdentity: requestIdentity, Objective: objective,
		LatestGenuineMessageID: latestMessageID, CurrentState: state,
		SynthesisDebt: debt,
	}
	if err := snapshot.Turn.Validate(); err != nil {
		return CanonicalSnapshot{}, err
	}
	return snapshot, nil
}

func ImportResurrection(state TurnState, effects map[string]EffectRecord) (CanonicalSnapshot, error) {
	if err := state.Validate(); err != nil {
		return CanonicalSnapshot{}, err
	}
	snapshot := CanonicalSnapshot{
		Effects: make(map[string]records.EffectRecord), Answers: make(map[string]records.AnswerRecord),
		Deliveries: make(map[string]records.DeliveryRecord),
	}
	canonicalState := resurrectionState(state)
	debt := records.SynthesisDebt{}
	turn := records.TurnRecord{
		LogicalTurnID: state.TurnID, ConversationID: state.SessionID,
		RequestIdentity: state.ActorID, Objective: state.Content,
		LatestGenuineMessageID: state.TurnID + ":request", CurrentState: canonicalState,
	}
	if state.Checkpoint != nil {
		encoded, err := json.Marshal(state.Checkpoint)
		if err != nil {
			return CanonicalSnapshot{}, err
		}
		cycle := records.CycleRecord{
			GenerationNumber: uint64(state.Checkpoint.Step), ProviderRequest: encoded,
			ContextManifest: records.ContextManifest{}, StreamedOutputState: []records.StreamedOutput{},
			ProposedToolCalls: []records.ProposedToolCall{}, ObservedToolOutcomes: []records.ToolOutcome{},
		}
		if state.Checkpoint.Coding != nil {
			cycle.NextIntendedAction = state.Checkpoint.Coding.NextAction
		}
		for index := range state.Checkpoint.ToolEvents {
			identity := fmt.Sprintf("resurrection-tool-result:%d", index)
			debt.UnconsumedEvidence = append(debt.UnconsumedEvidence, identity)
			cycle.ObservedToolOutcomes = append(cycle.ObservedToolOutcomes, records.ToolOutcome{
				FailureLayer: records.FailureNone, EffectStatus: records.EffectCompleted,
				Evidence: []records.EvidenceReference{{Identity: identity, Kind: "legacy-tool-event"}},
			})
		}
		debt.Owed = len(debt.UnconsumedEvidence) > 0
		if debt.Owed && canonicalState != records.StateReconciliationRequired {
			turn.CurrentState = records.StateSynthesisOwed
		}
		snapshot.Cycles = []records.CycleRecord{cycle}
		snapshot.Convergence.ProviderUsage = []records.ProviderUsage{{RequestCount: uint64(state.Checkpoint.ProviderAttempts)}}
	}
	turn.SynthesisDebt = debt
	turn.CumulativeBudgets.ProviderCalls = firstProviderCount(snapshot.Convergence.ProviderUsage)
	if err := turn.Validate(); err != nil {
		return CanonicalSnapshot{}, err
	}
	snapshot.Turn = turn
	for key, effect := range effects {
		status := records.EffectStarted
		if effect.Status == EffectCompleted {
			status = records.EffectCompleted
		}
		class := records.SideEffectNonIdempotentReconciliable
		if effect.RetrySafe {
			class = records.SideEffectConditionallyIdempotent
		}
		converted := records.EffectRecord{
			Operation: effect.ToolName, NormalizedArguments: json.RawMessage(`{}`),
			SideEffectClass: class, IdempotencyStrategy: effect.IdempotencyKey,
			ReconciliationStrategy: "resurrection-effect-state", EffectState: status,
		}
		if effect.Result != nil {
			outcome := records.ToolOutcome{FailureLayer: records.FailureNone, Retryable: effect.Result.Retryable, EffectStatus: status, NormalizedCause: effect.Result.FailureClass}
			if effect.Result.IsError {
				outcome.FailureLayer = records.FailureTool
			}
			converted.Result = &outcome
		}
		snapshot.Effects[key] = converted
	}
	return snapshot, nil
}

func resurrectionState(state TurnState) records.TurnState {
	if state.Checkpoint != nil && state.Checkpoint.PendingCall != nil {
		return records.StateReconciliationRequired
	}
	switch state.Status {
	case StatusRunning:
		return records.StateGenerating
	case StatusRecovering, StatusIncomplete, StatusInterrupted:
		return records.StateRetryingGeneration
	case StatusCompleted:
		return records.StateDelivered
	case StatusFailed, StatusCancelled:
		return records.StateBlockedAwaitingPerson
	default:
		return records.StateAccepted
	}
}

func firstProviderCount(usage []records.ProviderUsage) uint64 {
	if len(usage) == 0 {
		return 0
	}
	return usage[0].RequestCount
}
