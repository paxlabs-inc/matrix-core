// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/cmharness"
	"matrix/cortex/memory"
)

// TestRepairRollupBuildsMissingHour proves req.5.2 for the finest tier: a
// read via RepairRollup over a closed hour window that has NEVER been rolled
// (BuildRollup/Cascade never called) triggers an idempotent rebuild, and the
// result is byte-identical to calling BuildRollup directly on an
// independent instance seeded identically (the eager path).
func TestRepairRollupBuildsMissingHour(t *testing.T) {
	eager, clkEager := openRollupCortex(t)
	lazy, clkLazy := openRollupCortex(t)

	clkEager.t = baseHour
	writePrefAt(t, eager, "alpha", 6)
	clkLazy.t = baseHour
	writePrefAt(t, lazy, "alpha", 6)

	w := cortex.HourWindow(baseHour.UnixNano())
	now := baseHour.Add(90 * time.Minute).UnixNano()
	clkEager.t = time.Unix(0, now)
	clkLazy.t = time.Unix(0, now)

	// Eager path: BuildRollup directly (what Cascade/SweepNow would do).
	if _, err := eager.BuildRollup(w); err != nil {
		t.Fatalf("eager BuildRollup: %v", err)
	}
	wantRec, err := eager.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("eager LoadRollup: %v", err)
	}
	wantBytes, err := cortex.EncodeRollupRecord(wantRec)
	if err != nil {
		t.Fatalf("encode want: %v", err)
	}

	// Lazy path: no BuildRollup/Cascade call at all — RepairRollup must
	// perform the rebuild on read.
	if _, err := lazy.LoadRollup(cortex.TierHour, w.Start); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("lazy instance must have no rollup yet, got err=%v", err)
	}
	gotRec, err := lazy.RepairRollup(w, now)
	if err != nil {
		t.Fatalf("RepairRollup: %v", err)
	}
	gotBytes, err := cortex.EncodeRollupRecord(gotRec)
	if err != nil {
		t.Fatalf("encode got: %v", err)
	}
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Fatalf("RepairRollup result not byte-identical to the eager BuildRollup path:\n eager %x\n lazy  %x", wantBytes, gotBytes)
	}

	// The repair must have persisted the record (a subsequent LoadRollup
	// finds it without another repair).
	if _, err := lazy.LoadRollup(cortex.TierHour, w.Start); err != nil {
		t.Fatalf("LoadRollup after repair: %v", err)
	}
}

// TestRepairRollupHourAuthoritativeWhenPresent proves the "missing only"
// discipline for the finest tier: when an hour rollup already exists for a
// closed window, RepairRollup returns it UNCHANGED and performs no rebuild
// (no new journal entry) — a closed hour window's journal facts are
// immutable, so the existing record is always authoritative.
func TestRepairRollupHourAuthoritativeWhenPresent(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	clk.t = baseHour
	writePrefAt(t, c, "alpha", 6)
	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}
	before := c.Store().JournalCount()

	now := baseHour.Add(90 * time.Minute).UnixNano()
	rec, err := c.RepairRollup(w, now)
	if err != nil {
		t.Fatalf("RepairRollup: %v", err)
	}
	after := c.Store().JournalCount()
	if after != before {
		t.Fatalf("RepairRollup rebuilt an already-present hour rollup: before=%d after=%d", before, after)
	}
	if rec.EntryCount == 0 {
		t.Fatalf("returned record looks empty: %+v", rec)
	}
}

// TestRepairRollupBuildsMissingCoarseFromExistingChildren proves req.5.2 for
// a coarse tier: when the HOUR children exist (built by Cascade(TierHour,
// ...)) but the day tier was NEVER cascaded, reading the day window via
// RepairRollup triggers an idempotent aggregation that is byte-identical to
// calling Cascade(TierDay, ...) directly on an independently-seeded
// instance (the eager path) — because both share buildCoarseRollup.
func TestRepairRollupBuildsMissingCoarseFromExistingChildren(t *testing.T) {
	eager, clkEager := openRollupCortex(t)
	lazy, clkLazy := openRollupCortex(t)
	seedCascade(t, eager, clkEager)
	seedCascade(t, lazy, clkLazy)

	now := baseHour.Add(10 * 24 * time.Hour).UnixNano()
	dayWindow := cortex.DayWindow(baseHour.UnixNano())

	// Eager path: cascade all the way to the day tier directly.
	clkEager.t = baseHour.Add(11 * 24 * time.Hour)
	if err := eager.Cascade(cortex.TierDay, now); err != nil {
		t.Fatalf("eager Cascade(day): %v", err)
	}
	wantRec, err := eager.LoadRollup(cortex.TierDay, dayWindow.Start)
	if err != nil {
		t.Fatalf("eager LoadRollup(day): %v", err)
	}
	wantBytes, err := cortex.EncodeRollupRecord(wantRec)
	if err != nil {
		t.Fatalf("encode want: %v", err)
	}

	// Lazy path: cascade ONLY the hour tier — the day tier is never built —
	// then read-repair the day window.
	clkLazy.t = baseHour.Add(11 * 24 * time.Hour)
	if err := lazy.Cascade(cortex.TierHour, now); err != nil {
		t.Fatalf("lazy Cascade(hour): %v", err)
	}
	if _, err := lazy.LoadRollup(cortex.TierDay, dayWindow.Start); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("lazy instance must have no day rollup yet, got err=%v", err)
	}
	gotRec, err := lazy.RepairRollup(dayWindow, now)
	if err != nil {
		t.Fatalf("RepairRollup(day): %v", err)
	}
	gotBytes, err := cortex.EncodeRollupRecord(gotRec)
	if err != nil {
		t.Fatalf("encode got: %v", err)
	}
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Fatalf("RepairRollup(day) not byte-identical to the eager Cascade(day) path:\n eager %x\n lazy  %x", wantBytes, gotBytes)
	}
}

// TestRepairRollupCoarseNoChildrenYet proves RepairRollup degrades to
// ErrNotFound (rather than fabricating a record) when a coarse window has no
// finer-tier rollups built at all.
func TestRepairRollupCoarseNoChildrenYet(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	writePrefAt(t, c, "alpha", 5)

	dayWindow := cortex.DayWindow(baseHour.UnixNano())
	now := baseHour.Add(48 * time.Hour).UnixNano()
	if _, err := c.RepairRollup(dayWindow, now); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("RepairRollup(day, no hour children) err = %v, want memory.ErrNotFound", err)
	}
}

// TestRepairRollupRejectsOpenWindow proves RepairRollup never rolls a
// still-open window (mirrors Cascade's windowClosed discipline).
func TestRepairRollupRejectsOpenWindow(t *testing.T) {
	c, _ := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())
	// now is strictly inside the window, not at/after its End.
	now := baseHour.Add(30 * time.Minute).UnixNano()
	if _, err := c.RepairRollup(w, now); err == nil {
		t.Fatal("RepairRollup on an open window must error")
	}
}

// TestRepairRollupDerivedLaneSafety proves the read-repair rebuild performs
// NO anchored-namespace SMT write (req.3.2 posture extended to the lazy
// path).
func TestRepairRollupDerivedLaneSafety(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())
	clk.t = baseHour
	writePrefAt(t, c, "alpha", 5)

	now := baseHour.Add(90 * time.Minute).UnixNano()
	clk.t = time.Unix(0, now)
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, rerr := c.RepairRollup(w, now)
		return rerr
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across RepairRollup: %v", err)
	}
}
