// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"testing"

	"matrix/neo/internal/tools"
)

// TestNewAutomatrixSurfaceExcludesMoney proves the structural guarantee at the
// AGENT boundary against the REAL advertised tool schema set (the slice the
// model is handed as ChatRequest.Tools): a normal agent over the same real
// Manager advertises core_execute, but an Automatrix agent — built through the
// existing RestrictTools mechanism — does not, and its advertised set contains
// no value-moving/signing tool at all.
func TestNewAutomatrixSurfaceExcludesMoney(t *testing.T) {
	m := &tools.Manager{} // real Manager type; Schemas() always advertises core_execute

	full := New(Options{Tools: m})
	if !advertises(full, tools.CoreExecuteTool) {
		t.Fatalf("baseline broken: full agent surface must advertise %q", tools.CoreExecuteTool)
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

func advertises(a *Agent, name string) bool {
	for _, s := range a.AdvertisedTools() {
		if s.Function.Name == name {
			return true
		}
	}
	return false
}
