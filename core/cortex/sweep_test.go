// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"testing"
	"time"

	"centra/core/cortex"
	"centra/core/cortex/cmharness"
)

// TestSweepNowBuildsTheLadder proves SweepNow (the Chronos-sweep entry point,
// req.5.1) performs the SAME work as calling Cascade(TierEpoch, now)
// directly: the full hour -> day -> epoch ladder is materialized.
func TestSweepNowBuildsTheLadder(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	if err := c.SweepNow(now); err != nil {
		t.Fatalf("SweepNow: %v", err)
	}

	hours, err := c.Rollups(cortex.TierHour, cascadeSince, now)
	if err != nil || len(hours) != 3 {
		t.Fatalf("hour rollups after SweepNow = %d err=%v, want 3", len(hours), err)
	}
	days, err := c.Rollups(cortex.TierDay, cascadeSince, now)
	if err != nil || len(days) != 2 {
		t.Fatalf("day rollups after SweepNow = %d err=%v, want 2", len(days), err)
	}
	epochs, err := c.Rollups(cortex.TierEpoch, cascadeSince, now)
	if err != nil || len(epochs) < 1 {
		t.Fatalf("epoch rollups after SweepNow = %d err=%v, want >= 1", len(epochs), err)
	}
}

// TestSweepNowIdempotentAcrossCadence proves req.5.1 "bounded work per
// sweep": firing SweepNow again over UNCHANGED store state (as a repeated
// Chronos cron fire would) appends ZERO new journal entries, regardless of
// how far the wall clock has advanced between fires.
func TestSweepNowIdempotentAcrossCadence(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	if err := c.SweepNow(now); err != nil {
		t.Fatalf("SweepNow #1: %v", err)
	}
	before := c.Store().JournalCount()

	// A later Chronos fire, wall clock advanced, same closed-window horizon.
	clk.t = baseHour.Add(365 * 24 * time.Hour)
	if err := c.SweepNow(now); err != nil {
		t.Fatalf("SweepNow #2: %v", err)
	}
	after := c.Store().JournalCount()
	if after != before {
		t.Fatalf("JournalCount changed across idempotent SweepNow fires: before=%d after=%d (want equal)", before, after)
	}
}

// TestSweepNowDerivedLaneSafety proves req.5.3: the write SweepNow performs
// happens ENTIRELY inside cortex on the derived lane — no anchored
// "memories"/"edges" SMT write. A Chronos trigger of this call therefore
// cannot touch the anchored world-state, regardless of what Chronos itself
// is or is not trusted with.
func TestSweepNowDerivedLaneSafety(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	clk.t = baseHour.Add(11 * 24 * time.Hour)
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		return c.SweepNow(now)
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across SweepNow: %v", err)
	}
}
