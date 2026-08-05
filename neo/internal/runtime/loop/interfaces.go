// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"encoding/json"

	"matrix/cortexclient"
	"matrix/neo/internal/consolidation"
	"matrix/neo/internal/runtime/liveness"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/turnstate"
)

type Generator interface {
	Generate(
		context.Context,
		protocol.GenerationRequest,
		*provider.TurnUsage,
	) (protocol.NormalizedGeneration, error)
}

type StreamingGenerator interface {
	GenerateStream(
		context.Context,
		protocol.GenerationRequest,
		*provider.TurnUsage,
		func(protocol.StreamChunk) error,
	) (protocol.NormalizedGeneration, error)
}

type ToolResult struct {
	Content        json.RawMessage `json:"content"`
	IsError        bool            `json:"is_error"`
	FailureClass   string          `json:"failure_class,omitempty"`
	Retryable      bool            `json:"retryable,omitempty"`
	FailureMessage string          `json:"failure_message,omitempty"`
}

type ReconcileStatus string

const (
	ReconcileCompleted  ReconcileStatus = "completed"
	ReconcileRetrySafe  ReconcileStatus = "retry_safe"
	ReconcileNotStarted ReconcileStatus = "not_started"
	ReconcileUnknown    ReconcileStatus = "unknown"
)

type ReconcileResult struct {
	Status ReconcileStatus
	Result ToolResult
}

type ToolManager interface {
	Surface(context.Context) []protocol.ToolDefinition
	Execute(
		context.Context,
		protocol.NormalizedToolCall,
		string,
	) (ToolResult, error)
	Reconcile(context.Context, string) (ReconcileResult, error)
}

type CheckpointStore interface {
	SaveTurnCheckpoint(context.Context, string, turnstate.Checkpoint) error
	SetTurnRecovery(
		context.Context,
		string,
		turnstate.Status,
		turnstate.Recovery,
	) error
	SetTurnStatus(context.Context, string, turnstate.Status) error
}

// PendingEffectStore commits the recoverable PendingCall checkpoint and the
// durable effect-start record in one storage transaction.
type PendingEffectStore interface {
	SavePendingEffect(
		context.Context,
		string,
		turnstate.Checkpoint,
		string,
		string,
		json.RawMessage,
		bool,
	) error
}

type ActivationRequest struct {
	ConversationID string
	Query          string
	Premises       []string
}

type ActivationSource interface {
	Activate(context.Context, ActivationRequest) (string, error)
}

type PremiseSource interface {
	ActivePremises() []string
}

type GuidanceSource interface {
	DrainGuidance() []string
}

type TurnRecorder interface {
	RecordUser(string)
	RecordAssistant(protocol.Message)
	RecordTool(protocol.Message)
	RecordDelivery(string)
	ProvenanceRange() (string, uint64, uint64)
}

type DeliveryReporter interface {
	Say(string, bool)
}

type HonestPartialReporter interface {
	SayHonestPartial(string)
}

type Consolidator interface {
	Consolidate(consolidation.Job)
}

type IncompleteRecorder interface {
	RecordIncomplete(context.Context, IncompleteRecord)
}

type EvidenceJournal interface {
	CommitToolExecution(
		context.Context,
		ToolExecution,
	) (cortexclient.ToolEventCitation, error)
}

type EvidenceObserver interface {
	ObserveToolExecution(context.Context, ToolExecution) error
}

type SubgoalResolver interface {
	SubgoalFor(protocol.NormalizedToolCall) string
}

// LivenessSource supplies the measured epistemic counters the per-turn liveness
// policy is derived from. It is optional: with no source the loop derives from
// the zero context, which is the healthy baseline. It carries counters only —
// never premise text or memory content.
type LivenessSource interface {
	MeasuredContext() liveness.MeasuredContext
}

// DoubtController is the silent voice. The loop hands it every committed tool
// execution; when the controller decides the step warrants doubt it returns
// the first-person line to fold into the NEXT request-time clone. The line is
// never appended to durable state, so controller guidance cannot reach the
// durable transcript or the Neocortex transcript.
type DoubtController interface {
	ObserveMismatch(context.Context, int, ToolExecution) (string, bool)
}

type GenerationObserver interface {
	ContentDelta(context.Context, string) error
	ReasoningDelta(context.Context, string) error
	Reset(context.Context) error
	CommitAttempt(context.Context) error
}

type CompletionDecision struct {
	Ready      bool
	Stop       bool
	Reason     string
	NextAction string
}

type CompletionGate interface {
	CheckCompletion(context.Context) (CompletionDecision, error)
}
