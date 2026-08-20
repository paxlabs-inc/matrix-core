// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"bytes"
	"testing"
	"time"

	"centra/core/cortex"
	"centra/core/cortex/cmharness"
	"centra/core/cortex/memory"
)

// cascadeSince/cascadeUntil bound the Rollups scans in these tests: all seeded
// window starts are positive and precede `now`, so [0, now] covers every
// built rollup of any tier.
const cascadeSince int64 = 0

// seedCascade writes real anchored Preferences (+ one Attest) at fixed clock
// instants that land in KNOWN hour/day windows across MORE THAN ONE day:
//
//	baseHour            -> hour H1, day D1  (importance 9, top salience)
//	baseHour + 2h       -> hour H2, day D1  (importance 5)
//	baseHour + 24h      -> hour H3, day D2  (importance 7)
//
// So the closed-window ladder is: 3 hour rollups, 2 day rollups (D1 has 2 hour
// children, D2 has 1), and at least one epoch rollup over the day rollups.
func seedCascade(t *testing.T, c *cortex.Cortex, clk *mutClock) {
	t.Helper()
	clk.t = baseHour
	topURI := writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(2 * time.Hour)
	writePrefAt(t, c, "beta", 5)
	clk.t = baseHour.Add(24 * time.Hour)
	writePrefAt(t, c, "gamma", 7)

	// An attest inside H1 so the aggregate OutcomeTally is exercised.
	clk.t = baseHour.Add(5 * time.Minute)
	if _, err := c.Attest(cortex.AttestOpts{
		IntentID:  "intent-cascade",
		Outcome:   cortex.AttestOutcomeSuccess,
		Cited:     []memory.URI{topURI},
		CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
}

// TestCascadeBuildsLadder proves req.4.4 + req.3.3: Cascade(TierEpoch, now)
// builds hour rollups from the journal, day rollups that AGGREGATE their hour
// rollups (Members reference the constituent HOUR rollup URIs, Ref.Kind ==
// "rollup"), and an epoch rollup that references its day rollups. Aggregate
// EntryCount at each day == sum of its child hour EntryCounts.
func TestCascadeBuildsLadder(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	if err := c.Cascade(cortex.TierEpoch, now); err != nil {
		t.Fatalf("Cascade: %v", err)
	}

	// --- hour tier: one rollup per closed hour that had entries -----------
	hours, err := c.Rollups(cortex.TierHour, cascadeSince, now)
	if err != nil {
		t.Fatalf("Rollups(hour): %v", err)
	}
	if len(hours) != 3 {
		t.Fatalf("hour rollups = %d, want 3 (H1,H2,H3)", len(hours))
	}
	// Every hour rollup's members resolve to MEMORIES (finest tier).
	for _, hr := range hours {
		for _, m := range hr.Members {
			if m.Kind != cortex.RefKindMemory {
				t.Fatalf("hour rollup member Kind = %q, want %q", m.Kind, cortex.RefKindMemory)
			}
		}
	}

	// Index the hour rollups by their URI + by day bucket.
	hourURI := map[memory.URI]cortex.RollupRecord{}
	hoursByDay := map[int64][]cortex.RollupRecord{}
	for _, hr := range hours {
		hourURI[cortex.BuildRollupURI(cortex.TierHour, hr.Window.Start)] = hr
		day := cortex.DayWindow(hr.Window.Start)
		hoursByDay[day.Start] = append(hoursByDay[day.Start], hr)
	}

	// --- day tier: aggregates of the hour rollups (req.3.3) ---------------
	days, err := c.Rollups(cortex.TierDay, cascadeSince, now)
	if err != nil {
		t.Fatalf("Rollups(day): %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("day rollups = %d, want 2 (D1,D2)", len(days))
	}

	dayURISet := map[memory.URI]bool{}
	for _, dr := range days {
		dayURISet[cortex.BuildRollupURI(cortex.TierDay, dr.Window.Start)] = true

		// Members of a day rollup MUST reference its constituent HOUR
		// rollups (Ref.Kind == "rollup"), and the referenced set must equal
		// the set of hour rollups inside that day window.
		gotMembers := map[memory.URI]bool{}
		for _, m := range dr.Members {
			if m.Kind != cortex.RefKindRollup {
				t.Fatalf("day rollup member Kind = %q, want %q", m.Kind, cortex.RefKindRollup)
			}
			if _, ok := hourURI[m.URI]; !ok {
				t.Fatalf("day rollup member %q does not resolve to a built HOUR rollup", m.URI)
			}
			gotMembers[m.URI] = true
		}
		wantMembers := map[memory.URI]bool{}
		var sumEntries uint32
		for _, hr := range hoursByDay[dr.Window.Start] {
			wantMembers[cortex.BuildRollupURI(cortex.TierHour, hr.Window.Start)] = true
			sumEntries += hr.EntryCount
		}
		if len(gotMembers) != len(wantMembers) {
			t.Fatalf("day %d members = %d, want %d hour children", dr.Window.Start, len(gotMembers), len(wantMembers))
		}
		for u := range wantMembers {
			if !gotMembers[u] {
				t.Fatalf("day %d missing hour child member %q", dr.Window.Start, u)
			}
		}
		// req.4.4 aggregation: day EntryCount == sum of child hour counts.
		if dr.EntryCount != sumEntries {
			t.Fatalf("day %d EntryCount = %d, want sum(child hour EntryCount) = %d",
				dr.Window.Start, dr.EntryCount, sumEntries)
		}
		if dr.ShortForm == "" {
			t.Fatalf("day %d ShortForm empty", dr.Window.Start)
		}
	}

	// --- epoch tier: aggregates of the day rollups (req.3.3) --------------
	epochs, err := c.Rollups(cortex.TierEpoch, cascadeSince, now)
	if err != nil {
		t.Fatalf("Rollups(epoch): %v", err)
	}
	if len(epochs) < 1 {
		t.Fatalf("epoch rollups = %d, want >= 1", len(epochs))
	}
	epochMembers := map[memory.URI]bool{}
	for _, er := range epochs {
		for _, m := range er.Members {
			if m.Kind != cortex.RefKindRollup {
				t.Fatalf("epoch rollup member Kind = %q, want %q", m.Kind, cortex.RefKindRollup)
			}
			if !dayURISet[m.URI] {
				t.Fatalf("epoch rollup member %q does not resolve to a built DAY rollup", m.URI)
			}
			epochMembers[m.URI] = true
		}
	}
	// Every day rollup is referenced by some epoch rollup.
	if len(epochMembers) != len(dayURISet) {
		t.Fatalf("epoch members reference %d day rollups, want all %d", len(epochMembers), len(dayURISet))
	}
}

// TestCascadeIdempotent proves req.4.1: a second Cascade over UNCHANGED store
// state appends ZERO new journal entries and yields a byte-identical day
// rollup record (same RecordHash).
func TestCascadeIdempotent(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	if err := c.Cascade(cortex.TierEpoch, now); err != nil {
		t.Fatalf("Cascade #1: %v", err)
	}

	days, err := c.Rollups(cortex.TierDay, cascadeSince, now)
	if err != nil || len(days) == 0 {
		t.Fatalf("Rollups(day): len=%d err=%v", len(days), err)
	}
	dayStart := days[0].Window.Start

	before := c.Store().JournalCount()
	rec1, err := c.LoadRollup(cortex.TierDay, dayStart)
	if err != nil {
		t.Fatalf("LoadRollup(day) #1: %v", err)
	}
	b1, err := cortex.EncodeRollupRecord(rec1)
	if err != nil {
		t.Fatalf("encode rec1: %v", err)
	}

	// Second Cascade over identical state, wall clock advanced far ahead.
	clk.t = baseHour.Add(365 * 24 * time.Hour)
	if err := c.Cascade(cortex.TierEpoch, now); err != nil {
		t.Fatalf("Cascade #2: %v", err)
	}
	after := c.Store().JournalCount()
	if after != before {
		t.Fatalf("JournalCount changed across idempotent Cascade: before=%d after=%d (want equal)", before, after)
	}

	rec2, err := c.LoadRollup(cortex.TierDay, dayStart)
	if err != nil {
		t.Fatalf("LoadRollup(day) #2: %v", err)
	}
	b2, err := cortex.EncodeRollupRecord(rec2)
	if err != nil {
		t.Fatalf("encode rec2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("day rollup not byte-identical across idempotent Cascade:\n #1 %x\n #2 %x", b1, b2)
	}
}

// TestCascadeDeterministic proves req.4.2: a coarse rollup is a pure function
// of its inputs. Two INDEPENDENT cortex instances seeded with identical
// entries at identical clock instants produce byte-identical day rollups —
// wall-clock independence and rebuildability from the journal.
func TestCascadeDeterministic(t *testing.T) {
	ca, clka := openRollupCortex(t)
	cb, clkb := openRollupCortex(t)
	seedCascade(t, ca, clka)
	seedCascade(t, cb, clkb)

	// Build the two cascades at DIFFERENT wall-now values to prove no wall
	// clock leaks into the stored record.
	nowA := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	nowB := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	clka.t = baseHour.Add(11 * 24 * time.Hour)
	clkb.t = baseHour.Add(500 * 24 * time.Hour)
	if err := ca.Cascade(cortex.TierEpoch, nowA); err != nil {
		t.Fatalf("Cascade A: %v", err)
	}
	if err := cb.Cascade(cortex.TierEpoch, nowB); err != nil {
		t.Fatalf("Cascade B: %v", err)
	}

	daysA, err := ca.Rollups(cortex.TierDay, cascadeSince, nowA)
	if err != nil || len(daysA) == 0 {
		t.Fatalf("Rollups A(day): len=%d err=%v", len(daysA), err)
	}
	for _, dr := range daysA {
		recB, err := cb.LoadRollup(cortex.TierDay, dr.Window.Start)
		if err != nil {
			t.Fatalf("LoadRollup B(day %d): %v", dr.Window.Start, err)
		}
		ba, err := cortex.EncodeRollupRecord(&dr)
		if err != nil {
			t.Fatalf("encode A day %d: %v", dr.Window.Start, err)
		}
		bb, err := cortex.EncodeRollupRecord(recB)
		if err != nil {
			t.Fatalf("encode B day %d: %v", dr.Window.Start, err)
		}
		if !bytes.Equal(ba, bb) {
			t.Fatalf("day rollup %d not byte-identical across independent instances:\n A %x\n B %x",
				dr.Window.Start, ba, bb)
		}
	}
}

// TestCascadeDerivedLaneSafety proves req.3.2 posture for the whole cascade:
// Cascade(TierEpoch,...) performs NO anchored-namespace SMT write, and the
// full OverallRoot rebuilds byte-identically with cascade rollup entries
// present in the journal.
func TestCascadeDerivedLaneSafety(t *testing.T) {
	c, clk := openRollupCortex(t)
	seedCascade(t, c, clk)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	clk.t = baseHour.Add(11 * 24 * time.Hour)
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		return c.Cascade(cortex.TierEpoch, now)
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across Cascade: %v", err)
	}

	res, err := cmharness.ReplayPreservesRoot(c, nil)
	if err != nil {
		t.Fatalf("ReplayPreservesRoot with cascade entries present: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
}
