// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mode

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
		err  bool
	}{
		{"", Engineer, false},
		{"prototype", Prototype, false},
		{"Engineer", Engineer, false},
		{" ARCHITECT ", Architect, false},
		{"yolo", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.err != (err != nil) || got != c.want {
			t.Fatalf("Parse(%q) = %v, %v", c.in, got, err)
		}
	}
}

func TestPolicyTuplesDiffer(t *testing.T) {
	proto, eng, arch := For(Prototype), For(Engineer), For(Architect)

	if proto.PlanningDepth != PlanInline || eng.PlanningDepth != PlanWaved || arch.PlanningDepth != PlanSpecFiles {
		t.Fatalf("planning depths: %s %s %s", proto.PlanningDepth, eng.PlanningDepth, arch.PlanningDepth)
	}
	if proto.VerifyCadence != VerifyMilestones || eng.VerifyCadence != VerifyPerTask || arch.VerifyCadence != VerifyPerTaskProperty {
		t.Fatalf("verify cadences: %s %s %s", proto.VerifyCadence, eng.VerifyCadence, arch.VerifyCadence)
	}
	// Implementation runs cold and ordered by mode ceremony; decisions run hot.
	if !(proto.ImplementationCreativity > eng.ImplementationCreativity && eng.ImplementationCreativity > arch.ImplementationCreativity) {
		t.Fatalf("implementation creativity ordering: %v %v %v", proto.ImplementationCreativity, eng.ImplementationCreativity, arch.ImplementationCreativity)
	}
	for _, p := range []Policy{proto, eng, arch} {
		if p.DecisionCreativity < p.ImplementationCreativity {
			t.Fatalf("%s: decision creativity %v must be hotter than implementation %v", p.Mode, p.DecisionCreativity, p.ImplementationCreativity)
		}
		if p.DecisionCandidates < 2 || p.DecisionCandidates > 3 {
			t.Fatalf("%s: decision candidates %d out of the 2-3 band", p.Mode, p.DecisionCandidates)
		}
	}
	if proto.Register != RegisterOutcome || eng.Register != RegisterTechnical {
		t.Fatalf("registers: %s %s", proto.Register, eng.Register)
	}
}

func TestModelPolicyPerRole(t *testing.T) {
	for _, m := range []Mode{Prototype, Engineer, Architect} {
		p := For(m)
		orch := p.OrchestratorLLM("http://gw", "did:matrix:u")
		dec := p.DecisionLLM("http://gw", "did:matrix:u")
		work := p.WorkerLLM("http://gw", "did:matrix:u")
		if orch.SlotLabel != GatewaySlot || dec.SlotLabel != GatewaySlot || work.SlotLabel != GatewaySlot {
			t.Fatalf("%s: slots %q/%q/%q, want cody for every role", m, orch.SlotLabel, dec.SlotLabel, work.SlotLabel)
		}
		if orch.Model != p.OrchestratorModel || dec.Model != p.OrchestratorModel || work.Model != p.WorkerModel {
			t.Fatalf("%s: models %q/%q/%q", m, orch.Model, dec.Model, work.Model)
		}
		// Workers run cold at the mode's implementation creativity; decision
		// authoring runs hot at the mode's decision creativity.
		if work.Temperature != p.ImplementationCreativity {
			t.Fatalf("%s: worker temperature %v, want implementation creativity %v", m, work.Temperature, p.ImplementationCreativity)
		}
		if dec.Temperature != p.DecisionCreativity {
			t.Fatalf("%s: decision temperature %v, want decision creativity %v", m, dec.Temperature, p.DecisionCreativity)
		}
		if !(dec.Temperature > work.Temperature || (m == Prototype && dec.Temperature >= work.Temperature)) {
			t.Fatalf("%s: decisions (%v) must run hotter than implementation (%v)", m, dec.Temperature, work.Temperature)
		}
	}
	// Every role in every mode pins grok-build-0.1 (xAI Grok) — the fast tier
	// and the strong tier are the same Grok model under the cody slot.
	const want = "grok-build-0.1"
	if FastModel != want {
		t.Fatalf("FastModel = %q, want %q", FastModel, want)
	}
	for _, m := range []Mode{Prototype, Engineer, Architect} {
		p := For(m)
		if p.OrchestratorModel != want || p.WorkerModel != want {
			t.Fatalf("%s: models orch=%q worker=%q, want both %q", m, p.OrchestratorModel, p.WorkerModel, want)
		}
	}
}

func TestRenderNeverRelaxesTheConstitution(t *testing.T) {
	for _, m := range []Mode{Prototype, Engineer, Architect} {
		out := For(m).Render()
		if !strings.Contains(out, "constitution binds identically") {
			t.Fatalf("%s render omits the constitution invariant:\n%s", m, out)
		}
	}
	if !strings.Contains(For(Prototype).Render(), "outcome language") {
		t.Fatal("prototype render missing the outcome register")
	}
	if !strings.Contains(For(Architect).Render(), "spec files") {
		t.Fatal("architect render missing durable spec files")
	}
}
