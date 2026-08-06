// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"context"
	"encoding/json"

	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/records"
)

type ContextRequest struct {
	Turn       records.TurnRecord
	Messages   []protocol.Message
	ToolSchema []protocol.ToolDefinition
}

type ComposedContext struct {
	Request  protocol.GenerationRequest
	Manifest records.ContextManifest
}

type ContextComposer interface {
	Compose(context.Context, ContextRequest) (ComposedContext, error)
}

type ProviderGenerator interface {
	Generate(context.Context, protocol.GenerationRequest, *provider.TurnUsage) (protocol.NormalizedGeneration, error)
}

type ProviderStreamer interface {
	GenerateStream(context.Context, protocol.GenerationRequest, *provider.TurnUsage, func(protocol.StreamChunk) error) (protocol.NormalizedGeneration, error)
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

type ToolDispatcher interface {
	Surface(context.Context) []protocol.ToolDefinition
	Execute(context.Context, protocol.NormalizedToolCall, string) (ToolResult, error)
	Reconcile(context.Context, string) (ReconcileResult, error)
}

type MemoryRequest struct {
	ConversationID string
	Query          string
	Premises       []string
}

type MemoryRetriever interface {
	Activate(context.Context, MemoryRequest) (string, error)
}

type MemoryProjection struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	Title           string  `json:"title"`
	Summary         string  `json:"summary"`
	SourceType      string  `json:"sourceType"`
	ConversationID  string  `json:"conversationID"`
	OccurredAt      string  `json:"occurredAt"`
	Confidence      float64 `json:"confidence"`
	RelevanceReason string  `json:"relevanceReason"`
	Provenance      string  `json:"provenance"`
}

type MemoryRenderer interface {
	RenderMemory(context.Context, json.RawMessage) (MemoryProjection, error)
}

type RetryRequest struct {
	Turn        records.TurnRecord
	Convergence records.ConvergenceRecord
	Failure     records.FailureFingerprint
}

type RetryDecision struct {
	Retry          bool
	StrategyChange bool
	Reconcile      bool
	Block          bool
	Reason         string
}

type RetryDecider interface {
	DecideRetry(context.Context, RetryRequest) (RetryDecision, error)
}

type CompletionDecision struct {
	Ready      bool
	Stop       bool
	Reason     string
	NextAction string
}

type AnswerCompleter interface {
	CheckCompletion(context.Context) (CompletionDecision, error)
}

type DeliveryReporter interface {
	Say(string, bool)
}

type HonestPartialReporter interface {
	SayHonestPartial(string)
}

type AnswerDeliverer interface {
	Deliver(context.Context, records.AnswerRecord, records.DeliveryRecord) (records.DeliveryRecord, error)
}

type RecoveryRequest struct {
	Turn        records.TurnRecord
	Convergence records.ConvergenceRecord
	Effects     []records.EffectRecord
}

type ProcessRecovery interface {
	Resume(context.Context, RecoveryRequest) (records.TurnRecord, error)
}

type GenerationObserver interface {
	ContentDelta(context.Context, string) error
	ReasoningDelta(context.Context, string) error
	Reset(context.Context) error
	CommitAttempt(context.Context) error
}
