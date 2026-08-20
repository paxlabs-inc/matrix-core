// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package records

import (
	"encoding/json"
	"fmt"
	"strings"
)

type TurnState string

const (
	StateAccepted               TurnState = "Accepted"
	StatePreparing              TurnState = "Preparing"
	StateGenerating             TurnState = "Generating"
	StateAwaitingTools          TurnState = "AwaitingTools"
	StateExecutingTools         TurnState = "ExecutingTools"
	StateSynthesisOwed          TurnState = "SynthesisOwed"
	StateAnswerReady            TurnState = "AnswerReady"
	StateDelivering             TurnState = "Delivering"
	StateDelivered              TurnState = "Delivered"
	StateReconciliationRequired TurnState = "ReconciliationRequired"
	StateRetryingGeneration     TurnState = "RetryingGeneration"
	StateBlockedAwaitingPerson  TurnState = "BlockedAwaitingPerson"
	StateDeliveryRetry          TurnState = "DeliveryRetry"
)

func (state TurnState) Valid() bool {
	switch state {
	case StateAccepted, StatePreparing, StateGenerating, StateAwaitingTools,
		StateExecutingTools, StateSynthesisOwed, StateAnswerReady,
		StateDelivering, StateDelivered, StateReconciliationRequired,
		StateRetryingGeneration, StateBlockedAwaitingPerson,
		StateDeliveryRetry:
		return true
	default:
		return false
	}
}

type BudgetCounters struct {
	ProviderCalls           uint64 `json:"provider_calls"`
	ToolCalls               uint64 `json:"tool_calls"`
	InputTokens             uint64 `json:"input_tokens"`
	OutputTokens            uint64 `json:"output_tokens"`
	EpistemicRepairAttempts uint64 `json:"epistemic_repair_attempts"`
	CassandraO1Repairs      uint64 `json:"cassandra_o1_repair_attempts"`
	DeliveryAttempts        uint64 `json:"delivery_attempts"`
}

type SynthesisDebt struct {
	Owed               bool     `json:"owed"`
	UnconsumedEvidence []string `json:"unconsumed_evidence,omitempty"`
}

type TurnRecord struct {
	LogicalTurnID          string         `json:"logical_turn_id"`
	ConversationID         string         `json:"conversation_id"`
	RequestIdentity        string         `json:"request_identity"`
	Objective              string         `json:"objective"`
	LatestGenuineMessageID string         `json:"latest_genuine_message_id"`
	CurrentState           TurnState      `json:"current_state"`
	CumulativeBudgets      BudgetCounters `json:"cumulative_budgets"`
	SynthesisDebt          SynthesisDebt  `json:"synthesis_debt"`
	AnswerIdentity         string         `json:"answer_identity,omitempty"`
	DeliveryIdentity       string         `json:"delivery_identity,omitempty"`
}

func (record TurnRecord) Validate() error {
	if strings.TrimSpace(record.LogicalTurnID) == "" ||
		strings.TrimSpace(record.ConversationID) == "" ||
		strings.TrimSpace(record.RequestIdentity) == "" ||
		strings.TrimSpace(record.Objective) == "" ||
		strings.TrimSpace(record.LatestGenuineMessageID) == "" ||
		!record.CurrentState.Valid() {
		return fmt.Errorf("neo runtime records: invalid turn record")
	}
	if record.SynthesisDebt.Owed != (len(record.SynthesisDebt.UnconsumedEvidence) > 0) {
		return fmt.Errorf("neo runtime records: inconsistent synthesis debt")
	}
	return nil
}

type ContextManifestEntry struct {
	SourceNamespace  string `json:"source_namespace"`
	SourceID         string `json:"source_id"`
	ConversationID   string `json:"conversation_id,omitempty"`
	SemanticKind     string `json:"semantic_kind"`
	ContentHash      string `json:"content_hash"`
	RevisionIdentity string `json:"revision_identity,omitempty"`
	Included         bool   `json:"included"`
	Reason           string `json:"reason"`
}

type ContextManifest struct {
	Entries []ContextManifestEntry `json:"entries"`
}

type StreamChannel string

const (
	StreamReasoning StreamChannel = "reasoning"
	StreamAnswer    StreamChannel = "answer"
)

type StreamStatus string

const (
	StreamProvisional StreamStatus = "provisional"
	StreamCommitted   StreamStatus = "committed"
	StreamRetracted   StreamStatus = "retracted"
)

type StreamedOutput struct {
	AttemptID string        `json:"attempt_id"`
	Sequence  uint64        `json:"sequence"`
	Channel   StreamChannel `json:"channel"`
	Status    StreamStatus  `json:"status"`
	Content   string        `json:"content"`
}

type ProposedToolCall struct {
	CallID              string          `json:"call_id"`
	Operation           string          `json:"operation"`
	NormalizedArguments json.RawMessage `json:"normalized_arguments"`
}

type FailureLayer string

const (
	FailureNone           FailureLayer = "none"
	FailureValidation     FailureLayer = "validation"
	FailureDispatch       FailureLayer = "dispatch"
	FailureTool           FailureLayer = "tool"
	FailureProvider       FailureLayer = "provider"
	FailureReconciliation FailureLayer = "reconciliation"
	FailureDelivery       FailureLayer = "delivery"
	FailureInfrastructure FailureLayer = "infrastructure"
)

