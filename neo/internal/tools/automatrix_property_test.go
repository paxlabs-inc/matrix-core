// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import "testing"

// This is the tools-package half of verification task 7.2 —
//
//	Property 2: money can never be reached by an autonomous run (structural).
//
// Validates: Requirements 3.1, 3.2.
//
// It quantifies the structural guarantee over a POPULATED real Manager surface
// (the agent-package half can only inspect a bare Manager whose restricted set
// is empty). Using the same real automatrixTestManager() — Natural tools plus
// an Escalate-classified money tool plus a wired delegate so the FULL surface
// advertises core_execute — it asserts the property holds for EVERY tool the
// restricted Automatrix surface actually advertises, and that the restriction
// removes only money, not capability.

// TestAutomatrixSurfaceProperty_NoValueTransferTool is the universally-
// quantified structural property: for ALL tools advertised on the real
// Automatrix surface, none is value-moving/signing and the money/chain delegate
// is absent — while the full surface the restriction derives from DOES advertise
// core_execute (non-vacuous), and the genuine non-money Natural tools survive.
func TestAutomatrixSurfaceProperty_NoValueTransferTool(t *testing.T) {
	m := automatrixTestManager()

	// Non-vacuous baseline: the full surface really can reach money.
	full := names(m.Schemas())
	if !full[CoreExecuteTool] {
		t.Fatalf("baseline broken: full surface must advertise %q (got %v)", CoreExecuteTool, keys(full))
	}

	auto := m.AutomatrixSchemas()
	if len(auto) == 0 {
		t.Fatal("populated Automatrix surface advertised nothing; expected the restricted Natural tools")
	}

	// Universally quantified over the real advertised set: no value-transfer.
	for _, s := range auto {
		name := s.Function.Name
		if name == CoreExecuteTool {
			t.Errorf("Automatrix surface advertises the money/chain delegate %q", name)
		}
		if IsValueTransferTool(name) {
			t.Errorf("Automatrix surface advertises value-moving/signing tool %q", name)
		}
	}
	// The package guard must agree with the per-tool quantification.
	if err := AssertNoValueTransferTools(auto); err != nil {
		t.Errorf("AssertNoValueTransferTools(AutomatrixSchemas) = %v, want nil", err)
	}

	// The restriction removes money, not capability: the Natural tools remain.
	got := names(auto)
	for _, want := range []string{"fs__read_file", "web__search"} {
		if !got[want] {
			t.Errorf("Automatrix surface dropped the non-money tool %q (got %v)", want, keys(got))
		}
	}
	// And the Escalate-classified money tool is structurally absent (it is
	// never even advertised on the full surface; only the synthetic delegate is).
	if got["paxeer__send_payment"] {
		t.Errorf("Automatrix surface must never advertise the escalate money tool")
	}
}
