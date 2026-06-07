// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"matrix/mcl/ir"
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
	d = &daemonState{plannerModel: "planner/y", executorModel: "exec/z"}
	if got := d.criticMod(); got != "planner/y" {
		t.Errorf("should fall back to synthMod (planner): got %q", got)
	}
	d = &daemonState{}
	if got := d.criticMod(); got != "" {
		t.Errorf("empty knobs -> empty: got %q", got)
	}
}

// The verdict JSON the auditor emits must round-trip into criticVerdict, and a
// fenced/reasoning-wrapped object must still be extractable via extractPlanJSON
// (the same extractor critiquePlan uses).
func TestCriticVerdict_ParseFromFenced(t *testing.T) {
	raw := "Here is my audit.\n```json\n{\"complete\": false, \"missing\": [\"Deploy the contract\", \"\"], \"rationale\": \"only compiled\"}\n```"
	clean := extractPlanJSON(raw)
	var v criticVerdict
	if err := json.Unmarshal([]byte(clean), &v); err != nil {
		t.Fatalf("unmarshal verdict: %v (clean=%q)", err, clean)
	}
	if v.Complete {
		t.Error("verdict should be incomplete")
	}
	if len(v.Missing) != 2 {
		t.Errorf("missing len = %d, want 2 (pre-normalization)", len(v.Missing))
	}
}
