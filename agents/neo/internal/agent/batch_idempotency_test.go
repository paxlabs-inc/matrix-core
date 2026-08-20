// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/tools"
)

// tcid builds a tool call with an explicit id (the shared tc helper sets
// id == name, which would collide for two core_execute calls in one batch).
func tcid(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
}

// TestStateTouchDedupKey verifies only core_execute (with a non-empty intent)
// is a dedup candidate, keyed on its resolved (trimmed) intent text.
func TestStateTouchDedupKey(t *testing.T) {
	if _, ok := stateTouchDedupKey(tc("web_search", `{"q":"x"}`)); ok {
		t.Error("a reversible tool must NOT be a dedup candidate")
	}
	if _, ok := stateTouchDedupKey(tc(tools.CoreExecuteTool, `{"intent":""}`)); ok {
		t.Error("core_execute with an empty intent must not be a dedup candidate")
	}
	k1, ok1 := stateTouchDedupKey(tc(tools.CoreExecuteTool, `{"intent":"launch SPARK"}`))
	k2, ok2 := stateTouchDedupKey(tc(tools.CoreExecuteTool, `{"intent":"  launch SPARK  "}`))
	if !ok1 || !ok2 {
		t.Fatal("core_execute with an intent must be a dedup candidate")
	}
	if k1 != k2 {
		t.Errorf("trimmed-equal intents must share a key: %q vs %q", k1, k2)
	}
}

// TestDedupStateTouching maps a batch to its join indices: equivalent
// core_execute calls collapse to the canonical (earlier) index; independent
// reversible calls and distinct intents stay canonical (-1).
func TestDedupStateTouching(t *testing.T) {
	calls := []llm.ToolCall{
		tc(tools.CoreExecuteTool, `{"intent":"launch SPARK"}`), // 0 canonical
		tc("web_search", `{"q":"price"}`),                      // 1 reversible
		tc(tools.CoreExecuteTool, `{"intent":"launch SPARK"}`), // 2 dup of 0
		tc(tools.CoreExecuteTool, `{"intent":"send 5 PAX"}`),   // 3 distinct
	}
	joinTo := dedupStateTouching(calls)
	want := []int{-1, -1, 0, -1}
	for i := range want {
		if joinTo[i] != want[i] {
			t.Errorf("joinTo[%d] = %d, want %d", i, joinTo[i], want[i])
		}
	}
}

// TestRunToolCalls_DuplicateCoreExecuteJoins verifies the whole batch produces
// one tool result per call (well-formed transcript: every tool_call id is
// answered) and the duplicate core_execute shares the canonical's result —
// exercised through BOTH the concurrent and serial dispatch paths.
func TestRunToolCalls_DuplicateCoreExecuteJoins(t *testing.T) {
	for _, conc := range []int{0, 4} {
		cfg := config.Default()
		cfg.ToolDispatchConcurrency = conc
		a := New(Options{Config: cfg})

		calls := []llm.ToolCall{
			tcid("c0", tools.CoreExecuteTool, `{"intent":"launch SPARK"}`),
			tcid("c1", "read_text_file", `{"path":"missing.txt"}`),
			tcid("c2", tools.CoreExecuteTool, `{"intent":"launch SPARK"}`),
		}
		a.runToolCalls(context.Background(), calls)

		if len(a.working) != 3 {
			t.Fatalf("conc=%d: expected 3 tool results (one per call), got %d", conc, len(a.working))
		}
		if a.working[0].Content != a.working[2].Content {
			t.Errorf("conc=%d: duplicate core_execute must share the canonical result: %q vs %q", conc, a.working[0].Content, a.working[2].Content)
		}
	}
}
