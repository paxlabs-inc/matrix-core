// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/tools"
)

// TestNewAutomatrixSurfaceExcludesMoney proves the structural guarantee at the
// AGENT boundary against the REAL advertised tool schema set (the slice the
// model is handed as ChatRequest.Tools): on the no-barrier surface (nothing
// Escalate-walled, no delegate — the production default) core_execute is a
// ghost and must not be advertised to ANY agent, and an Automatrix agent —
// built through the existing RestrictTools mechanism — advertises no
// value-moving/signing tool at all. The positive walled-manager advertisement
// is proven in the tools package (spawn/automatrix tests).
func TestNewAutomatrixSurfaceExcludesMoney(t *testing.T) {
	m := &tools.Manager{} // real Manager, no-barrier: nothing walled, no delegate

	full := New(Options{Tools: m})
	if advertises(full, tools.CoreExecuteTool) {
		t.Fatalf("no-barrier full agent surface must not advertise %q", tools.CoreExecuteTool)
	}

	auto := NewAutomatrix(Options{Tools: m})
	if advertises(auto, tools.CoreExecuteTool) {
		t.Errorf("Automatrix agent must NOT advertise %q", tools.CoreExecuteTool)
	}
	for _, s := range auto.AdvertisedTools() {
		if tools.IsValueTransferTool(s.Function.Name) {
			t.Errorf("Automatrix agent advertises value-moving/signing tool %q", s.Function.Name)
		}
	}
	if err := tools.AssertNoValueTransferTools(auto.AdvertisedTools()); err != nil {
		t.Errorf("Automatrix advertised set not clean: %v", err)
	}
}

// TestNewAutomatrixForcesRestrictTools pins that the helper is the restriction:
// even if a caller passes RestrictTools=false, NewAutomatrix overrides it, so a
// proactive run can never be dispatched on the full money-capable surface.
func TestNewAutomatrixForcesRestrictTools(t *testing.T) {
	m := &tools.Manager{}
	auto := NewAutomatrix(Options{Tools: m, RestrictTools: false})
	if advertises(auto, tools.CoreExecuteTool) {
		t.Errorf("NewAutomatrix must force RestrictTools regardless of the passed option")
	}
}

// TestNewAutomatrixAutonomousPersona pins that an Automatrix agent's system
// prompt carries the autonomous-mode framing (user away, self-contained
// deliverable, no screen surfaces, no questions back) and a normal agent's
// does not — the "Neo isn't aware I'm not there" fix.
func TestNewAutomatrixAutonomousPersona(t *testing.T) {
	m := &tools.Manager{}

	auto := NewAutomatrix(Options{Tools: m})
	p := auto.systemPrompt()
	for _, want := range []string{
		"Autonomous mode — the user is AWAY",
		"NOT watching",
		"self-contained",
		"Never ask the user anything",
		"on your screen",
	} {
		if !containsStr(p, want) {
			t.Errorf("Automatrix system prompt missing %q", want)
		}
	}

	full := New(Options{Tools: m})
	if containsStr(full.systemPrompt(), "Autonomous mode — the user is AWAY") {
		t.Errorf("normal agent must not carry the autonomous-mode section")
	}
}

// TestRestrictedDispatchRejectsUnadvertisedTool proves the advertised-surface
// guard at the REAL dispatch path: a restricted agent calling a tool name that
// is not in its advertised set (e.g. the synthetic construct_render, which the
// Manager's dispatch switch would otherwise serve) gets an in-band rejection
// and the call never reaches the Manager.
func TestRestrictedDispatchRejectsUnadvertisedTool(t *testing.T) {
	m := &tools.Manager{}
	auto := NewAutomatrix(Options{Tools: m})
	if advertises(auto, tools.ConstructRenderTool) {
		t.Fatalf("precondition: restricted surface must not advertise %q", tools.ConstructRenderTool)
	}
	content, _, _, isErr, class, _ := auto.dispatchWithRetry(t.Context(), tools.ConstructRenderTool, map[string]interface{}{})
	if !isErr {
		t.Fatalf("unadvertised tool dispatch must be an in-band error, got content %q", content)
	}
	if !containsStr(content, "not available") {
		t.Errorf("rejection should name the tool as unavailable, got %q", content)
	}
	_ = class
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

func advertises(a *Agent, name string) bool {
	for _, s := range a.AdvertisedTools() {
		if s.Function.Name == name {
			return true
		}
	}
	return false
}
