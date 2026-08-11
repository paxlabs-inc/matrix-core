// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConformerAcceptsValidGeneration(t *testing.T) {
	pc := NewProviderConformer(DialectOpenAI)
	gen := &NormalizedGeneration{
		Content:      "Here is the result.",
		FinishReason: "stop",
	}
	if err := pc.Validate(gen); err != nil {
		t.Fatalf("valid generation should pass: %v", err)
	}
	if gen.Malformed {
		t.Fatal("should not be marked malformed")
	}
}

func TestConformerCatchesControlTokenInContent(t *testing.T) {
	pc := NewProviderConformer(DialectDeepSeek)
	gen := &NormalizedGeneration{
		Content:      "<think>Let me think...</think>Here is the answer.",
		FinishReason: "stop",
	}
	err := pc.Validate(gen)
	if err == nil {
		t.Fatal("control token in content should fail")
	}
	if !gen.Malformed {
		t.Fatal("should be marked malformed")
	}
}

func TestConformerCatchesEmptyToolCallName(t *testing.T) {
	pc := NewProviderConformer(DialectOpenAI)
	gen := &NormalizedGeneration{
		ToolCalls: []NormalizedToolCall{
			{ID: "call_1", Name: "", Arguments: json.RawMessage(`{}`)},
		},
		FinishReason: "tool_calls",
	}
	err := pc.Validate(gen)
	if err == nil {
		t.Fatal("empty tool call name should fail")
	}
	if !gen.Malformed {
		t.Fatal("should be marked malformed")
	}
}

func TestConformerCatchesInvalidJSONArguments(t *testing.T) {
	pc := NewProviderConformer(DialectOpenAI)
	gen := &NormalizedGeneration{
		ToolCalls: []NormalizedToolCall{
			{ID: "call_1", Name: "fs__read_file", Arguments: json.RawMessage(`{invalid}`)},
		},
		FinishReason: "tool_calls",
	}
	err := pc.Validate(gen)
	if err == nil {
		t.Fatal("invalid JSON arguments should fail")
	}
}

func TestConformerRejectsReasoningOnlyGeneration(t *testing.T) {
	pc := NewProviderConformer(DialectOpenAI)
	gen := &NormalizedGeneration{Reasoning: "internal only", FinishReason: "length"}
	if err := pc.Validate(gen); err == nil {
		t.Fatal("reasoning-only malformed generation must fail")
	}
	if !gen.Malformed {
		t.Fatal("reasoning-only generation must be marked malformed")
	}
}

func TestConformerRejectsMissingToolCallIDAndNonObjectArguments(t *testing.T) {
	pc := NewProviderConformer(DialectOpenAI)
	gen := &NormalizedGeneration{ToolCalls: []NormalizedToolCall{{Name: "fs__read_file", Arguments: json.RawMessage(`{}`)}}}
	if err := pc.Validate(gen); err == nil {
		t.Fatal("missing tool call id should fail")
	}
	gen = &NormalizedGeneration{ToolCalls: []NormalizedToolCall{{
		ID: "call-1", Name: "fs__read_file", Arguments: json.RawMessage(`["not","an","object"]`),
	}}}
	if err := pc.Validate(gen); err == nil {
		t.Fatal("non-object arguments should fail")
	}
}

func TestConformerAcceptsValidToolCall(t *testing.T) {
	pc := NewProviderConformer(DialectOpenAI)
	gen := &NormalizedGeneration{
		ToolCalls: []NormalizedToolCall{
			{ID: "call_1", Name: "fs__read_file", Arguments: json.RawMessage(`{"path":"/tmp/test"}`)},
		},
		FinishReason: "tool_calls",
	}
	if err := pc.Validate(gen); err != nil {
		t.Fatalf("valid tool call should pass: %v", err)
	}
}

func TestConformerStripsControlTokens(t *testing.T) {
	pc := NewProviderConformer(DialectDeepSeek)
	cleaned := pc.StripControlTokens("<think>internal reasoning</think>actual answer")
	// StripControlTokens removes the markers but not the content between them.
	if cleaned == "<think>internal reasoning</think>actual answer" {
		t.Fatal("markers were not stripped")
	}
	if !strings.Contains(cleaned, "actual answer") {
		t.Fatalf("expected content preserved, got: %q", cleaned)
	}
}

func TestBudgetCheckRejectsTooManyToolCalls(t *testing.T) {
	budget := RepresentationBudget{MaxTokens: 4096, MaxToolCalls: 2, MaxArgumentSize: 1024}
	gen := &NormalizedGeneration{
		ToolCalls: []NormalizedToolCall{
			{Name: "a", Arguments: json.RawMessage(`{}`)},
			{Name: "b", Arguments: json.RawMessage(`{}`)},
			{Name: "c", Arguments: json.RawMessage(`{}`)},
		},
	}
	if err := budget.BudgetCheck(gen); err == nil {
		t.Fatal("exceeding tool call budget should fail")
	}
}

func TestBudgetCheckRejectsOversizedArguments(t *testing.T) {
	budget := RepresentationBudget{MaxTokens: 4096, MaxToolCalls: 10, MaxArgumentSize: 10}
	gen := &NormalizedGeneration{
		ToolCalls: []NormalizedToolCall{
			{Name: "fs__write_file", Arguments: json.RawMessage(`{"content":"this is way more than ten bytes of content"}`)},
		},
	}
	if err := budget.BudgetCheck(gen); err == nil {
		t.Fatal("oversized arguments should fail budget check")
	}
}

func TestBudgetCheckEnforcesCompletionTokens(t *testing.T) {
	budget := RepresentationBudget{MaxTokens: 4, MaxToolCalls: 1, MaxArgumentSize: 32}
	gen := &NormalizedGeneration{Content: "too large", Usage: Usage{CompletionTokens: 5}}
	if err := budget.BudgetCheck(gen); err == nil {
		t.Fatal("completion token overflow should fail")
	}
}

func TestBudgetCheckPassesWithinLimits(t *testing.T) {
	budget := DefaultBudget()
	gen := &NormalizedGeneration{
		ToolCalls: []NormalizedToolCall{
			{Name: "fs__read_file", Arguments: json.RawMessage(`{"path":"/tmp"}`)},
		},
	}
	if err := budget.BudgetCheck(gen); err != nil {
		t.Fatalf("within-budget generation should pass: %v", err)
	}
}

func TestNilConformerSkipsValidation(t *testing.T) {
	var pc *ProviderConformer
	gen := &NormalizedGeneration{Content: "test", FinishReason: "stop"}
	if err := pc.Validate(gen); err != nil {
		t.Fatalf("nil conformer should not error: %v", err)
	}
}
