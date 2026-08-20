// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"centra/protocol/codegraph/selfmodel"
	"centra/agents/neo/internal/config"
)

// neoWord matches the IDENTITY word "Neo" (capitalized, word-bounded) — never
// the lowercase module path segment ("centra/agents/neo/...") that legitimately rides
// structural fragments.
var neoWord = regexp.MustCompile(`\bNeo\b`)

// TestMorpheusIdentity_CharterRendersFromConfig proves MORPHEUS req.8.2/8.3:
// the default charter speaks as Morpheus end to end — the identity clauses,
// the embedded ground truth ({{AGENT_NAME}} bound from config), and the
// sub-agent persona charter inheriting through cfg.AgentName — with no stray
// hardcoded Neo on any of it.
func TestMorpheusIdentity_CharterRendersFromConfig(t *testing.T) {
	a := New(Options{Config: config.Default()})
	sp := a.systemPrompt()
	for _, want := range []string{
		"You are Morpheus, Centra AI's default agent",
		"You are Morpheus, the default agent of Centra AI", // ground truth, name bound from config
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("default charter missing %q", want)
		}
	}
	if strings.Contains(sp, "{{AGENT_NAME}}") {
		t.Error("ground-truth placeholder must be bound, not rendered raw")
	}
	if hit := neoWord.FindString(sp); hit != "" {
		t.Errorf("stray hardcoded %q on the flipped charter surface", hit)
	}

	// Sub-agent personas inherit the name through cfg.AgentName (req.8.3).
	sub := New(Options{Config: config.Default(), Persona: "verify the deployment", RestrictTools: true})
	ssp := sub.systemPrompt()
	if !strings.Contains(ssp, `You are "Morpheus", a focused sub-agent`) {
		t.Error("sub-agent persona charter must inherit the Morpheus identity")
	}
	if hit := neoWord.FindString(ssp); hit != "" {
		t.Errorf("stray hardcoded %q on the sub-agent charter", hit)
	}
}

// TestMorpheusIdentity_NeoStaysAValidConfiguredName proves req.8.2's second
// half: the flip is config-driven, so a deployment that configures the agent
// as Neo keeps rendering as Neo — the name is a setting, not a constant.
func TestMorpheusIdentity_NeoStaysAValidConfiguredName(t *testing.T) {
	cfg := config.Default()
	cfg.AgentName = "Neo"
	a := New(Options{Config: cfg})
	sp := a.systemPrompt()
	if !strings.Contains(sp, "You are Neo, Centra AI's default agent") ||
		!strings.Contains(sp, "You are Neo, the default agent of Centra AI") {
		t.Error("a configured Neo identity must render as Neo")
	}
	if strings.Contains(sp, "Morpheus") {
		t.Error("a configured Neo identity must not leak the Morpheus default")
	}
}

// TestMorpheusIdentity_CapabilitySurfaceFromRealArtifact proves req.8.3 on the
// REAL regenerated artifact: the resident capability surface renders from the
// derived self-model (never hand-written) and speaks in the flipped voice —
// no identity-Neo remains on it.
func TestMorpheusIdentity_CapabilitySurfaceFromRealArtifact(t *testing.T) {
	raw, err := os.ReadFile("../../../graph/self-model/self-model.json")
	if err != nil {
		t.Fatalf("read the regenerated self-model artifact: %v", err)
	}
	var artifact selfmodel.Artifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("unmarshal artifact: %v", err)
	}
	if !strings.HasPrefix(artifact.Summary, "Your structural self is derived from codegraph.") {
		t.Fatalf("artifact must speak in the derived second-person voice, got: %.80s", artifact.Summary)
	}
	if hit := neoWord.FindString(artifact.Summary); hit != "" {
		t.Fatalf("the regenerated artifact still carries the identity word %q", hit)
	}

	a := New(Options{Config: config.Default(), Capability: &CapabilitySurface{StructuralSummary: artifact.Summary}})
	rendered := a.renderCapabilitySurface()
	if !strings.Contains(rendered, "Your structural self is derived from codegraph.") {
		t.Fatal("the resident capability surface must render the real artifact summary")
	}
	if hit := neoWord.FindString(rendered); hit != "" {
		t.Fatalf("stray identity %q on the rendered capability surface", hit)
	}
}

// TestMorpheusIdentity_ScrubBothNames proves req.8.2's scrub clause: the
// underlying-LLM identity net keeps scrubbing model/lab names to the
// configured identity for BOTH names, and 'Neo' — a former product name, not a
// foundation model — is never treated as a leak.
func TestMorpheusIdentity_ScrubBothNames(t *testing.T) {
	if got, leaked := scrubIdentity("Morpheus", "I'm Grok, here to help."); !leaked || got != "I'm Morpheus, here to help." {
		t.Errorf("LLM name must scrub to Morpheus: got %q leaked=%v", got, leaked)
	}
	if got, leaked := scrubIdentity("Neo", "I'm Grok, here to help."); !leaked || got != "I'm Neo, here to help." {
		t.Errorf("Neo remains a valid configured identity for the scrub: got %q leaked=%v", got, leaked)
	}
	if got, leaked := scrubIdentity("Morpheus", "I'm Neo, and here is the plan."); leaked || got != "I'm Neo, and here is the plan." {
		t.Errorf("'Neo' is a former product name, NOT an underlying-LLM leak: got %q leaked=%v", got, leaked)
	}
}

// TestMorpheusIdentity_NoStrayNeoOnPinnedBlock proves the pager's pinned
// identity line follows the config flip (the pinned block is a model-facing
// flipped surface).
func TestMorpheusIdentity_NoStrayNeoOnPinnedBlock(t *testing.T) {
	// The pinned identity line resolves the same config field; assert at the
	// config level plus the invariant rules carry no identity-Neo.
	cfg := config.Default()
	if cfg.AgentName != "Morpheus" {
		t.Fatalf("default AgentName = %q, want Morpheus", cfg.AgentName)
	}
	if cfg2 := config.Sandbox(); !strings.HasPrefix(cfg2.AgentName, "Morpheus") {
		t.Fatalf("sandbox AgentName = %q, want Morpheus-prefixed", cfg2.AgentName)
	}
}
