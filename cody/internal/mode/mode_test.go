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
	if !(proto.Creativity > eng.Creativity && eng.Creativity > arch.Creativity) {
		t.Fatalf("creativity ordering: %v %v %v", proto.Creativity, eng.Creativity, arch.Creativity)
	}
	if proto.Register != RegisterOutcome || eng.Register != RegisterTechnical {
		t.Fatalf("registers: %s %s", proto.Register, eng.Register)
	}
}

func TestModelPolicyPerRole(t *testing.T) {
	for _, m := range []Mode{Prototype, Engineer, Architect} {
		p := For(m)
		orch := p.OrchestratorLLM("http://gw", "did:matrix:u")
		work := p.WorkerLLM("http://gw", "did:matrix:u")
		if orch.SlotLabel != GatewaySlot || work.SlotLabel != GatewaySlot {
			t.Fatalf("%s: slots %q/%q, want cody for both roles", m, orch.SlotLabel, work.SlotLabel)
		}
		if orch.Model != p.OrchestratorModel || work.Model != p.WorkerModel {
			t.Fatalf("%s: models %q/%q", m, orch.Model, work.Model)
		}
		if work.Temperature != p.Creativity {
			t.Fatalf("%s: worker temperature %v, want the mode's creativity %v", m, work.Temperature, p.Creativity)
		}
	}
	// Prototype workers run a faster model than the orchestrator's pin.
	proto := For(Prototype)
	if proto.WorkerModel == proto.OrchestratorModel {
		t.Fatal("prototype worker should not pin the orchestrator's stronger model")
	}
	if proto.WorkerModel == For(Engineer).WorkerModel {
		t.Fatal("prototype worker model should be the fast tier, not the default")
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
