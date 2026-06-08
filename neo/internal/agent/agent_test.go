// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
)

func tc(name, args string) llm.ToolCall {
	return llm.ToolCall{ID: name, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
}

func TestBatchSignatureOrderIndependent(t *testing.T) {
	a := []llm.ToolCall{tc("read", `{"p":1}`), tc("write", `{"p":2}`)}
	b := []llm.ToolCall{tc("write", `{"p":2}`), tc("read", `{"p":1}`)}
	if batchSignature(a) != batchSignature(b) {
		t.Error("signature must be order-independent (sorted)")
	}
	c := []llm.ToolCall{tc("read", `{"p":9}`), tc("write", `{"p":2}`)}
	if batchSignature(a) == batchSignature(c) {
		t.Error("different args must yield a different signature")
	}
}

func TestSafeTailKeepsFromLastUser(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("first"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc("f", "{}")}},
		llm.ToolResult("f", "f", "r1"),
		llm.UserMessage("second"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc("g", "{}")}},
		llm.ToolResult("g", "g", "r2"),
	}
	tail := safeTail(msgs)
	if len(tail) == 0 || tail[0].Role != llm.RoleUser || tail[0].Content != "second" {
		t.Fatalf("safeTail should start at the last user msg, got %+v", tail)
	}
	// a tool-result must never be left without its preceding assistant call.
	if tail[0].Role == llm.RoleTool {
		t.Error("tail must not start on a tool result")
	}
}

func TestRenderTranscript(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("do x"),
		{Role: llm.RoleAssistant, Content: "working", ToolCalls: []llm.ToolCall{tc("read", `{"p":"/a"}`)}},
		llm.ToolResult("read", "read", "file body"),
	}
	got := renderTranscript(msgs)
	for _, want := range []string{"USER: do x", "ASSISTANT: working", "ASSISTANT→tool read", "TOOL read: file body"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q:\n%s", want, got)
		}
	}
}

func TestBudgetPct(t *testing.T) {
	cfg := config.Default()
	cfg.ContextWindowTokens = 1000
	a := New(Options{Config: cfg})

	small := a.budgetPct("short system")
	a.working = []llm.Message{llm.UserMessage(strings.Repeat("token ", 800))}
	big := a.budgetPct("short system")
	if big <= small {
		t.Errorf("more transcript should raise budget pct: small=%d big=%d", small, big)
	}
	if big < 0 || big > 100 {
		t.Errorf("pct out of range: %d", big)
	}

	cfg.ContextWindowTokens = 0
	a2 := New(Options{Config: cfg})
	if a2.budgetPct("anything") != 0 {
		t.Error("zero window must yield 0 pct (no divide-by-zero)")
	}
}

func TestEstimateTokenHelpers(t *testing.T) {
	if estimateToolTokens(nil) != 0 {
		t.Error("nil schemas -> 0 tokens")
	}
	if estimateToolTokens([]llm.Tool{llm.NewFunctionTool("f", "d", nil)}) <= 0 {
		t.Error("a real schema should estimate > 0 tokens")
	}
	if estimateMessagesTokens([]llm.Message{llm.UserMessage("hello there")}) <= 0 {
		t.Error("a message should estimate > 0 tokens")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("under-limit string must be unchanged")
	}
	got := truncate("abcdefghij", 4)
	if !strings.HasSuffix(got, "…") || len(got) > len("abcd")+len("…") {
		t.Errorf("truncate wrong: %q", got)
	}
}
