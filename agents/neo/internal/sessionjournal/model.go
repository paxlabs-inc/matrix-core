// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package sessionjournal

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Kind string

const (
	KindUserMessage      Kind = "user_message"
	KindAssistantMessage Kind = "assistant_message"
	KindToolCall         Kind = "tool_call"
	KindToolResult       Kind = "tool_result"
	KindReasoning        Kind = "reasoning"
	KindProviderReplay   Kind = "provider_replay"
	KindApproval         Kind = "approval"
	KindArtifact         Kind = "artifact"
	KindUncertainEffect  Kind = "uncertain_effect"
	KindSupervisor       Kind = "supervisor"
	KindRecovery         Kind = "recovery"
)

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

var (
	ErrClosed       = errors.New("session journal closed")
	ErrInvalidEvent = errors.New("invalid session journal event")
)

type Event struct {
	ConversationID string    `json:"conversation_id"`
	Sequence       int64     `json:"sequence"`
	TurnID         string    `json:"turn_id,omitempty"`
	Attempt        int       `json:"attempt,omitempty"`
	Kind           Kind      `json:"kind"`
	CreatedAt      time.Time `json:"created_at"`
	DisplayContent string    `json:"display_content,omitempty"`
	APIContent     []byte    `json:"api_content,omitempty"`

	Message         *Message         `json:"message,omitempty"`
	ToolCall        *ToolCall        `json:"tool_call,omitempty"`
	ToolResult      *ToolResult      `json:"tool_result,omitempty"`
	Reasoning       *Reasoning       `json:"reasoning,omitempty"`
	ProviderReplay  *ProviderReplay  `json:"provider_replay,omitempty"`
	Approval        *Approval        `json:"approval,omitempty"`
	Artifact        *Artifact        `json:"artifact,omitempty"`
	UncertainEffect *UncertainEffect `json:"uncertain_effect,omitempty"`
	Supervisor      *Supervisor      `json:"supervisor,omitempty"`
	Recovery        *Recovery        `json:"recovery,omitempty"`
}

type Message struct {
	Role             string     `json:"role"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ProviderMetadata []byte     `json:"provider_metadata,omitempty"`
	Partial          bool       `json:"partial,omitempty"`
}

type ToolCall struct {
	CallID         string `json:"call_id"`
	Type           string `json:"type,omitempty"`
	Name           string `json:"name"`
	Arguments      []byte `json:"arguments,omitempty"`
	StateChanging  bool   `json:"state_changing,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type ToolResult struct {
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Result           []byte `json:"result,omitempty"`
	IsError          bool   `json:"is_error,omitempty"`
	FailureClass     string `json:"failure_class,omitempty"`
	ProviderMetadata []byte `json:"provider_metadata,omitempty"`
}

type Reasoning struct {
	Text             string `json:"text,omitempty"`
	ProviderMetadata []byte `json:"provider_metadata,omitempty"`
}

type ProviderReplay struct {
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	ResponseID    string `json:"response_id,omitempty"`
	RequestBytes  []byte `json:"request_bytes"`
	ResponseBytes []byte `json:"response_bytes,omitempty"`
	Metadata      []byte `json:"metadata,omitempty"`
}

