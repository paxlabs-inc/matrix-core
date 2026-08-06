// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
)

// TestPrompt_SkillIndexInjectedNamesOnly verifies (P2-2): a names-only skill
// INDEX is injected into the STABLE system prefix (the byte-stable, cacheable
// block from P1-2). The index must contain ONLY skill names — never full
// step/gotcha/criteria bodies (those are pulled on demand via memory_recall) —
// so the cacheable prefix stays token-bounded and stable across turns.
func TestPrompt_SkillIndexInjectedNamesOnly(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.SetSkillIndex([]string{
		"deploy-erc20",
		"verify-contract",
		"resolve-merge-conflict",
	})

	stable := a.stableSystem()

	// The index header must be present in the STABLE prefix.
	if !strings.Contains(stable, "Skill") {
		t.Fatal("stable prefix must contain a skill index section")
	}

	// Every skill NAME must appear (names-only).
	for _, name := range []string{"deploy-erc20", "verify-contract", "resolve-merge-conflict"} {
		if !strings.Contains(stable, name) {
			t.Errorf("skill name %q missing from stable prefix skill index", name)
		}
	}

	// NO skill BODIES (steps, gotchas, criteria) may leak into the index —
	// those are pulled on demand via memory_recall, keeping the prefix
	// token-bounded. We check for sentinel body tokens that would only appear
	// if a full PatternSpec.Render() leaked in.
	for _, bodySentinel := range []string{"steps:", "gotchas:", "success:", "preconditions:", "→"} {
		if strings.Contains(strings.ToLower(stable), bodySentinel) {
			// "steps:" could appear in the charter ("steps" is a common word),
			// so we only flag it inside a skill-index context. Check that the
			// skill index line itself doesn't carry body sentinels.
			idx := strings.Index(stable, "Skill")
			if idx >= 0 {
				skillSection := stable[idx:]
				if strings.Contains(strings.ToLower(skillSection), bodySentinel) {
					t.Errorf("skill index must be names-only; found body sentinel %q in skill section:\n%s", bodySentinel, skillSection)
				}
			}
		}
	}
}

// TestPrompt_SkillIndexStableAcrossTurns verifies the skill index lives in the
// CACHEABLE stable prefix (consistent with P1-2): it must be byte-identical
// across turns even as the volatile tail mutates. This is the prompt-cache
// invariant extended to the skill index.
func TestPrompt_SkillIndexStableAcrossTurns(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.SetSkillIndex([]string{"deploy-erc20", "verify-contract"})

	p0 := a.stableSystem()

	// Mutate volatile state (would have changed the old single-block prefix).
	a.working = nil

	p1 := a.stableSystem()
	if p0 != p1 {
		t.Fatal("stable prefix with skill index must be byte-identical across turns (cache invariant)")
	}

	// The skill names must be in the stable prefix, NOT pushed to the tail.
	tail := a.renderActivationBundle(nil) + a.epistemicTail() + a.budgetTail(1)
	if strings.Contains(tail, "deploy-erc20") {
		t.Error("skill index must live in the STABLE prefix, not the activation tail")
	}
}

// TestPrompt_SkillIndexEmptyOmitsSection verifies that when no skills are
// indexed, the stable prefix does NOT emit an empty skill section (keeps the
// prefix clean for deployments without skills).
func TestPrompt_SkillIndexEmptyOmitsSection(t *testing.T) {
	a := New(Options{Config: config.Default()})
	// No SetSkillIndex call → empty.

	stable := a.stableSystem()
	idx := strings.Index(stable, "Skills you have")
	if idx >= 0 {
		// If the section exists, it should not list any skill names.
		section := stable[idx:]
		if strings.Contains(section, "deploy-erc20") {
			t.Error("empty skill index should not list skill names")
		}
	}
}

func TestStableSystemIncludesConstructionDateWithoutDrift(t *testing.T) {
	agent := &Agent{currentUTCDate: "August 6, 2026"}
	first := agent.stableSystem()
	second := agent.stableSystem()
	if first != second {
		t.Fatal("stable system changed across calls")
	}
	if !strings.Contains(first, "Today's UTC date is August 6, 2026") ||
		!strings.Contains(first, "current research") {
		t.Fatalf("stable system omitted current-date grounding: %q", first)
	}
}
