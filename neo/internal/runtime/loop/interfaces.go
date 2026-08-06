// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"encoding/json"

	"matrix/cortexclient"
	"matrix/neo/internal/consolidation"
	runtimecontract "matrix/neo/internal/runtime"
	"matrix/neo/internal/runtime/liveness"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/records"
	"matrix/neo/internal/runtime/turnstate"
)

type Generator = runtimecontract.ProviderGenerator
type StreamingGenerator = runtimecontract.ProviderStreamer
type ToolResult = runtimecontract.ToolResult
type ReconcileStatus = runtimecontract.ReconcileStatus

const (
	ReconcileCompleted  = runtimecontract.ReconcileCompleted
	ReconcileRetrySafe  = runtimecontract.ReconcileRetrySafe
	ReconcileNotStarted = runtimecontract.ReconcileNotStarted
	ReconcileUnknown    = runtimecontract.ReconcileUnknown
)

type ReconcileResult = runtimecontract.ReconcileResult
type ToolManager = runtimecontract.ToolDispatcher

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

type ContextManifestStore interface {
	SaveContextManifest(context.Context, string, uint64, records.ContextManifest) error
}

type AnswerStateStore interface {
	SaveAnswerRecord(context.Context, string, string, records.AnswerRecord) error
}

type DeliveryStateStore interface {
	AnswerStateStore
	LoadAnswerRecord(context.Context, string, string) (records.AnswerRecord, error)
	SaveDeliveryRecord(context.Context, string, string, records.DeliveryRecord) error
	LoadDeliveryRecord(context.Context, string, string) (records.DeliveryRecord, error)
	MarkAnswerReady(context.Context, string, string) error
	MarkDelivering(context.Context, string, string) error
	MarkDeliveryRetry(context.Context, string) error
	MarkDelivered(context.Context, string) error
}

type ConvergenceStore interface {
	SaveConvergenceRecord(context.Context, string, records.ConvergenceRecord) error
	LoadConvergenceRecord(context.Context, string) (records.ConvergenceRecord, error)
}

type SynthesisDebtStore interface {
	LoadTurnRecord(context.Context, string) (records.TurnRecord, error)
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

type ActivationRequest = runtimecontract.MemoryRequest
type ActivationSource = runtimecontract.MemoryRetriever

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

type DeliveryReporter = runtimecontract.DeliveryReporter
type HonestPartialReporter = runtimecontract.HonestPartialReporter

type ReliableDeliveryReporter interface {
	SayResult(string, bool) error
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

type GenerationObserver = runtimecontract.GenerationObserver
type CompletionDecision = runtimecontract.CompletionDecision
type CompletionGate = runtimecontract.AnswerCompleter