type Approval struct {
	ApprovalID string `json:"approval_id"`
	Action     string `json:"action"`
	Decision   string `json:"decision"`
	Authority  string `json:"authority,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type Artifact struct {
	ArtifactID string `json:"artifact_id"`
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	URI        string `json:"uri,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

type UncertainEffect struct {
	CallID         string `json:"call_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	ToolName       string `json:"tool_name"`
	Status         string `json:"status"`
	Evidence       []byte `json:"evidence,omitempty"`
}

type Supervisor struct {
	IntentID string `json:"intent_id,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
	Action   string `json:"action"`
	Reason   string `json:"reason,omitempty"`
}

type Recovery struct {
	State             string `json:"state"`
	Reason            string `json:"reason"`
	FinalizesSequence int64  `json:"finalizes_sequence,omitempty"`
	PendingCallID     string `json:"pending_call_id,omitempty"`
	Checkpoint        []byte `json:"checkpoint,omitempty"`
}

type TailCondition string

const (
	TailClean            TailCondition = "clean"
	TailPendingToolCall  TailCondition = "pending_tool_call"
	TailPartialAssistant TailCondition = "partial_assistant"
)

type TailState struct {
	Condition TailCondition
	Event     Event
}

func (state TailState) Abnormal() bool { return state.Condition != TailClean }

func (event Event) Validate() error {
	if strings.TrimSpace(event.ConversationID) == "" {
		return fmt.Errorf("%w: conversation ID is required", ErrInvalidEvent)
	}
	if event.Sequence < 0 {
		return fmt.Errorf("%w: sequence cannot be negative", ErrInvalidEvent)
	}
	if event.Attempt < 0 {
		return fmt.Errorf("%w: attempt cannot be negative", ErrInvalidEvent)
	}
	payloads := 0
	for _, present := range []bool{
		event.Message != nil, event.ToolCall != nil, event.ToolResult != nil,
		event.Reasoning != nil, event.ProviderReplay != nil,
		event.Approval != nil, event.Artifact != nil,
		event.UncertainEffect != nil, event.Supervisor != nil,
		event.Recovery != nil,
	} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return fmt.Errorf("%w: exactly one typed payload is required", ErrInvalidEvent)
	}
	var err error
	switch event.Kind {
	case KindUserMessage:
		err = validateMessage(event.Message, RoleUser)
	case KindAssistantMessage:
		err = validateMessage(event.Message, RoleAssistant)
	case KindToolCall:
		err = validateToolCall(event.ToolCall)
	case KindToolResult:
		err = validateToolResult(event.ToolResult)
	case KindReasoning:
		if event.Reasoning == nil || (strings.TrimSpace(event.Reasoning.Text) == "" && len(event.Reasoning.ProviderMetadata) == 0) {
			err = errors.New("reasoning payload is empty")
		}
	case KindProviderReplay:
		if event.ProviderReplay == nil || len(event.ProviderReplay.RequestBytes) == 0 {
			err = errors.New("provider replay request bytes are required")
		}
	case KindApproval:
		if event.Approval == nil || strings.TrimSpace(event.Approval.ApprovalID) == "" || strings.TrimSpace(event.Approval.Action) == "" || strings.TrimSpace(event.Approval.Decision) == "" {
			err = errors.New("approval ID, action, and decision are required")
		}
	case KindArtifact:
		if event.Artifact == nil || strings.TrimSpace(event.Artifact.ArtifactID) == "" || strings.TrimSpace(event.Artifact.Kind) == "" || (strings.TrimSpace(event.Artifact.Path) == "" && strings.TrimSpace(event.Artifact.URI) == "") {
			err = errors.New("artifact ID, kind, and location are required")
		}
	case KindUncertainEffect:
		if event.UncertainEffect == nil || strings.TrimSpace(event.UncertainEffect.CallID) == "" || strings.TrimSpace(event.UncertainEffect.ToolName) == "" || strings.TrimSpace(event.UncertainEffect.Status) == "" {
			err = errors.New("uncertain effect call ID, tool name, and status are required")
		}
	case KindSupervisor:
		if event.Supervisor == nil || strings.TrimSpace(event.Supervisor.Action) == "" {
			err = errors.New("supervisor action is required")
		}
	case KindRecovery:
		if event.Recovery == nil || strings.TrimSpace(event.Recovery.State) == "" || strings.TrimSpace(event.Recovery.Reason) == "" {
			err = errors.New("recovery state and reason are required")
		}
	default:
		err = fmt.Errorf("unknown event kind %q", event.Kind)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	return nil
}

func validateMessage(message *Message, role string) error {
	if message == nil || message.Role != role {
		return fmt.Errorf("%s message payload is required", role)
	}
	for index := range message.ToolCalls {
		if err := validateToolCall(&message.ToolCalls[index]); err != nil {
			return err
		}
	}
	return nil
}

func validateToolCall(call *ToolCall) error {
	if call == nil || strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" {
		return errors.New("tool call ID and name are required")
	}
	return nil
}

func validateToolResult(result *ToolResult) error {
	if result == nil || strings.TrimSpace(result.CallID) == "" || strings.TrimSpace(result.Name) == "" {
		return errors.New("tool result call ID and name are required")
	}
	return nil
}

func (event Event) EqualProviderContent(other Event) bool {
	return bytes.Equal(event.APIContent, other.APIContent)
}
