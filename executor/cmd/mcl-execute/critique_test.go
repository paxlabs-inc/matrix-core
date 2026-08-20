// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"strings"
	"testing"

	"centra/core/cassandra"
	"centra/core/mcl/ir"
)

// samplePlan builds a small executed plan: a tool_call that compiled a contract
// (with a recorded result) and a step that summarized — enough to exercise the
// digest builder over both node kinds.
func samplePlan() *ir.PlanTree {
	return &ir.PlanTree{
		IntentID: "i1",
		Root: ir.PlanNode{
			ID:   "root",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{
					ID:   "n01",
					Kind: ir.NodeToolCall,
					ToolCall: &ir.ToolCallPayload{
						ToolRef: "matrix://tool/mcp/tachyon/tachyon_compile@0.1.0",
						Args:    map[string]string{"sources": "{...}", "contract": "MFT"},
					},
					ResultText: "project_id=abc123\nartifact=MFT",
				},
				{
					ID:         "n02",
					Kind:       ir.NodeStep,
					ResultText: "Compiled MFT cleanly; project_id abc123.",
				},
			},
		},
	}
}

func TestBuildExecutionDigest(t *testing.T) {
	d := buildExecutionDigest(samplePlan())
	for _, want := range []string{
		"TOOL matrix://tool/mcp/tachyon/tachyon_compile@0.1.0",
		"contract=MFT",
		"project_id=abc123",
		"STEP n02",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("digest missing %q\n--- digest ---\n%s", want, d)
		}
	}
	// Newlines in a node result must be collapsed to keep one node per line.
	if strings.Contains(d, "project_id=abc123\nartifact") {
		t.Errorf("digest did not collapse multi-line result: %s", d)
	}
}

func TestBuildExecutionDigest_Empty(t *testing.T) {
	if got := buildExecutionDigest(nil); got == "" {
		t.Error("nil plan digest should be a non-empty sentinel")
	}
}

func TestCompactArgs_SortedAndDeterministic(t *testing.T) {
	got := compactArgs(map[string]string{"b": "2", "a": "1", "c": "3"})
	if got != "{a=1, b=2, c=3}" {
		t.Errorf("compactArgs = %q, want sorted {a=1, b=2, c=3}", got)
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\nb\tc\r\nd"); got != "a b c d" {
		t.Errorf("oneLine = %q, want 'a b c d'", got)
	}
}

func TestBuildContinuationNote(t *testing.T) {
	note := buildContinuationNote("TOOL x -> ok", []string{"Deploy the contract", "Transfer 1000 MFT"})
	for _, want := range []string{
		"CONTINUATION",
		"do NOT repeat",
		"TOOL x -> ok",
		"1. Deploy the contract",
		"2. Transfer 1000 MFT",
		"${<nodeID>.output}",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("continuation note missing %q\n--- note ---\n%s", want, note)
		}
	}
}

func TestCriticMod_Precedence(t *testing.T) {
	d := &daemonState{criticModel: "critic/x", plannerModel: "planner/y", executorModel: "exec/z"}
	if got := d.criticMod(); got != "critic/x" {
		t.Errorf("criticModel should win: got %q", got)
	}
	// Unset critic knob falls back to Cassandra's non-reasoning grok pin,
	// NOT the planner/executor model.
	d = &daemonState{plannerModel: "planner/y", executorModel: "exec/z"}
	if got := d.criticMod(); got != defaultCriticModel {
		t.Errorf("should fall back to defaultCriticModel: got %q", got)
	}
	d = &daemonState{}
	if got := d.criticMod(); got != defaultCriticModel {
		t.Errorf("empty knobs -> defaultCriticModel: got %q", got)
	}
}

// The verdict JSON the auditor emits must round-trip through the re-homed
// cassandra parser, and a fenced/reasoning-wrapped object must still be
// extractable — the exact path critiquePlan now uses via cassandra.ParseVerdict.
func TestCriticVerdict_ParseFromFenced(t *testing.T) {
	raw := "Here is my audit.\n```json\n{\"complete\": false, \"missing\": [\"Deploy the contract\", \"\"], \"rationale\": \"only compiled\"}\n```"
	v, err := cassandra.ParseVerdict(raw)
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.CoverageComplete() {
		t.Error("verdict should be incomplete")
	}
	// The canonical parser drops the blank entry, leaving the one real item.
	if len(v.Missing) != 1 || v.Missing[0] != "Deploy the contract" {
		t.Errorf("missing = %v, want [\"Deploy the contract\"]", v.Missing)
	}
}
