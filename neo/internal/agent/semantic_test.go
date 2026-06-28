// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

func semAgent(pct int) *Agent {
	cfg := config.Default()
	cfg.SemanticStallSimilarityPct = pct
	return New(Options{Config: cfg})
}

// TestSemanticRepeat_RewordedSameOperation: a reworded core_execute intent at
// the SAME target (SPARK) must count as a repeat even though the byte signature
// differs — the SPARK-loop defence (NE-4).
func TestSemanticRepeat_RewordedSameOperation(t *testing.T) {
	a := semAgent(50)
	prev := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"launch the SPARK token"}`)}
	cur := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"please launch SPARK token now"}`)}
	if batchSignature(prev) == batchSignature(cur) {
		t.Fatal("test setup: the two batches must have DIFFERENT byte signatures")
	}
	if !a.semanticRepeat(prev, cur) {
		t.Error("a reworded retry of the same operation must be detected as a semantic repeat")
	}
}

// TestSemanticRepeat_DifferentTargetIsNotRepeat: the false-trip guard — two
// reads of DIFFERENT pools share a tool but have different targets, so they
// must NOT be a repeat (req 4.3).
func TestSemanticRepeat_DifferentTargetIsNotRepeat(t *testing.T) {
	a := semAgent(50)
	prev := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"read the balance of pool 0xAAAAAAAAAAAA"}`)}
	cur := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"read the balance of pool 0xBBBBBBBBBBBB"}`)}
	if a.semanticRepeat(prev, cur) {
		t.Error("two operations on DIFFERENT targets must not be treated as a repeat")
	}
}

// TestSemanticRepeat_DifferentOperationSameTarget: a different verb on the same
// target (read vs transfer) has low word overlap and must not trip.
func TestSemanticRepeat_DifferentOperationSameTarget(t *testing.T) {
	a := semAgent(50)
	prev := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"read balance SPARK"}`)}
	cur := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"transfer SPARK"}`)}
	if a.semanticRepeat(prev, cur) {
		t.Error("distinct operations on the same target must not be a semantic repeat")
	}
}

// TestSemanticRepeat_DisabledByZeroThreshold: threshold 0 disables semantic
// detection (legacy exact-only behavior).
func TestSemanticRepeat_DisabledByZeroThreshold(t *testing.T) {
	a := semAgent(0)
	prev := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"launch the SPARK token"}`)}
	cur := []llm.ToolCall{tc(tools.CoreExecuteTool, `{"intent":"please launch SPARK token now"}`)}
	if a.semanticRepeat(prev, cur) {
		t.Error("semantic detection must be disabled when the threshold is 0")
	}
}

// TestSemanticRepeat_DifferentToolShape: different tool names are distinct ops.
func TestSemanticRepeat_DifferentToolShape(t *testing.T) {
	a := semAgent(50)
	prev := []llm.ToolCall{tc("read", `{"q":"SPARK token price"}`)}
	cur := []llm.ToolCall{tc("web_search", `{"q":"SPARK token price"}`)}
	if a.semanticRepeat(prev, cur) {
		t.Error("different tools must not be treated as the same operation")
	}
}

func TestOverlapPct(t *testing.T) {
	set := func(ks ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, k := range ks {
			m[k] = struct{}{}
		}
		return m
	}
	if got := overlapPct(set(), set()); got != 100 {
		t.Errorf("empty/empty overlap = %d, want 100", got)
	}
	if got := overlapPct(set("a"), set()); got != 0 {
		t.Errorf("non-empty/empty overlap = %d, want 0", got)
	}
	if got := overlapPct(set("a", "b"), set("a", "b", "c")); got != 100 {
		t.Errorf("subset overlap = %d, want 100 (overlap coefficient)", got)
	}
	if got := overlapPct(set("a", "b"), set("c", "d")); got != 0 {
		t.Errorf("disjoint overlap = %d, want 0", got)
	}
}
