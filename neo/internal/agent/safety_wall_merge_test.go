// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"sort"
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/tools"
)

// selfModelKnowsTheWall is a self-model whose structural summary explicitly
// names the value-transfer wall and whose failure patterns mention money tools.
// The agent KNOWS about core_execute — the point of the wall-under-merge proof
// is that knowing about it must never let a self-aware or unified agent ADVERTISE
// it. Self-knowledge is a reasoning aid, not a capability grant (req.11.2).
const selfModelKnowsTheWall = "Architecture (how you are built): core_execute is the ONLY value-transfer path; it is classified Escalate and is absent from every autonomous and sub-agent surface. send_payment and swap are Escalate money tools reachable only through it.\n" +
	"Failure patterns to avoid:\n- [failure-mode:money] I must never try to move funds outside core_execute."

func toolNameSet(a *Agent) []string {
	names := make([]string, 0)
	for _, s := range a.AdvertisedTools() {
		names = append(names, s.Function.Name)
	}
	sort.Strings(names)
	return names
}

func sameToolSet(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// TestSafetyWallInvariantUnderMerge proves req.11.1 + 11.4 on the REAL agent
// surfaces: core_execute stays classified as the value-transfer money path, yet
// on the no-barrier surface (nothing Escalate-walled, the production default —
// the wallet write lane is carried directly) it is advertised NOWHERE: not on
// the unified agent's full surface, the autonomous surface, or any sub-agent
// surface. Advertising the ghost tool on the full surface while the pinned
// prose said it does not exist is what sent the model to core_execute under
// failure (2026-07-13 LayerX deposit incident). The positive half — a manager
// that DOES wall tools advertises core_execute on the full surface only — is
// proven in the tools package with real walled managers (spawn/automatrix
// tests). A more capable, self-aware, unified agent is not a more dangerous one.
func TestSafetyWallInvariantUnderMerge(t *testing.T) {
	m := &tools.Manager{} // real Manager, no-barrier: nothing walled, no delegate

	// core_execute stays classified as the value-transfer / Escalate money path.
	if !tools.IsValueTransferTool(tools.CoreExecuteTool) {
		t.Fatalf("core_execute must classify as the value-transfer money path")
	}

	// The unified agent's full surface must NOT carry the ghost delegate when
	// nothing is walled behind it — the wallet tools are the money lane.
	full := New(Options{Config: config.Default(), Tools: m, SelfModel: selfModelKnowsTheWall})
	if advertises(full, tools.CoreExecuteTool) {
		t.Fatalf("no-barrier full surface must not advertise %q", tools.CoreExecuteTool)
	}

	// The autonomous surface — money is structurally absent even with a self-model
	// that describes it.
	auto := NewAutomatrix(Options{Config: config.Default(), Tools: m, SelfModel: selfModelKnowsTheWall})
	if advertises(auto, tools.CoreExecuteTool) {
		t.Errorf("autonomous surface must not advertise %q", tools.CoreExecuteTool)
	}
	if err := tools.AssertNoValueTransferTools(auto.AdvertisedTools()); err != nil {
		t.Errorf("autonomous surface must pass the safety wall: %v", err)
	}

	// Every sub-agent surface — restricted, and self-model-bearing — passes too.
	sub := New(Options{
		Config:        config.Default(),
		Tools:         m,
		Persona:       "a scoped reviewer",
		SelfModel:     selfModelKnowsTheWall,
		RestrictTools: true,
	})
	if advertises(sub, tools.CoreExecuteTool) {
		t.Errorf("sub-agent surface must not advertise %q", tools.CoreExecuteTool)
	}
	if err := tools.AssertNoValueTransferTools(sub.AdvertisedTools()); err != nil {
		t.Errorf("sub-agent surface must pass the safety wall: %v", err)
	}
}

// TestSelfModelNeverWidensTheSurface proves req.11.2: injecting the shared
// self-model — even one that names the money wall — changes the agent's PROSE,
// never its advertised tool surface. The restricted surface is byte-identical
// with and without the self-model, so no self-model / self-authoring code path
// can add, advertise, or unlock a value-moving capability.
func TestSelfModelNeverWidensTheSurface(t *testing.T) {
	m := &tools.Manager{}

	plain := New(Options{Config: config.Default(), Tools: m, Persona: "a scoped reviewer", RestrictTools: true})
	withSM := New(Options{Config: config.Default(), Tools: m, Persona: "a scoped reviewer", RestrictTools: true, SelfModel: selfModelKnowsTheWall})

	plainSet := toolNameSet(plain)
	smSet := toolNameSet(withSM)
	if !sameToolSet(plainSet, smSet) {
		t.Fatalf("self-model injection widened/changed the advertised surface:\n without=%v\n with   =%v", plainSet, smSet)
	}
	// Both surfaces remain value-transfer-free.
	if err := tools.AssertNoValueTransferTools(plain.AdvertisedTools()); err != nil {
		t.Errorf("plain restricted surface: %v", err)
	}
	if err := tools.AssertNoValueTransferTools(withSM.AdvertisedTools()); err != nil {
		t.Errorf("self-model-bearing restricted surface: %v", err)
	}
	// The self-model injection is non-vacuous: the sub-agent's prose really did
	// gain the wall-describing self-knowledge (otherwise this proves nothing).
	if got := withSM.systemPrompt(); !strings.Contains(got, "value-transfer path") {
		t.Fatalf("precondition: the self-model must actually be injected into the sub-agent prose")
	}
}
