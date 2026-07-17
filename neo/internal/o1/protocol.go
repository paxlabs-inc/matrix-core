// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProviderDialect identifies a provider's wire format for tool calls,
// reasoning, streaming, and control syntax.
type ProviderDialect string

const (
	DialectOpenAI    ProviderDialect = "openai"
	DialectFireworks ProviderDialect = "fireworks"
	DialectMimo      ProviderDialect = "mimo"
	DialectDeepSeek  ProviderDialect = "deepseek"
	DialectXAI       ProviderDialect = "xai"
	DialectBaseten   ProviderDialect = "baseten"
	DialectGeneric   ProviderDialect = "generic"
)

// NormalizedToolCall is the provider-independent internal representation
// of a model-authored tool call. Per O1 req.4 ac.2: all supported forms
// normalize into one typed internal representation.
type NormalizedToolCall struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
	RawDialect ProviderDialect `json:"raw_dialect"`
}

// NormalizedGeneration is the provider-independent result of a model turn.
// Per O1 req.4 ac.1 and ac.2: streaming content, reasoning, structured calls,
// fragmented arguments, finish reasons, and usage are captured.
type NormalizedGeneration struct {
	Content         string               `json:"content"`
	Reasoning       string               `json:"reasoning,omitempty"`
	ToolCalls       []NormalizedToolCall `json:"tool_calls,omitempty"`
	FinishReason    string               `json:"finish_reason"`
	Usage           Usage                `json:"usage"`
	Dialect         ProviderDialect      `json:"dialect"`
	ControlTokens   []string             `json:"control_tokens,omitempty"`
	Malformed       bool                 `json:"malformed,omitempty"`
	MalformedReason string               `json:"malformed_reason,omitempty"`
}

// Usage captures token consumption per turn.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ProviderConformer validates and normalizes provider output.
// Per O1 req.4 ac.2: raw tool markup, guidance, control tokens, malformed
// partial calls, and reasoning-only calls SHALL never execute accidentally.
type ProviderConformer struct {
	dialect        ProviderDialect
	controlMarkers []string
}

// NewProviderConformer creates a conformer for the given dialect.
func NewProviderConformer(dialect ProviderDialect) *ProviderConformer {
	pc := &ProviderConformer{dialect: dialect}
	switch dialect {
	case DialectDeepSeek:
		pc.controlMarkers = []string{"<think>", "</think>", "<|tool_call|>", "<|/tool_call|>"}
	case DialectMimo:
		pc.controlMarkers = []string{"<think>", "</think>"}
	default:
		pc.controlMarkers = []string{"<think>", "</think>", "<|tool_call|>", "<|/tool_call|>"}
	}
	return pc
}

// Validate checks a NormalizedGeneration for structural integrity before
// execution. Per O1 req.4 ac.3: raw tool markup, guidance, control tokens,
// malformed partial calls, and reasoning-only calls are caught here.
func (pc *ProviderConformer) Validate(gen *NormalizedGeneration) error {
	if gen == nil {
		return fmt.Errorf("nil generation")
	}
	if pc == nil {
		return nil
	}
	// Check for control token leakage in content
	for _, marker := range pc.controlMarkers {
		if strings.Contains(gen.Content, marker) {
			gen.Malformed = true
			gen.MalformedReason = "control_token_in_content:" + marker
			return fmt.Errorf("provider control token leaked into content: %s", marker)
		}
	}
	// Check tool calls for structural integrity
	for i, tc := range gen.ToolCalls {
		if tc.ID == "" {
			gen.Malformed = true
			gen.MalformedReason = fmt.Sprintf("tool_call[%d]: empty id", i)
			return fmt.Errorf("tool call %d has empty id", i)
		}
		if tc.Name == "" {
			gen.Malformed = true
			gen.MalformedReason = fmt.Sprintf("tool_call[%d]: empty name", i)
			return fmt.Errorf("tool call %d has empty name", i)
		}
		if len(tc.Arguments) > 0 {
			var probe map[string]interface{}
			if err := json.Unmarshal(tc.Arguments, &probe); err != nil {
				gen.Malformed = true
				gen.MalformedReason = fmt.Sprintf("tool_call[%d]: invalid json arguments", i)
				return fmt.Errorf("tool call %d has invalid JSON arguments: %w", i, err)
			}
		}
	}
	// Check for reasoning-only calls (no content AND no tool calls)
	if strings.TrimSpace(gen.Content) == "" && len(gen.ToolCalls) == 0 && gen.FinishReason != "stop" {
		gen.Malformed = true
		gen.MalformedReason = "reasoning_only: no content or tool calls with finish_reason=" + gen.FinishReason
		return fmt.Errorf("provider returned reasoning-only generation with finish_reason=%s", gen.FinishReason)
	}
	return nil
}

// StripControlTokens removes provider-specific control syntax from content
// so it never reaches users or assistant history.
func (pc *ProviderConformer) StripControlTokens(content string) string {
	for _, marker := range pc.controlMarkers {
		content = strings.ReplaceAll(content, marker, "")
	}
	return strings.TrimSpace(content)
}

// RepresentationBudget estimates the token cost of representing an operation
// before generation. Per O1 req.4 ac.4: operations whose representation
// cannot fit the budget are decomposed deterministically.
type RepresentationBudget struct {
	MaxTokens       int `json:"max_tokens"`
	MaxToolCalls    int `json:"max_tool_calls"`
	MaxArgumentSize int `json:"max_argument_bytes"`
}

// DefaultBudget returns the default representation budget.
func DefaultBudget() RepresentationBudget {
	return RepresentationBudget{
		MaxTokens:       4096,
		MaxToolCalls:    10,
		MaxArgumentSize: 16 * 1024, // 16KB, matching file mutation limit
	}
}

// BudgetCheck verifies that a generation fits within the budget.
// Returns an error if the operation should be decomposed instead of generated.
func (b RepresentationBudget) BudgetCheck(gen *NormalizedGeneration) error {
	if gen == nil {
		return fmt.Errorf("nil generation")
	}
	if b.MaxTokens <= 0 || b.MaxToolCalls < 0 || b.MaxArgumentSize <= 0 {
		return fmt.Errorf("invalid representation budget")
	}
	if gen.Usage.CompletionTokens > b.MaxTokens {
		return fmt.Errorf("generation used %d completion tokens; budget allows %d — decompose deterministically",
			gen.Usage.CompletionTokens, b.MaxTokens)
	}
	if len(gen.ToolCalls) > b.MaxToolCalls {
		return fmt.Errorf("generation has %d tool calls; budget allows %d — decompose deterministically",
			len(gen.ToolCalls), b.MaxToolCalls)
	}
	for i, tc := range gen.ToolCalls {
		if len(tc.Arguments) > b.MaxArgumentSize {
			return fmt.Errorf("tool call %d (%s) arguments are %d bytes; budget allows %d — decompose deterministically",
				i, tc.Name, len(tc.Arguments), b.MaxArgumentSize)
		}
	}
	if gen.Usage.CompletionTokens == 0 {
		estimated := (len(gen.Content) + len(gen.Reasoning) + 3) / 4
		for _, tc := range gen.ToolCalls {
			estimated += (len(tc.Name) + len(tc.Arguments) + 3) / 4
		}
		if estimated > b.MaxTokens {
			return fmt.Errorf("generation representation is approximately %d tokens; budget allows %d — decompose deterministically",
				estimated, b.MaxTokens)
		}
	}
	return nil
}
