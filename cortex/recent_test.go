// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/cmharness"
	"matrix/cortex/memory"
)

// writeFactAt writes a real anchored Fact at the clock's current time with a
// given DeclaredImportance and returns its URI.
func writeFactAt(t *testing.T, c *cortex.Cortex, stmt string, importance uint8) memory.URI {
	t.Helper()
	uri, err := c.Write(memory.Head{
		ActorScope:         "andrew",
		DeclaredImportance: importance,
	}, memory.FactData{
		SchemaVersion: 1,
		Statement:     stmt,
		Subject:       "matrix://chain/pax",
		Predicate:     "block_time_ms",
	}, cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "fact:" + stmt},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writeFactAt(%s): %v", stmt, err)
	}
	return uri
}

// writeEventAt writes a real anchored Event at the clock's current time with a
// given DeclaredImportance and returns its URI.
func writeEventAt(t *testing.T, c *cortex.Cortex, summary string, importance uint8) memory.URI {
	t.Helper()
	uri, err := c.Write(memory.Head{
		ActorScope:         "andrew",
		DeclaredImportance: importance,
	}, memory.EventData{
		SchemaVersion: 1,
		Kind:          memory.EventIntentCompleted,
		OutcomeVal:    memory.OutcomeSuccess,
		Summary:       summary,
	}, cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "event:" + summary},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writeEventAt(%s): %v", summary, err)
	}
	return uri
}

// TestRecentEpisodesAcrossTypesRanked proves req.6.1: RecentEpisodes returns
// members drawn from MULTIPLE memory types, ordered by Salience DESC with the
// ID tiebreak, len <= n, served from the materialized hour rollup.
func TestRecentEpisodesAcrossTypesRanked(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	// Three DIFFERENT memory types inside the same hour window, distinct
	// importances so the recency-scored ranking is deterministic and known.
	clk.t = baseHour
	prefURI := writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	factURI := writeFactAt(t, c, "pax block ~277ms", 5)
	clk.t = baseHour.Add(2 * time.Minute)
	eventURI := writeEventAt(t, c, "boot", 1)

	// Materialize the hour rollup.
	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}

	now := baseHour.Add(90 * time.Minute)
	eps, err := c.RecentEpisodes(10, now)
	if err != nil {
		t.Fatalf("RecentEpisodes: %v", err)
	}
	if len(eps) != 3 {
		t.Fatalf("RecentEpisodes len = %d, want 3", len(eps))
	}

	// Ranking: Salience DESC.
	for i := 1; i < len(eps); i++ {
		if eps[i-1].Salience < eps[i].Salience {
			t.Fatalf("episodes not sorted by salience desc: %v", eps)
		}
	}

	// Across-types: the surfaced URIs must span at least two distinct memory
	// types (Preference, Fact, Event were written).
	types := map[memory.Type]struct{}{}
	uris := map[memory.URI]struct{}{}
	for _, e := range eps {
		typ, _, _, perr := cortex.ParseURI(e.Ref.URI)
		if perr != nil {
			t.Fatalf("ParseURI(%q): %v", e.Ref.URI, perr)
		}
		types[typ] = struct{}{}
		uris[e.Ref.URI] = struct{}{}
		if e.Ref.Kind != cortex.RefKindMemory {
			t.Fatalf("episode Ref.Kind = %q, want %q", e.Ref.Kind, cortex.RefKindMemory)
		}
		if e.Window != w {
			t.Fatalf("episode Window = %+v, want provenance %+v", e.Window, w)
		}
	}
	if len(types) < 2 {
		t.Fatalf("episodes span %d types, want >= 2 (types=%v)", len(types), types)
	}
	for _, u := range []memory.URI{prefURI, factURI, eventURI} {
		if _, ok := uris[u]; !ok {
			t.Fatalf("expected URI %q among episodes, got %v", u, uris)
		}
	}

	// Highest-importance write (pref, importance 9) ranks first at a common now.
	if eps[0].Ref.URI != prefURI {
		t.Fatalf("top episode = %q, want highest-salience %q", eps[0].Ref.URI, prefURI)
	}
}

// TestRecentEpisodesMaterialization proves req.6.2: with real memories written
// but NO rollups built yet, RecentEpisodes returns EMPTY (it is served from
// materialized rollups, not a raw memory/journal scan). After building the hour
// rollup, the same call returns the episodes.
func TestRecentEpisodesMaterialization(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	clk.t = baseHour
	writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	writeFactAt(t, c, "pax block ~277ms", 5)

	now := baseHour.Add(90 * time.Minute)

	// No rollups materialized yet -> empty, proving it does not scan memories.
	eps, err := c.RecentEpisodes(10, now)
	if err != nil {
		t.Fatalf("RecentEpisodes (pre-materialize): %v", err)
	}
	if len(eps) != 0 {
		t.Fatalf("RecentEpisodes pre-materialize len = %d, want 0 (must be served from rollups)", len(eps))
	}

	// Materialize -> the same call now surfaces the episodes.
	clk.t = now
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}
	eps, err = c.RecentEpisodes(10, now)
	if err != nil {
		t.Fatalf("RecentEpisodes (post-materialize): %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("RecentEpisodes post-materialize len = %d, want 2", len(eps))
	}
}

