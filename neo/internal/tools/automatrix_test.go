// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"testing"

	"matrix/neo/internal/llm"
)

// automatrixTestManager builds a REAL Manager (no fakes) whose surface mixes a
// couple of Natural tools, an Escalate-classified money tool, and the full set
// of synthetics wired (delegate/recall/swarm), so the FULL surface would
// advertise core_execute. The Automatrix surface is then asserted to omit every
// value-moving/signing tool.
func automatrixTestManager() *Manager {
	obj := func() map[string]interface{} { return map[string]interface{}{"type": "object"} }
	m := &Manager{
		byFunc: map[string]*boundTool{
			"fs__read_file":        {funcName: "fs__read_file", alias: "fs", name: "read_file", sideEffect: "read", desc: "read", params: obj(), surface: Natural},
			"web__search":          {funcName: "web__search", alias: "web-search", name: "web_search", sideEffect: "network", desc: "search", params: obj(), surface: Natural},
			"paxeer__send_payment": {funcName: "paxeer__send_payment", alias: "paxeer", name: "send_payment", sideEffect: "network", desc: "move funds", params: obj(), surface: Escalate},
		},
		// Escalate tools are bound but NEVER advertised (never in order); the
		// only money path on the full surface is the synthetic core_execute.
		order:     []string{"fs__read_file", "web__search"},
		escalated: []string{"paxeer__send_payment"},
	}
	m.delegate = func(context.Context, string) (string, error) { return "", nil }
	return m
}

// TestAutomatrixSchemasOmitMoney is the structural guarantee (req.3.1/3.2): the
// Automatrix advertised tool schema set contains NO value-moving/signing tool,
// while the full surface it derives from DOES advertise core_execute.
func TestAutomatrixSchemasOmitMoney(t *testing.T) {
	m := automatrixTestManager()

	full := names(m.Schemas())
	if !full[CoreExecuteTool] {
		t.Fatalf("baseline broken: full surface must advertise %q, got %v", CoreExecuteTool, keys(full))
	}

	auto := m.AutomatrixSchemas()
	got := names(auto)
	if got[CoreExecuteTool] {
		t.Errorf("Automatrix surface must NOT advertise %q", CoreExecuteTool)
	}
	for name := range got {
		if IsValueTransferTool(name) {
			t.Errorf("Automatrix surface advertises value-moving/signing tool %q", name)
		}
	}
	if err := AssertNoValueTransferTools(auto); err != nil {
		t.Errorf("Automatrix advertised set not clean: %v", err)
	}
	// The restriction retains only structurally read-only research capability.
	if !got["fs__read_file"] || !got["web__search"] {
		t.Errorf("Automatrix surface should keep the natural tools, got %v", keys(got))
	}
}

// TestAutomatrixSchemasMatchSubagent pins that the Automatrix surface IS the
// existing sub-agent restricted surface (not a parallel mechanism).
func TestAutomatrixSchemasMatchSubagent(t *testing.T) {
	m := automatrixTestManager()
	auto := names(m.AutomatrixSchemas())
	sub := names(m.SubagentSchemas())
	if len(auto) != len(sub) {
		t.Fatalf("Automatrix surface diverges from sub-agent surface: %v vs %v", keys(auto), keys(sub))
	}
	for n := range sub {
		if !auto[n] {
			t.Errorf("Automatrix surface missing sub-agent tool %q", n)
		}
	}
}

func TestIsValueTransferTool(t *testing.T) {
	transfer := []string{CoreExecuteTool, "paxeer__send_payment", "dex__swap_tokens", "wallet__withdraw", "bridge__transfer"}
	for _, n := range transfer {
		if !IsValueTransferTool(n) {
			t.Errorf("IsValueTransferTool(%q) = false, want true", n)
		}
	}
	clean := []string{"fs__read_file", "web__search", "task_complete", "read_overflow", "memory_recall"}
	for _, n := range clean {
		if IsValueTransferTool(n) {
			t.Errorf("IsValueTransferTool(%q) = true, want false", n)
		}
	}
}

func TestAssertNoValueTransferToolsCatchesMoney(t *testing.T) {
	dirty := []llm.Tool{
		llm.NewFunctionTool("fs__read_file", "read", nil),
		coreExecuteSchema(),
	}
	if err := AssertNoValueTransferTools(dirty); err == nil {
		t.Error("AssertNoValueTransferTools must reject a set containing core_execute")
	}
	clean := []llm.Tool{
		llm.NewFunctionTool("fs__read_file", "read", nil),
		llm.NewFunctionTool("web__search", "search", nil),
	}
	if err := AssertNoValueTransferTools(clean); err != nil {
		t.Errorf("AssertNoValueTransferTools(clean) = %v, want nil", err)
	}
}

func names(schemas []llm.Tool) map[string]bool {
	out := map[string]bool{}
	for _, s := range schemas {
		out[s.Function.Name] = true
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
