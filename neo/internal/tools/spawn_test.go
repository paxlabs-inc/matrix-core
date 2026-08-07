// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"strings"
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

// TestSubagentSchemasAreStructurallyReadOnly pins the complete worker boundary:
// only local reads, read-only Git, and observation-only web/browser operations
// survive. A misleading read-like mutation and every synthetic remain absent.
func TestSubagentSchemasAreStructurallyReadOnly(t *testing.T) {
	obj := map[string]interface{}{"type": "object"}
	m := &Manager{
		byFunc: map[string]*boundTool{
			"fs__read_file":             {funcName: "fs__read_file", alias: "fs", name: "read_file", sideEffect: "read", desc: "read", params: obj},
			"fs__write_file":            {funcName: "fs__write_file", alias: "fs", name: "write_file", sideEffect: "write", desc: "write", params: obj},
			"exec__run":                 {funcName: "exec__run", alias: "exec", name: "run", sideEffect: "shell", desc: "run", params: obj},
			"git__git_log":              {funcName: "git__git_log", alias: "git", name: "git_log", sideEffect: "read", desc: "history", params: obj},
			"git__git_commit":           {funcName: "git__git_commit", alias: "git", name: "git_commit", sideEffect: "write", desc: "commit", params: obj},
			"browser__browser_navigate": {funcName: "browser__browser_navigate", alias: "browser", name: "browser_navigate", sideEffect: "network", desc: "navigate", params: obj},
			"browser__browser_snapshot": {funcName: "browser__browser_snapshot", alias: "browser", name: "browser_snapshot", sideEffect: "read", desc: "snapshot", params: obj},
			"browser__browser_click":    {funcName: "browser__browser_click", alias: "browser", name: "browser_click", sideEffect: "network", desc: "click", params: obj},
			"fetch__fetch":              {funcName: "fetch__fetch", alias: "fetch", name: "fetch", sideEffect: "network", desc: "fetch", params: obj},
			"web-search__web_search":    {funcName: "web-search__web_search", alias: "web-search", name: "web_search", sideEffect: "network", desc: "search", params: obj},
			"calendar__list_schedules":  {funcName: "calendar__list_schedules", alias: "calendar", name: "list_schedules", sideEffect: "read", desc: "schedules", params: obj},
			"memory__read_records":      {funcName: "memory__read_records", alias: "memory", name: "read_records", sideEffect: "read", desc: "memory", params: obj},
			"mail__send_message":        {funcName: "mail__send_message", alias: "mail", name: "send_message", sideEffect: "read", desc: "misclassified mutation", params: obj},
		},
		order: []string{
			"browser__browser_click", "browser__browser_navigate", "browser__browser_snapshot",
			"calendar__list_schedules", "exec__run", "fetch__fetch", "fs__read_file", "fs__write_file",
			"git__git_commit", "git__git_log", "mail__send_message", "memory__read_records",
			"web-search__web_search",
		},
		native: &nativeLocal{},
	}
	// Wire the synthetics so the FULL surface would advertise them.
	m.delegate = func(context.Context, string) (string, error) { return "", nil }
	m.recall = func(context.Context, string, []string, int, *time.Time) (string, error) { return "", nil }
	m.swarm = func(context.Context, []SubagentSpec) (string, error) { return "", nil }

	names := map[string]bool{}
	for _, s := range m.SubagentSchemas() {
		names[s.Function.Name] = true
	}
	for _, banned := range []string{
		CoreExecuteTool, MemoryRecallTool, SpawnSubagentsTool,
		"exec__run", "fs__write_file", "git__git_commit", "browser__browser_click",
		"calendar__list_schedules", "memory__read_records", "mail__send_message",
		nativeWriteFile, nativeShell, nativeServiceList, nativeShellOutput,
	} {
		if names[banned] {
			t.Errorf("sub-agent surface must NOT advertise %q", banned)
		}
	}
	for _, allowed := range []string{
		"fs__read_file", "git__git_log", "browser__browser_navigate",
		"browser__browser_snapshot", "fetch__fetch", "web-search__web_search",
		nativeReadFile, nativeListDir, nativeSearchFiles, nativeFileInfo, nativeGitDiff,
	} {
		if !names[allowed] {
			t.Errorf("sub-agent surface should advertise research tool %q, got %v", allowed, names)
		}
	}
}

func TestParseSubagentSpecsBoundsContext(t *testing.T) {
	long := strings.Repeat("x", 13_000)
	specs := parseSubagentSpecs(map[string]interface{}{"agents": []interface{}{
		map[string]interface{}{"name": long, "persona": long, "task": long},
	}})
	if len(specs) != 1 {
		t.Fatalf("spec count = %d, want 1", len(specs))
	}
	if len([]rune(specs[0].Name)) != 80 || len([]rune(specs[0].Persona)) != 600 || len([]rune(specs[0].Task)) != 12_000 {
		t.Fatalf("subagent context was not bounded: name=%d persona=%d task=%d", len([]rune(specs[0].Name)), len([]rune(specs[0].Persona)), len([]rune(specs[0].Task)))
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
			"fs__read_file":        {funcName: "fs__read_file", alias: "fs", name: "read_file", sideEffect: "read", desc: "read", params: map[string]interface{}{"type": "object"}},
			"paxeer__send_payment": {funcName: "paxeer__send_payment", desc: "move funds", params: map[string]interface{}{"type": "object"}, surface: Escalate},
		},
		order:     []string{"fs__read_file"},
		escalated: []string{"paxeer__send_payment"},
	}
	// Wire the money delegate so the FULL surface advertises core_execute
	// (advertised only when an Escalate-walled tool exists AND the delegate is
	// wired — on the no-barrier surface with nothing walled it never appears).
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
