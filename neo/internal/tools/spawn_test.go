// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"testing"
	"time"
)

func TestParseSubagentSpecs(t *testing.T) {
	args := map[string]interface{}{
		"agents": []interface{}{
			map[string]interface{}{"name": "Go Analyst", "persona": "senior Go reviewer", "task": "review the executor package"},
			map[string]interface{}{"persona": "security", "task": "audit the contracts"}, // no name -> synthesized
			map[string]interface{}{"name": "Empty", "persona": "x"},                      // no task -> dropped
			"not-an-object", // wrong type -> skipped
		},
	}
	specs := parseSubagentSpecs(args)
	if len(specs) != 2 {
		t.Fatalf("want 2 specs (the no-task and non-object entries dropped), got %d: %+v", len(specs), specs)
	}
	if specs[0].Name != "Go Analyst" || specs[0].Task == "" {
		t.Errorf("spec[0] not parsed: %+v", specs[0])
	}
	if specs[1].Name == "" {
		t.Errorf("spec[1] should have a synthesized name, got empty")
	}
}

func TestParseSubagentSpecsNoAgents(t *testing.T) {
	if got := parseSubagentSpecs(map[string]interface{}{}); got != nil {
		t.Errorf("missing 'agents' should yield nil, got %+v", got)
	}
	if got := parseSubagentSpecs(map[string]interface{}{"agents": "nope"}); got != nil {
		t.Errorf("non-array 'agents' should yield nil, got %+v", got)
	}
}

// TestSubagentSchemasExcludeSynthetic asserts a sub-agent is never advertised
// the synthetic tools (no money, no recursion, no memory_recall) regardless of
// what the parent surface exposes.
func TestSubagentSchemasExcludeSynthetic(t *testing.T) {
	m := &Manager{
		byFunc: map[string]*boundTool{
			"fs__read_file": {funcName: "fs__read_file", desc: "read", params: map[string]interface{}{"type": "object"}},
			"exec__run":     {funcName: "exec__run", desc: "run", params: map[string]interface{}{"type": "object"}},
		},
		order: []string{"exec__run", "fs__read_file"},
	}
	// Wire the synthetics so the FULL surface would advertise them.
	m.delegate = func(context.Context, string) (string, error) { return "", nil }
	m.recall = func(context.Context, string, []string, int, *time.Time) (string, error) { return "", nil }
	m.swarm = func(context.Context, []SubagentSpec) (string, error) { return "", nil }

	names := map[string]bool{}
	for _, s := range m.SubagentSchemas() {
		names[s.Function.Name] = true
	}
	for _, banned := range []string{CoreExecuteTool, MemoryRecallTool, SpawnSubagentsTool} {
		if names[banned] {
			t.Errorf("sub-agent surface must NOT advertise %q", banned)
		}
	}
	if !names["fs__read_file"] || !names["exec__run"] {
		t.Errorf("sub-agent surface should keep the natural tools, got %v", names)
	}
}

// TestSubagentSurfacePassesValueTransferWall proves the sub-agent alignment
// contract never weakens the value-transfer wall (self-model task 4.3, req.9.3):
// even though the FULL parent surface advertises the money core_execute tool,
// the sub-agent surface (SubagentSchemas — what every spawned sub-agent runs on)
// passes AssertNoValueTransferTools.
func TestSubagentSurfacePassesValueTransferWall(t *testing.T) {
	m := &Manager{
		byFunc: map[string]*boundTool{
			"fs__read_file": {funcName: "fs__read_file", desc: "read", params: map[string]interface{}{"type": "object"}},
		},
		order: []string{"fs__read_file"},
	}
	// Wire the money delegate so the FULL surface advertises core_execute.
	m.delegate = func(context.Context, string) (string, error) { return "", nil }

	// Precondition: the full parent surface DOES carry a value-transfer tool.
	if err := AssertNoValueTransferTools(m.Schemas()); err == nil {
		t.Fatal("precondition: the full surface must advertise core_execute (the money wall)")
	}
	// The sub-agent surface must be clean.
	if err := AssertNoValueTransferTools(m.SubagentSchemas()); err != nil {
		t.Errorf("sub-agent surface must pass the value-transfer wall: %v", err)
	}
}
