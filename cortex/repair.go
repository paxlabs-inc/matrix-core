// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 3.1: LAZY READ-REPAIR for the temporal ladder
// (req.5.2).
//
// Cascade (cascade.go) is the EAGER half of the ladder's Q4 cascade-trigger
// decision: a Chronos-scheduled sweep (sweep.go's SweepNow) rolls every
// closed window up through the coarsest tier on a cadence. RepairRollup is
// the LAZY half: a reader that encounters a missing or stale rollup for a
// specific closed window calls RepairRollup instead of failing, and gets
// back a record that is BYTE-IDENTICAL to what the eager sweep would have
// produced for the same window and store state — because it shares the
// EXACT SAME builder functions Cascade uses (BuildRollup for the hour tier,
// the package-private buildCoarseRollup for day/epoch), not a parallel
// implementation.
//
// "Stale" only applies to coarse tiers (day/epoch): a coarse record is stale
// when its finer-tier children have changed since it was last built — the
// same compute-compare-skip staleness check buildCoarseRollup already
// performs on every call (cascade.go:324-332), so a stale record is silently
// rewritten to match its current children and an up-to-date one is left
// untouched. An hour rollup, once built for a CLOSED window, is authoritative
// forever (its journal facts are immutable), so "stale" never applies there —
// only "missing" does (mirrors cascadeHours' build-only-if-missing).
//
// Only CLOSED windows (w.End <= now) are ever repaired — an open window is
// never rolled, exactly like Cascade.
package cortex

import (
	"errors"
	"fmt"

	"matrix/cortex/memory"
)

// RepairRollup returns the RollupRecord for the closed window w, performing
// a lazy read-repair (idempotent rebuild) when the persisted record is
// missing or, for a coarse tier, stale relative to its current finer-tier
// children. Returns memory.ErrNotFound when w has no rollup and none can yet
// be built (e.g. a coarse window whose finer-tier children have not been
// built, or an hour window with no in-window journal entries).
func (c *Cortex) RepairRollup(w Window, now int64) (*RollupRecord, error) {
	if w.End <= w.Start {
		return nil, fmt.Errorf("cortex.RepairRollup: empty window [%d,%d)", w.Start, w.End)
	}
	if !windowClosed(w, now) {
		return nil, fmt.Errorf("cortex.RepairRollup: window [%d,%d) is not closed as of now=%d", w.Start, w.End, now)
	}

	switch w.Tier {
	case TierHour:
		return c.repairHourRollup(w)
	case TierDay:
		return c.repairCoarseRollup(w, TierHour)
	case TierEpoch:
		return c.repairCoarseRollup(w, TierDay)
	default:
		return nil, fmt.Errorf("cortex.RepairRollup: unknown tier %d", uint8(w.Tier))
	}
}

// repairHourRollup rebuilds the hour rollup at w ONLY if missing — a closed
// hour window's journal facts are immutable, so an existing record is always
// authoritative (mirrors cascadeHours' build-only-if-missing).
func (c *Cortex) repairHourRollup(w Window) (*RollupRecord, error) {
	if rec, err := c.LoadRollup(w.Tier, w.Start); err == nil {
		return rec, nil
	} else if !errors.Is(err, memory.ErrNotFound) {
		return nil, fmt.Errorf("cortex.RepairRollup: load hour rollup: %w", err)
	}
	if _, err := c.BuildRollup(w); err != nil {
		return nil, fmt.Errorf("cortex.RepairRollup: build hour rollup: %w", err)
	}
	rec, err := c.LoadRollup(w.Tier, w.Start)
	if err != nil {
		return nil, fmt.Errorf("cortex.RepairRollup: load rebuilt hour rollup: %w", err)
	}
	return rec, nil
}

// repairCoarseRollup rebuilds the coarse-tier (day/epoch) rollup at w by
// aggregating the CURRENT finer-tier rollups whose window Start falls inside
// w, via the exact same buildCoarseRollup Cascade uses (compute-compare-skip:
// unchanged children -> no write; missing or stale -> write). Returns
// memory.ErrNotFound when w has no finer-tier children built yet.
func (c *Cortex) repairCoarseRollup(w Window, finer RollupTier) (*RollupRecord, error) {
	children, err := c.Rollups(finer, w.Start, w.End-1)
	if err != nil {
		return nil, fmt.Errorf("cortex.RepairRollup: load %s rollups: %w", finer.String(), err)
	}
	if len(children) == 0 {
		return nil, memory.ErrNotFound
	}
	if _, err := c.buildCoarseRollup(w, children); err != nil {
		return nil, fmt.Errorf("cortex.RepairRollup: build %s rollup: %w", w.Tier.String(), err)
	}
	rec, err := c.LoadRollup(w.Tier, w.Start)
	if err != nil {
		return nil, fmt.Errorf("cortex.RepairRollup: load rebuilt %s rollup: %w", w.Tier.String(), err)
	}
	return rec, nil
}
