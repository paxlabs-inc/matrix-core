// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package writeback

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

// TestConsolidator_ProposesSkillAfterNSuccesses verifies the auto-propose
// extension (P2-2): when a pattern is reinforced past the existing confidence/
// N-success guard (cfg.MinPatternSuccesses), the consolidator PROPOSES it as a
// candidate skill. A pattern below the guard must NOT be proposed (anti-overfit).
// This tests the propose logic directly without the LLM round-trip, isolating
// the guard from the extraction path.
func TestConsolidator_ProposesSkillAfterNSuccesses(t *testing.T) {
	cfg := config.Default()
	cfg.MinPatternSuccesses = 3
	c := &Consolidator{cfg: cfg}

	spec := memory.PatternSpec{
		Name:  "deploy ERC-20",
		Steps: []string{"compile", "deploy"},
	}

	// Below the guard: coverage 1 and 2 must NOT propose.
	c.proposeIfReady(spec, 1)
	if proposed := c.ProposedSkills(); len(proposed) != 0 {
		t.Errorf("coverage 1 (< guard %d) must not propose; got %v", cfg.MinPatternSuccesses, proposed)
	}
	c.proposeIfReady(spec, 2)
	if proposed := c.ProposedSkills(); len(proposed) != 0 {
		t.Errorf("coverage 2 (< guard %d) must not propose; got %v", cfg.MinPatternSuccesses, proposed)
	}

	// AT the guard: coverage == MinPatternSuccesses must propose.
	c.proposeIfReady(spec, cfg.MinPatternSuccesses)
	proposed := c.ProposedSkills()
	if len(proposed) != 1 {
		t.Fatalf("coverage == guard %d must propose exactly 1 skill; got %v", cfg.MinPatternSuccesses, proposed)
	}
	if proposed[0] != "deploy ERC-20" {
		t.Errorf("proposed skill name = %q, want %q", proposed[0], "deploy ERC-20")
	}

	// Re-proposing the SAME pattern must NOT duplicate the proposal (dedup).
	c.proposeIfReady(spec, cfg.MinPatternSuccesses+1)
	proposed = c.ProposedSkills()
	if len(proposed) != 1 {
		t.Errorf("re-proposing the same pattern must dedup, not append; got %v", proposed)
	}

	// A DIFFERENT pattern crossing the guard proposes a second distinct skill.
	spec2 := memory.PatternSpec{
		Name:  "verify contract",
		Steps: []string{"cast call", "check balance"},
	}
	c.proposeIfReady(spec2, cfg.MinPatternSuccesses)
	proposed = c.ProposedSkills()
	if len(proposed) != 2 {
		t.Fatalf("a second distinct pattern must add a second proposal; got %v", proposed)
	}
	if proposed[1] != "verify contract" {
		t.Errorf("second proposed name = %q, want %q", proposed[1], "verify contract")
	}
}

// TestSkillLoop_NoOverfitSingleSuccess verifies the anti-overfit guard
// (procedural.guards): a SINGLE success must NOT trigger a skill proposal.
// Only after the pattern repeats past cfg.MinPatternSuccesses does it become a
// candidate. This is the core guard that prevents the agent from overfitting a
// one-off tool sequence into a "skill".
func TestSkillLoop_NoOverfitSingleSuccess(t *testing.T) {
	cfg := config.Default()
	cfg.MinPatternSuccesses = 3
	c := &Consolidator{cfg: cfg}

	spec := memory.PatternSpec{
		Name:  "one-off flow",
		Steps: []string{"rare step"},
	}

	// A single success (coverage 1) must NOT propose — the guard requires
	// MinPatternSuccesses (3) repetitions before a candidate becomes active.
	c.proposeIfReady(spec, 1)
	if proposed := c.ProposedSkills(); len(proposed) != 0 {
		t.Errorf("single success must NOT propose (anti-overfit); got %v", proposed)
	}

	// Even two successes (below the guard) must not propose.
	c.proposeIfReady(spec, 2)
	if proposed := c.ProposedSkills(); len(proposed) != 0 {
		t.Errorf("two successes (< guard %d) must NOT propose; got %v", cfg.MinPatternSuccesses, proposed)
	}

	// Only when it reaches the guard does it propose.
	c.proposeIfReady(spec, cfg.MinPatternSuccesses)
	if proposed := c.ProposedSkills(); len(proposed) != 1 {
		t.Errorf("coverage == guard should propose; got %v", proposed)
	}
}

// TestConsolidator_ProposeEmptySpecRejected verifies that an empty spec (no
// name, trigger, or steps) is never proposed — it carries no usable identity.
func TestConsolidator_ProposeEmptySpecRejected(t *testing.T) {
	cfg := config.Default()
	c := &Consolidator{cfg: cfg}

	c.proposeIfReady(memory.PatternSpec{}, 100)
	if proposed := c.ProposedSkills(); len(proposed) != 0 {
		t.Errorf("empty spec must never be proposed; got %v", proposed)
	}
}

// TestProposedSkillsDedupByDedupKey verifies that proposals dedup by the
// PatternSpec dedup key (name → trigger → steps), not by the raw coverage
// count, so reinforcing the same logical recipe multiple times yields one
// proposal.
func TestProposedSkillsDedupByDedupKey(t *testing.T) {
	cfg := config.Default()
	cfg.MinPatternSuccesses = 2
	c := &Consolidator{cfg: cfg}

	// Two specs with the same NAME but different step phrasings share a dedup
	// key (name wins) and must produce a single proposal.
	a := memory.PatternSpec{Name: "same skill", Steps: []string{"a", "b"}}
	b := memory.PatternSpec{Name: "same skill", Steps: []string{"x", "y"}}

	c.proposeIfReady(a, cfg.MinPatternSuccesses)
	c.proposeIfReady(b, cfg.MinPatternSuccesses)

	if proposed := c.ProposedSkills(); len(proposed) != 1 {
		t.Errorf("same-name specs must dedup to 1 proposal; got %v", proposed)
	}
	if len(strings.Join(c.ProposedSkills(), "")) == 0 {
		t.Error("proposed skills should be non-empty")
	}
}

// Ensure the Consolidator zero-value is safe to call (no panic).
func TestConsolidator_ProposeNilSafe(t *testing.T) {
	var c *Consolidator // nil receiver
	// These must not panic on a nil consolidator.
	c.proposeIfReady(memory.PatternSpec{Name: "x"}, 5)
	if got := c.ProposedSkills(); len(got) != 0 {
		t.Errorf("nil consolidator should report no proposals; got %v", got)
	}
}
