// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package tools

import (
	"context"
	"testing"
	"time"
)

func briefTestManager() *Manager {
	obj := func() map[string]interface{} { return map[string]interface{}{"type": "object"} }
	m := &Manager{
		byFunc: map[string]*boundTool{
			"web__web_search":      {funcName: "web__web_search", desc: "search", params: obj(), surface: Natural},
			"web__web_news":        {funcName: "web__web_news", desc: "news", params: obj(), surface: Natural},
			"fetch__fetch":         {funcName: "fetch__fetch", desc: "fetch", params: obj(), surface: Natural},
			"fs__read_file":        {funcName: "fs__read_file", desc: "read", params: obj(), surface: Natural},
			"fs__write_file":       {funcName: "fs__write_file", desc: "write", params: obj(), surface: Natural},
			"exec__run":            {funcName: "exec__run", desc: "run", params: obj(), surface: Natural},
			"paxeer__send_payment": {funcName: "paxeer__send_payment", desc: "move funds", params: obj(), surface: Escalate},
		},
		order:     []string{"web__web_search", "web__web_news", "fetch__fetch", "fs__read_file", "fs__write_file", "exec__run"},
		escalated: []string{"paxeer__send_payment"},
	}
	m.delegate = func(context.Context, string) (string, error) { return "", nil }
	m.recall = func(context.Context, string, []string, int, *time.Time) (string, error) { return "", nil }
	return m
}

// TestBriefSchemasPositiveAllowlist proves the brief surface advertises ONLY the
// read-only info tools + memory_recall, and none of fs/exec/money (req 15.1).
func TestBriefSchemasPositiveAllowlist(t *testing.T) {
	m := briefTestManager()

	// Baseline: the full surface advertises fs/exec and core_execute.
	full := names(m.Schemas())
	if !full["fs__read_file"] || !full[CoreExecuteTool] {
		t.Fatalf("baseline broken: full surface must advertise fs + core_execute, got %v", keys(full))
	}

	got := names(m.BriefSchemas())
	want := []string{"web__web_search", "web__web_news", "fetch__fetch", MemoryRecallTool}
	for _, w := range want {
		if !got[w] {
			t.Errorf("brief surface missing allowed tool %q; got %v", w, keys(got))
		}
	}
	forbidden := []string{"fs__read_file", "fs__write_file", "exec__run", "paxeer__send_payment", CoreExecuteTool, SpawnSubagentsTool}
	for _, f := range forbidden {
		if got[f] {
			t.Errorf("brief surface must NOT advertise %q", f)
		}
	}
	// The advertised set must pass BOTH the positive brief assertion and the
	// value-transfer wall.
	if err := AssertBriefSurface(m.BriefSchemas()); err != nil {
		t.Errorf("brief surface not clean: %v", err)
	}
	if err := AssertNoValueTransferTools(m.BriefSchemas()); err != nil {
		t.Errorf("brief surface has a value-transfer tool: %v", err)
	}
}

// TestBriefSchemasNoRecallWhenUnwired proves memory_recall is absent when the
// cortex recall seam is not wired.
func TestBriefSchemasNoRecallWhenUnwired(t *testing.T) {
	m := briefTestManager()
	m.recall = nil
	if names(m.BriefSchemas())[MemoryRecallTool] {
		t.Error("memory_recall must be absent when recall is unwired")
	}
}

// TestIsBriefTool covers base-name matching + the memory_recall exception.
func TestIsBriefTool(t *testing.T) {
	allow := []string{"web__web_search", "web__web_news", "fetch__fetch", "fetch", MemoryRecallTool}
	for _, n := range allow {
		if !IsBriefTool(n) {
			t.Errorf("IsBriefTool(%q) = false, want true", n)
		}
	}
	deny := []string{"fs__read_file", "exec__run", "paxeer__send_payment", CoreExecuteTool, "web__websearch_history"}
	for _, n := range deny {
		if IsBriefTool(n) {
			t.Errorf("IsBriefTool(%q) = true, want false", n)
		}
	}
}

// TestAssertBriefSurfaceRejectsNonInfo proves a filesystem tool in the set fails.
func TestAssertBriefSurfaceRejectsNonInfo(t *testing.T) {
	m := briefTestManager()
	dirty := append(m.BriefSchemas(), m.Schemas()[3]) // splice in a non-info tool
	if err := AssertBriefSurface(dirty); err == nil {
		t.Error("a set with a filesystem tool must fail AssertBriefSurface")
	}
}
