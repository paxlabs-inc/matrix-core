// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"
	FinishError     = "error"
)

type Message struct {
	Role       MessageRole          `json:"role"`
	Content    string               `json:"content"`
	Reasoning  string               `json:"reasoning,omitempty"`
	Name       string               `json:"name,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	ToolCalls  []NormalizedToolCall `json:"tool_calls,omitempty"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type GenerationRequest struct {
	Model           string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Stream          bool             `json:"stream"`
}

type NormalizedGeneration struct {
	Content      string               `json:"content"`
	Reasoning    string               `json:"reasoning"`
	ToolCalls    []NormalizedToolCall `json:"tool_calls"`
	FinishReason string               `json:"finish_reason"`
	Usage        TokenUsage           `json:"usage"`
	Provider     string               `json:"provider"`
	Model        string               `json:"model"`
	Duration     time.Duration        `json:"duration"`
}

type NormalizedToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type TokenUsage struct {
	PromptTokens            int   `json:"prompt_tokens"`
	CompletionTokens        int   `json:"completion_tokens"`
	TotalTokens             int   `json:"total_tokens"`
	ReasoningTokens         int   `json:"reasoning_tokens"`
	CachedTokens            int   `json:"cached_tokens"`
	ModelCostMicrocents     int64 `json:"model_cost_microcents"`
	ProviderSpendMicrocents int64 `json:"provider_spend_microcents"`
	ModelCostKnown          bool  `json:"model_cost_known"`
	ProviderSpendKnown      bool  `json:"provider_spend_known"`
}

type StreamChunk struct {
	ContentDelta   string                `json:"content_delta"`
	ReasoningDelta string                `json:"reasoning_delta"`
	ToolCall       *NormalizedToolCall   `json:"tool_call,omitempty"`
	ToolCallDeltas []StreamToolCallDelta `json:"tool_call_deltas,omitempty"`
	FinishReason   string                `json:"finish_reason,omitempty"`
	Usage          *TokenUsage           `json:"usage,omitempty"`
	Done           bool                  `json:"done"`
}

type StreamToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func (request GenerationRequest) Validate() error {
	if strings.TrimSpace(request.Model) == "" {
		return fmt.Errorf("runtime protocol: model is required")
	}
	if len(request.Messages) == 0 {
		return fmt.Errorf("runtime protocol: at least one message is required")
	}
	for index, message := range request.Messages {
		if !message.Role.Valid() {
			return fmt.Errorf("runtime protocol: message %d has invalid role", index)
		}
		if message.Role == RoleTool && strings.TrimSpace(message.ToolCallID) == "" {
			return fmt.Errorf("runtime protocol: tool message %d requires tool call ID", index)
		}
	}
	for index, tool := range request.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("runtime protocol: tool %d name is required", index)
		}
		if !validJSONObject(tool.Parameters) {
			return fmt.Errorf("runtime protocol: tool %q parameters must be a JSON object", tool.Name)
		}
	}
	if request.MaxOutputTokens < 0 {
		return fmt.Errorf("runtime protocol: max output tokens cannot be negative")
	}
	return nil
}

func (generation NormalizedGeneration) Validate() error {
	switch generation.FinishReason {
	case FinishStop, FinishToolCalls, FinishLength, FinishError:
	default:
		return fmt.Errorf("runtime protocol: invalid finish reason %q", generation.FinishReason)
	}
	if err := generation.Usage.Validate(); err != nil {
		return err
	}
	for index, call := range generation.ToolCalls {
		if err := call.Validate(); err != nil {
			return fmt.Errorf("runtime protocol: tool call %d: %w", index, err)
		}
	}
	return nil
}

func (call NormalizedToolCall) Validate() error {
	if strings.TrimSpace(call.ID) == "" {
		return fmt.Errorf("tool call ID is required")
	}
	if strings.TrimSpace(call.Name) == "" {
		return fmt.Errorf("tool call name is required")
	}
	if !validJSONObject(call.Arguments) {
		return fmt.Errorf("tool call arguments must be a JSON object")
	}
	return nil
}

func (usage TokenUsage) Validate() error {
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 ||
		usage.TotalTokens < 0 || usage.ReasoningTokens < 0 ||
		usage.CachedTokens < 0 || usage.ModelCostMicrocents < 0 ||
		usage.ProviderSpendMicrocents < 0 {
		return fmt.Errorf("runtime protocol: token usage cannot be negative")
	}
	if !usage.ModelCostKnown && usage.ModelCostMicrocents != 0 ||
		!usage.ProviderSpendKnown && usage.ProviderSpendMicrocents != 0 {
		return fmt.Errorf("runtime protocol: token cost requires an authoritative tariff")
	}
	if usage.TotalTokens != 0 &&
		usage.TotalTokens < usage.PromptTokens+usage.CompletionTokens {
		return fmt.Errorf("runtime protocol: total tokens is smaller than prompt plus completion")
	}
	return nil
}

func (role MessageRole) Valid() bool {
	switch role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

func validJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) >= 2 && trimmed[0] == '{' &&
		trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}