type EffectStatus string

const (
	EffectNotStarted EffectStatus = "not_started"
	EffectStarted    EffectStatus = "started"
	EffectCompleted  EffectStatus = "completed"
	EffectFailed     EffectStatus = "failed"
	EffectUnknown    EffectStatus = "unknown"
)

type EvidenceReference struct {
	Identity string          `json:"identity"`
	Kind     string          `json:"kind"`
	Content  json.RawMessage `json:"content,omitempty"`
}

type ToolOutcome struct {
	FailureLayer       FailureLayer        `json:"failure_layer"`
	Retryable          bool                `json:"retryable"`
	EffectStatus       EffectStatus        `json:"effect_status"`
	Evidence           []EvidenceReference `json:"evidence,omitempty"`
	NormalizedCause    string              `json:"normalized_cause,omitempty"`
	SuggestedRecovery  string              `json:"suggested_recovery,omitempty"`
	ArtifactReferences []string            `json:"artifact_references,omitempty"`
}

type CycleRecord struct {
	GenerationNumber     uint64             `json:"generation_number"`
	ContextManifest      ContextManifest    `json:"context_manifest"`
	ProviderRequest      json.RawMessage    `json:"provider_request"`
	StreamedOutputState  []StreamedOutput   `json:"streamed_output_state"`
	ProposedToolCalls    []ProposedToolCall `json:"proposed_tool_calls"`
	ObservedToolOutcomes []ToolOutcome      `json:"observed_tool_outcomes"`
	NextIntendedAction   string             `json:"next_intended_action"`
}

type SideEffectClass string

const (
	SideEffectReadOnly                     SideEffectClass = "read-only"
	SideEffectIdempotentWrite              SideEffectClass = "idempotent write"
	SideEffectConditionallyIdempotent      SideEffectClass = "conditionally idempotent"
	SideEffectNonIdempotentReconciliable   SideEffectClass = "non-idempotent reconcilable"
	SideEffectNonIdempotentUnreconciliable SideEffectClass = "non-idempotent unreconcilable"
)

type EffectRecord struct {
	Operation                     string              `json:"operation"`
	NormalizedArguments           json.RawMessage     `json:"normalized_arguments"`
	SideEffectClass               SideEffectClass     `json:"side_effect_class"`
	IdempotencyStrategy           string              `json:"idempotency_strategy"`
	ReconciliationStrategy        string              `json:"reconciliation_strategy"`
	EffectState                   EffectStatus        `json:"effect_state"`
	AuthoritativeDispatchEvidence []EvidenceReference `json:"authoritative_dispatch_evidence,omitempty"`
	Result                        *ToolOutcome        `json:"result,omitempty"`
	UnknownEffectStatus           string              `json:"unknown_effect_status,omitempty"`
}

type FailureFingerprint struct {
	Operation           string          `json:"operation"`
	NormalizedArguments json.RawMessage `json:"normalized_arguments"`
	Phase               string          `json:"phase"`
	FailureLayer        FailureLayer    `json:"failure_layer"`
	NormalizedCause     string          `json:"normalized_cause"`
	EffectStatus        EffectStatus    `json:"effect_status"`
}

type AttemptCount struct {
	Fingerprint FailureFingerprint `json:"fingerprint"`
	Count       uint64             `json:"count"`
}

type ProviderUsage struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	RequestCount uint64 `json:"request_count"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

type ConvergenceRecord struct {
	FailureFingerprints    []FailureFingerprint `json:"failure_fingerprints"`
	AttemptCounts          []AttemptCount       `json:"attempt_counts"`
	StrategyChanges        []string             `json:"strategy_changes"`
	ProviderUsage          []ProviderUsage      `json:"provider_usage"`
	CumulativeInputTokens  uint64               `json:"cumulative_input_tokens"`
	CumulativeOutputTokens uint64               `json:"cumulative_output_tokens"`
	ToolCalls              uint64               `json:"tool_calls"`
	RepairCounters         map[string]uint64    `json:"repair_counters"`
	Tuning                 map[string]uint64    `json:"tuning,omitempty"`
}

type CompletionAssessment struct {
	Ready      bool   `json:"ready"`
	Blocked    bool   `json:"blocked"`
	Reason     string `json:"reason"`
	NextAction string `json:"next_action,omitempty"`
}

type AnswerRecord struct {
	GeneratedAnswer       string               `json:"generated_answer"`
	SupportingEvidenceIDs []string             `json:"supporting_evidence_ids"`
	CompletionAssessment  CompletionAssessment `json:"completion_assessment"`
	StreamCommitState     StreamStatus         `json:"stream_commit_state"`
}

type DeliveryAttempt struct {
	Number uint64 `json:"number"`
	Error  string `json:"error,omitempty"`
}

type DeliveryRecord struct {
	Channel           string            `json:"channel"`
	Attempts          []DeliveryAttempt `json:"attempts"`
	Acknowledgement   string            `json:"acknowledgement,omitempty"`
	LastDeliveryError string            `json:"last_delivery_error,omitempty"`
}
