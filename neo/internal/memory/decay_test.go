// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"testing"
	"time"

	cmem "matrix/cortex/memory"
)

// Identity never decays: its multiplier is exactly 1.0 no matter how stale.
func TestRecencyMultiplierIdentityNeverDecays(t *testing.T) {
	now := time.Unix(1_000_000_000, 0).UTC()
	old := now.Add(-10 * 365 * 24 * time.Hour).UnixNano() // a decade ago
	if m := recencyMultiplier(cmem.TypeIdentity.String(), old, now); m != 1.0 {
		t.Errorf("Identity multiplier = %v, want 1.0 (no decay)", m)
	}
}

// Unlisted / uncovered types (Event, Pattern, Capability, Unknown) are left
// undecayed so the re-rank never penalizes a type the table doesn't cover.
func TestRecencyMultiplierUncoveredTypesUndecayed(t *testing.T) {
	now := time.Unix(1_000_000_000, 0).UTC()
	old := now.Add(-100 * 24 * time.Hour).UnixNano()
	for _, typ := range []string{
		cmem.TypeEvent.String(), cmem.TypePattern.String(),
		cmem.TypeCapability.String(), "Unknown",
	} {
		if m := recencyMultiplier(typ, old, now); m != 1.0 {
			t.Errorf("%s multiplier = %v, want 1.0 (uncovered → no decay)", typ, m)
		}
	}
}

// Missing or future timestamps are treated as fresh (1.0), never a penalty.
func TestRecencyMultiplierMissingOrFutureTimestamp(t *testing.T) {
	now := time.Unix(1_000_000_000, 0).UTC()
	if m := recencyMultiplier(cmem.TypeFact.String(), 0, now); m != 1.0 {
		t.Errorf("zero timestamp multiplier = %v, want 1.0", m)
	}
	future := now.Add(24 * time.Hour).UnixNano()
	if m := recencyMultiplier(cmem.TypeFact.String(), future, now); m != 1.0 {
		t.Errorf("future timestamp multiplier = %v, want 1.0", m)
	}
}

// At exactly one half-life of age, a decaying type's multiplier is ~0.5.
func TestRecencyMultiplierHalfLifePoint(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	// Fact half-life is 30 days; one half-life ago must read ≈ 0.5.
	old := now.Add(-30 * 24 * time.Hour).UnixNano()
	m := recencyMultiplier(cmem.TypeFact.String(), old, now)
	if m < 0.49 || m > 0.51 {
		t.Errorf("Fact at one half-life = %v, want ≈0.5", m)
	}
}

// The acceptance property: two equal-cosine candidates of different types, at
// the SAME recency gap, rank by half-life — the longer-lived type keeps more
// of its score. Constraint (365d) > Preference (90d) > Fact/Goal (30d) >
// Belief (7d).
func TestRecencyMultiplierLongerHalfLifeRanksHigher(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	gap := now.Add(-60 * 24 * time.Hour).UnixNano() // 60-day recency gap

	constraint := recencyMultiplier(cmem.TypeConstraint.String(), gap, now)
	preference := recencyMultiplier(cmem.TypePreference.String(), gap, now)
	fact := recencyMultiplier(cmem.TypeFact.String(), gap, now)
	goal := recencyMultiplier(cmem.TypeGoal.String(), gap, now)
	belief := recencyMultiplier(cmem.TypeBelief.String(), gap, now)

	if !(constraint > preference && preference > fact && fact > belief) {
		t.Errorf("half-life ordering broken: constraint=%v preference=%v fact=%v belief=%v",
			constraint, preference, fact, belief)
	}
	if fact != goal {
		t.Errorf("Fact and Goal share a 30d half-life: fact=%v goal=%v", fact, goal)
	}
	// All decaying types stay strictly within (0,1] at a finite gap.
	for _, m := range []float32{constraint, preference, fact, belief} {
		if m <= 0 || m > 1 {
			t.Errorf("multiplier %v out of (0,1]", m)
		}
	}
}
