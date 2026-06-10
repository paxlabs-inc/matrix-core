// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"

	cmem "matrix/cortex/memory"

	"matrix/neo/internal/config"
)

// these tests open a real cortex store under t.TempDir() — fully offline (the
// hash embedder needs no network) and hermetic.

func testCfg(t *testing.T) config.Config {
	t.Helper()
	c := config.Default()
	c.CortexRoot = t.TempDir()
	c.CortexActor = "neo-store-test"
	return c
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestPagerWriteRetrieveAndCoverageGate(t *testing.T) {
	cfg := testCfg(t)
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	if _, err := p.RememberFact(ctx, "the dev box repo is at /root/matrix"); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	if _, err := p.RecordOutcome(ctx, "built neo green", OutcomeSuccess, ""); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	proven := PatternSpec{Name: "deploy erc20", Steps: []string{"compile", "deploy"}}
	if _, err := p.WritePattern(ctx, proven, 0.9, cfg.MinPatternSuccesses, nil); err != nil {
		t.Fatalf("WritePattern proven: %v", err)
	}
	candidate := PatternSpec{Name: "rare flow", Steps: []string{"x"}}
	if _, err := p.WritePattern(ctx, candidate, 0.5, 1, nil); err != nil {
		t.Fatalf("WritePattern candidate: %v", err)
	}

	// Empty query forces the deterministic type-filtered path (independent of
	// async embedding freshness).
	snips, err := p.Retrieve(ctx, "")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(snips) == 0 {
		t.Error("expected the written fact/outcome/patterns back from Retrieve")
	}

	pats, err := p.Procedural(ctx, "")
	if err != nil {
		t.Fatalf("Procedural: %v", err)
	}
	var names []string
	for _, pt := range pats {
		names = append(names, pt.Spec.Name)
	}
	if !contains(names, "deploy erc20") {
		t.Errorf("proven pattern (coverage=%d >= gate) should inject; got %v", cfg.MinPatternSuccesses, names)
	}
	if contains(names, "rare flow") {
		t.Errorf("under-covered candidate must NOT inject (anti-overfit); got %v", names)
	}
}

func TestReinforcePatternGraduatesPastGate(t *testing.T) {
	cfg := testCfg(t)
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	spec := PatternSpec{Name: "repeatable recipe", Steps: []string{"a", "b"}}
	for i := 0; i < cfg.MinPatternSuccesses; i++ {
		if _, err := p.ReinforcePattern(ctx, spec, nil); err != nil {
			t.Fatalf("ReinforcePattern #%d: %v", i, err)
		}
	}

	pats, err := p.Procedural(ctx, "")
	if err != nil {
		t.Fatalf("Procedural: %v", err)
	}
	found := false
	for _, pt := range pats {
		if pt.Spec.Name == "repeatable recipe" {
			found = true
			if pt.Coverage < cfg.MinPatternSuccesses {
				t.Errorf("coverage = %d, want >= %d after reinforcement", pt.Coverage, cfg.MinPatternSuccesses)
			}
		}
	}
	if !found {
		t.Error("a reinforced pattern should graduate past the anti-overfit gate")
	}
}

func TestPinnedIncludesIdentityRulesAndHardConstraint(t *testing.T) {
	cfg := testCfg(t)
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	if _, err := p.cortex.Write(p.head(9),
		cmem.ConstraintData{
			SchemaVersion: 1,
			Statement:     "never wipe prod chain state",
			Polarity:      cmem.PolarityDont,
			StrengthVal:   cmem.StrengthHard,
			Source:        cmem.ConstraintSourceUserDeclared,
		},
		p.writeMeta()); err != nil {
		t.Fatalf("write constraint: %v", err)
	}
	if _, err := p.cortex.Write(p.head(9),
		cmem.IdentityData{SchemaVersion: 1, Name: "Neo", DID: "did:matrix:neo:abcd1234"},
		p.writeMeta()); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	pinned := p.Pinned(ctx, "")
	if !strings.Contains(pinned, "Inviolable operating rules") {
		t.Error("pinned block must always carry the baked invariant rules")
	}
	if !strings.Contains(pinned, "never wipe prod chain state") {
		t.Error("a hard constraint from cortex should be pinned")
	}
	if !strings.Contains(pinned, "did:matrix:neo:abcd1234") {
		t.Error("the identity DID should be pinned")
	}
}

func TestUserProfilePinnedAndDeduped(t *testing.T) {
	cfg := testCfg(t)
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	if _, err := p.RememberUserFact(ctx, "The user's name is Andrew"); err != nil {
		t.Fatalf("RememberUserFact: %v", err)
	}
	// A generic (non-user) fact must NOT enter the profile.
	if _, err := p.RememberFact(ctx, "The repo uses Go 1.26"); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	// Repeats (even with case/spacing drift) dedupe instead of duplicating.
	if _, err := p.RememberUserFact(ctx, "  the user's NAME is Andrew "); err != nil {
		t.Fatalf("RememberUserFact dup: %v", err)
	}

	profile := p.UserProfile(ctx)
	if len(profile) != 1 {
		t.Fatalf("UserProfile = %v, want exactly the one deduped user fact", profile)
	}
	if profile[0] != "The user's name is Andrew" {
		t.Errorf("profile[0] = %q", profile[0])
	}

	pinned := p.Pinned(ctx, "")
	if !strings.Contains(pinned, "What you know about your user:") ||
		!strings.Contains(pinned, "The user's name is Andrew") {
		t.Errorf("pinned block should carry the user profile, got:\n%s", pinned)
	}
	if strings.Contains(pinned, "Go 1.26") {
		t.Error("generic facts must not leak into the pinned profile")
	}
}

func TestActiveGoalRoundTrip(t *testing.T) {
	cfg := testCfg(t)
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	if p.ActiveGoal() != "" {
		t.Error("a fresh pager should carry no active goal")
	}
	p.SetActiveGoal("  ship neo  ")
	if got := p.ActiveGoal(); got != "ship neo" {
		t.Errorf("active goal not trimmed/stored: %q", got)
	}
}
