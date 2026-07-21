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

func TestSystemPromptHasPaxeerGrounding(t *testing.T) {
	a := New(Options{Config: config.Default()})
	sp := a.systemPrompt()
	// The grounding must be present so Neo knows Paxeer is real and where to
	// reach it — instead of denying it exists or blind web-searching.
	for _, want := range []string{
		"Paxeer", "125", "api.hyperpax.xyz", "paxscan.io", "browser", "MachineMail", "pending_approval", "self:",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing grounding fact %q", want)
		}
	}
}

func TestSeedPrimesTranscriptOnce(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.Seed([]llm.Message{llm.UserMessage("hi"), llm.AssistantMessage("hello")}, "hi")
	if len(a.working) != 2 || a.activeGoal != "hi" {
		t.Fatalf("seed should prime 2 turns + goal, got %d turns goal=%q", len(a.working), a.activeGoal)
	}
	// A second seed must not clobber an in-flight transcript.
	a.Seed([]llm.Message{llm.UserMessage("other")}, "other")
	if len(a.working) != 2 || a.activeGoal != "hi" {
		t.Error("seed must be a no-op once the transcript has content")
	}
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

func TestCapToolResult(t *testing.T) {
	a := New(Options{Config: config.Default()})
	defer func() {
		if a.turn.overflow != nil {
			a.turn.overflow.cleanup()
		}
	}()

	small := strings.Repeat("x", 100)
	if a.capToolResult(small) != small {
		t.Error("under-limit tool result must be unchanged")
	}

	// An oversized result is NOT silently cut (neo-smoothness req.4.1): the FULL
	// output is spilled to a run-scoped overflow file and the transcript keeps a
	// bounded head+tail plus a truncation notice carrying the overflow token.
	big := strings.Repeat("y", maxToolResultChars*3)
	got := a.capToolResult(big)
	if len(got) >= len(big) {
		t.Errorf("oversized result not bounded: got %d of %d", len(got), len(big))
	}
	if !strings.Contains(got, "truncation_notice") || !strings.Contains(got, "read_overflow") {
		t.Errorf("overflow notice missing from inline result: %q", got)
	}
	if !strings.HasPrefix(got, "y") || !strings.HasSuffix(got, "y") {
		t.Error("head and tail of the result must be preserved")
	}
	// The notice must register an unread overflow (the read-full gate).
	if !a.overflowUnread() {
		t.Fatal("oversized result must register an unread overflow")
	}
	// And the FULL original must be retrievable from the overflow store.
	tokens := a.turn.overflow.unread()
	if len(tokens) != 1 {
		t.Fatalf("want exactly one overflow token, got %v", tokens)
	}
	chunk, total, _, ok := a.turn.overflow.read(tokens[0], 0, len(big))
	if !ok || total != len(big) || chunk != big {
		t.Errorf("overflow file must hold the FULL output verbatim (ok=%v total=%d want=%d)", ok, total, len(big))
	}
	// Reading it satisfies the read-full latch.
	if a.overflowUnread() {
		t.Error("overflow must be marked read after read_overflow")
	}
}

func TestCapToolResultPlainFallback(t *testing.T) {
	small := strings.Repeat("x", 100)
	if capToolResultPlain(small) != small {
		t.Error("under-limit tool result must be unchanged")
	}
	big := strings.Repeat("y", maxToolResultChars*3)
	got := capToolResultPlain(big)
	if len(got) >= len(big) || !strings.Contains(got, "tool result truncated") {
		t.Errorf("plain fallback must bound + mark: %d of %d", len(got), len(big))
	}
}

func TestWindowBytes(t *testing.T) {
	a := New(Options{Config: config.Default()})
	empty := a.windowBytes("sys")
	a.working = []llm.Message{
		llm.UserMessage(strings.Repeat("a", 5000)),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc("f", strings.Repeat("b", 5000))}},
	}
	full := a.windowBytes("sys")
	// Byte proxy must grow with transcript content + tool-call argument bytes.
	if full <= empty+10000 {
		t.Errorf("windowBytes should count content + tool args: empty=%d full=%d", empty, full)
	}
}

// TestAgentGenerationSetGet verifies the generation tag (P2-6 lane-based
// concurrency): SetGeneration stores a value, Generation retrieves it, and the
// default is zero (unset).
func TestAgentGenerationSetGet(t *testing.T) {
	a := New(Options{Config: config.Default()})
	if a.Generation() != 0 {
		t.Errorf("default generation = %d, want 0 (unset)", a.Generation())
	}
	a.SetGeneration(42)
	if a.Generation() != 42 {
		t.Errorf("after SetGeneration(42): got %d, want 42", a.Generation())
	}
	a.SetGeneration(7)
	if a.Generation() != 7 {
		t.Errorf("after SetGeneration(7): got %d, want 7", a.Generation())
	}
}