// TestRecentEpisodesHorizon proves the horizon bound: episodes only come from
// rollups within the lookback window; a rollup older than the horizon is
// excluded.
func TestRecentEpisodesHorizon(t *testing.T) {
	c, clk := openRollupCortex(t)

	oldHour := cortex.HourWindow(baseHour.UnixNano())
	// A window well beyond the 24h horizon relative to the recent window.
	recentBase := baseHour.Add(48 * time.Hour)
	recentHour := cortex.HourWindow(recentBase.UnixNano())

	clk.t = baseHour
	oldURI := writePrefAt(t, c, "ancient", 9)
	clk.t = recentBase
	recentURI := writeFactAt(t, c, "fresh", 5)

	// Materialize both hour rollups.
	clk.t = recentBase.Add(90 * time.Minute)
	if _, err := c.BuildRollup(oldHour); err != nil {
		t.Fatalf("BuildRollup(old): %v", err)
	}
	if _, err := c.BuildRollup(recentHour); err != nil {
		t.Fatalf("BuildRollup(recent): %v", err)
	}

	now := recentBase.Add(30 * time.Minute)
	eps, err := c.RecentEpisodes(10, now)
	if err != nil {
		t.Fatalf("RecentEpisodes: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("RecentEpisodes len = %d, want 1 (old rollup outside horizon)", len(eps))
	}
	if eps[0].Ref.URI != recentURI {
		t.Fatalf("episode = %q, want in-horizon %q", eps[0].Ref.URI, recentURI)
	}
	// The out-of-horizon memory must not appear.
	for _, e := range eps {
		if e.Ref.URI == oldURI {
			t.Fatalf("out-of-horizon URI %q surfaced", oldURI)
		}
	}
}

// TestRecentEpisodesCap proves the cap: with more members than n across
// rollups, exactly n highest-recency episodes are returned.
func TestRecentEpisodesCap(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	// Five members in the window with strictly increasing importance so the
	// top-3 by recency-score are the three highest-importance writes.
	clk.t = baseHour
	writePrefAt(t, c, "i1", 1)
	clk.t = baseHour.Add(1 * time.Minute)
	writeFactAt(t, c, "i2", 3)
	clk.t = baseHour.Add(2 * time.Minute)
	writeEventAt(t, c, "i3", 5)
	clk.t = baseHour.Add(3 * time.Minute)
	topA := writePrefAt(t, c, "i4", 7)
	clk.t = baseHour.Add(4 * time.Minute)
	topB := writeFactAt(t, c, "i5", 9)

	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}

	now := baseHour.Add(90 * time.Minute)
	eps, err := c.RecentEpisodes(3, now)
	if err != nil {
		t.Fatalf("RecentEpisodes: %v", err)
	}
	if len(eps) != 3 {
		t.Fatalf("RecentEpisodes len = %d, want exactly 3 (cap)", len(eps))
	}
	// The two highest-importance writes must be in the capped set.
	got := map[memory.URI]struct{}{}
	for _, e := range eps {
		got[e.Ref.URI] = struct{}{}
	}
	for _, u := range []memory.URI{topA, topB} {
		if _, ok := got[u]; !ok {
			t.Fatalf("high-salience URI %q missing from capped set %v", u, got)
		}
	}
}

// TestRecentEpisodesSkipsTombstoned proves a tombstoned member is skipped.
func TestRecentEpisodesSkipsTombstoned(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	clk.t = baseHour
	deadURI := writePrefAt(t, c, "doomed", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	liveURI := writeFactAt(t, c, "survivor", 5)

	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}

	// Tombstone the top member AFTER the rollup materialized it.
	clk.t = baseHour.Add(91 * time.Minute)
	if err := c.Tombstone(deadURI, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}

	now := baseHour.Add(92 * time.Minute)
	eps, err := c.RecentEpisodes(10, now)
	if err != nil {
		t.Fatalf("RecentEpisodes: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("RecentEpisodes len = %d, want 1 (tombstoned member skipped)", len(eps))
	}
	if eps[0].Ref.URI != liveURI {
		t.Fatalf("episode = %q, want live %q", eps[0].Ref.URI, liveURI)
	}
	for _, e := range eps {
		if e.Ref.URI == deadURI {
			t.Fatalf("tombstoned URI %q surfaced", deadURI)
		}
	}
}

// TestRecentEpisodesReadOnly proves req.6.2's read-only posture: RecentEpisodes
// writes nothing — no anchored SMT drift.
func TestRecentEpisodesReadOnly(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	clk.t = baseHour
	writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	writeEventAt(t, c, "boot", 5)

	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}

	now := baseHour.Add(90 * time.Minute)
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, err := c.RecentEpisodes(10, now)
		return err
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across RecentEpisodes: %v", err)
	}
}
