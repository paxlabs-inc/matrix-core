// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"centra/core/cortex"
	"centra/core/cortex/cmharness"
	"centra/core/cortex/memory"
	"centra/core/cortex/store"
)

// mutClock is a mutable deterministic clock: tests set .t to place a Write /
// Attest at a known instant (so its journal CreatedAt lands in a known
// window), and may advance it to prove BuildRollup is wall-clock-independent.
type mutClock struct{ t time.Time }

func (m *mutClock) now() time.Time { return m.t }

// openRollupCortex opens a fresh cortex over its own temp store with a
// mutable clock and a deterministic counter idGen.
func openRollupCortex(t *testing.T) (*cortex.Cortex, *mutClock) {
	t.Helper()
	s, err := store.Open(t.TempDir(), "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	clk := &mutClock{}
	var n byte
	gen := func() memory.ID {
		n++
		var id memory.ID
		id[0] = n
		return id
	}
	c := cortex.New(s, cortex.WithClock(clk.now), cortex.WithIDGen(gen))
	return c, clk
}

// writePrefAt writes a real anchored Preference at the clock's current time
// with a given DeclaredImportance (drives salience so member ranking is
// predictable) and returns its URI.
func writePrefAt(t *testing.T, c *cortex.Cortex, topic string, importance uint8) memory.URI {
	t.Helper()
	uri, err := c.Write(memory.Head{
		ActorScope:         "andrew",
		DeclaredImportance: importance,
	}, memory.PreferenceData{
		SchemaVersion: 1,
		Topic:         topic,
		Polarity:      memory.PolarityPrefer,
		StrengthVal:   0.5,
	}, cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "pref:" + topic},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writePrefAt(%s): %v", topic, err)
	}
	return uri
}

// baseHour is a fixed instant inside a known UTC hour bucket.
var baseHour = time.Date(2026, 1, 15, 12, 30, 0, 0, time.UTC)

// TestBuildRollupBasic proves req.3.1/3.3/4.2: a rollup over a window covering
// real journal entries carries the right EntryCount, kind/outcome tallies,
// top-salience members (in score-DESC order), a non-empty ShortForm, and
// LoadRollup round-trips the exact record.
func TestBuildRollupBasic(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	// Three writes inside the window at distinct minutes with distinct
	// importances, so the salience ordering is deterministic and known.
	clk.t = baseHour
	topURI := writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	writePrefAt(t, c, "beta", 5)
	clk.t = baseHour.Add(2 * time.Minute)
	writePrefAt(t, c, "gamma", 1)

	// An attest (success) inside the window citing the top memory.
	clk.t = baseHour.Add(3 * time.Minute)
	if _, err := c.Attest(cortex.AttestOpts{
		IntentID:  "intent-roll",
		Outcome:   cortex.AttestOutcomeSuccess,
		Cited:     []memory.URI{topURI},
		CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	// Build the rollup (advance wall clock beyond the window first — the
	// record must not depend on wall-now).
	clk.t = baseHour.Add(90 * time.Minute)
	uri, err := c.BuildRollup(w)
	if err != nil {
		t.Fatalf("BuildRollup: %v", err)
	}
	if uri != cortex.BuildRollupURI(cortex.TierHour, w.Start) {
		t.Fatalf("BuildRollup uri = %q, want %q", uri, cortex.BuildRollupURI(cortex.TierHour, w.Start))
	}

	rec, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup: %v", err)
	}

	if rec.KindTally["write"] != 3 {
		t.Fatalf("KindTally[write] = %d, want 3 (tally=%v)", rec.KindTally["write"], rec.KindTally)
	}
	if rec.KindTally["attest"] != 1 {
		t.Fatalf("KindTally[attest] = %d, want 1 (tally=%v)", rec.KindTally["attest"], rec.KindTally)
	}
	if rec.OutcomeTally["success"] != 1 {
		t.Fatalf("OutcomeTally[success] = %d, want 1 (tally=%v)", rec.OutcomeTally["success"], rec.OutcomeTally)
	}
	// EntryCount must equal the sum of the kind tally (KindRollup excluded).
	var sum uint32
	for _, v := range rec.KindTally {
		sum += v
	}
	if rec.EntryCount != sum {
		t.Fatalf("EntryCount = %d, want sum(KindTally) = %d", rec.EntryCount, sum)
	}
	if len(rec.Members) != 3 {
		t.Fatalf("Members len = %d, want 3", len(rec.Members))
	}
	// Top-salience member is the importance-9 write.
	if rec.Members[0].URI != topURI {
		t.Fatalf("Members[0].URI = %q, want top-salience %q", rec.Members[0].URI, topURI)
	}
	if rec.Members[0].Kind != cortex.RefKindMemory {
		t.Fatalf("Members[0].Kind = %q, want %q", rec.Members[0].Kind, cortex.RefKindMemory)
	}
	if rec.ShortForm == "" {
		t.Fatalf("ShortForm empty")
	}
	if rec.Salience <= 0 {
		t.Fatalf("Salience = %v, want > 0 (top member score)", rec.Salience)
	}
	if rec.EnrichRef != "" {
		t.Fatalf("EnrichRef = %q, want empty (task 2.3 populates)", rec.EnrichRef)
	}
	if rec.Window != w {
		t.Fatalf("Window = %+v, want %+v", rec.Window, w)
	}
}

// TestBuildRollupReproducible proves req.4.1 idempotence + req.4.2 determinism:
// building twice over the same window — with the wall clock advanced between
// the two calls — yields byte-identical RollupRecord encodings.
func TestBuildRollupReproducible(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	clk.t = baseHour
	uri := writePrefAt(t, c, "alpha", 7)
	clk.t = baseHour.Add(5 * time.Minute)
	writePrefAt(t, c, "beta", 3)
	clk.t = baseHour.Add(6 * time.Minute)
	if _, err := c.Attest(cortex.AttestOpts{
		IntentID: "intent-x", Outcome: cortex.AttestOutcomeFailure,
		Reason: "factual_error", Cited: []memory.URI{uri}, CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	clk.t = baseHour.Add(90 * time.Minute)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup #1: %v", err)
	}
	rec1, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup #1: %v", err)
	}
	b1, err := cortex.EncodeRollupRecord(rec1)
	if err != nil {
		t.Fatalf("encode rec1: %v", err)
	}

	// Advance wall clock far into the future and rebuild.
	clk.t = baseHour.Add(365 * 24 * time.Hour)
	if _, err := c.BuildRollup(w); err != nil {
		t.Fatalf("BuildRollup #2: %v", err)
	}
	rec2, err := c.LoadRollup(cortex.TierHour, w.Start)
	if err != nil {
		t.Fatalf("LoadRollup #2: %v", err)
	}
	b2, err := cortex.EncodeRollupRecord(rec2)
	if err != nil {
		t.Fatalf("encode rec2: %v", err)
	}

	if !bytes.Equal(b1, b2) {
		t.Fatalf("RollupRecord not byte-identical across rebuilds:\n #1 %x\n #2 %x", b1, b2)
	}
	if rec1.ShortForm != rec2.ShortForm {
		t.Fatalf("ShortForm drift:\n #1 %q\n #2 %q", rec1.ShortForm, rec2.ShortForm)
	}
}

// TestBuildRollupBelowFloor proves req.4.1: an empty (under-floor) window
// produces NO roll/ record.
func TestBuildRollupBelowFloor(t *testing.T) {
	c, clk := openRollupCortex(t)

	// Write one entry far in the past so the target window is empty.
	clk.t = baseHour
	writePrefAt(t, c, "alpha", 5)

	// A window a year later has zero in-window entries.
	empty := cortex.HourWindow(baseHour.Add(365 * 24 * time.Hour).UnixNano())
	clk.t = baseHour.Add(400 * 24 * time.Hour)
	uri, err := c.BuildRollup(empty)
	if err != nil {
		t.Fatalf("BuildRollup(empty): %v", err)
	}
	if uri != "" {
		t.Fatalf("BuildRollup(empty) uri = %q, want empty (below floor)", uri)
	}
	if _, lerr := c.LoadRollup(cortex.TierHour, empty.Start); !errors.Is(lerr, memory.ErrNotFound) {
		t.Fatalf("LoadRollup(empty) err = %v, want memory.ErrNotFound", lerr)
	}
}

// TestRollupsAscendingOrder proves req.3.1: Rollups(tier, since, until) returns
// built rollups in ascending window-start order.
func TestRollupsAscendingOrder(t *testing.T) {
	c, clk := openRollupCortex(t)

	w0 := cortex.HourWindow(baseHour.UnixNano())
	w1 := cortex.HourWindow(baseHour.Add(3 * time.Hour).UnixNano())
	w2 := cortex.HourWindow(baseHour.Add(6 * time.Hour).UnixNano())

	// Write into each window.
	clk.t = baseHour
	writePrefAt(t, c, "w0", 5)
	clk.t = baseHour.Add(3 * time.Hour)
	writePrefAt(t, c, "w1", 5)
	clk.t = baseHour.Add(6 * time.Hour)
	writePrefAt(t, c, "w2", 5)

	// Build in a deliberately out-of-order sequence; storage order must be
	// by window start regardless.
	clk.t = baseHour.Add(24 * time.Hour)
	for _, w := range []cortex.Window{w2, w0, w1} {
		if _, err := c.BuildRollup(w); err != nil {
			t.Fatalf("BuildRollup(%d): %v", w.Start, err)
		}
	}

	got, err := c.Rollups(cortex.TierHour, w0.Start, w2.Start)
	if err != nil {
		t.Fatalf("Rollups: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Rollups len = %d, want 3", len(got))
	}
	if got[0].Window.Start != w0.Start || got[1].Window.Start != w1.Start || got[2].Window.Start != w2.Start {
		t.Fatalf("Rollups order = [%d,%d,%d], want ascending [%d,%d,%d]",
			got[0].Window.Start, got[1].Window.Start, got[2].Window.Start,
			w0.Start, w1.Start, w2.Start)
	}
}

// TestBuildRollupDerivedLaneSafety proves the derived-lane posture: BuildRollup
// causes NO anchored-namespace SMT drift (req.3.2), and the FULL OverallRoot
// (incl the KindRollup journal-MMR leaves) rebuilds byte-identically after a
// replay drop+rebuild.
func TestBuildRollupDerivedLaneSafety(t *testing.T) {
	c, clk := openRollupCortex(t)
	w := cortex.HourWindow(baseHour.UnixNano())

	// Seed anchored + derived state inside the window.
	clk.t = baseHour
	uri := writePrefAt(t, c, "alpha", 8)
	clk.t = baseHour.Add(1 * time.Minute)
	writePrefAt(t, c, "beta", 4)
	clk.t = baseHour.Add(2 * time.Minute)
	if _, err := c.Attest(cortex.AttestOpts{
		IntentID: "intent-safe", Outcome: cortex.AttestOutcomeSuccess,
		Cited: []memory.URI{uri}, CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if _, err := c.AppendMessage(cortex.Message{
		ConversationID: "conv-roll", Role: cortex.RoleUser, Content: "summarize the hour",
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// req.3.2: a BuildRollup performs NO memories/edges SMT write.
	clk.t = baseHour.Add(90 * time.Minute)
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, berr := c.BuildRollup(w)
		return berr
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across BuildRollup: %v", err)
	}

	// D11: full OverallRoot rebuilds byte-identically with rollup journal
	// entries present.
	res, err := cmharness.ReplayPreservesRoot(c, nil)
	if err != nil {
		t.Fatalf("ReplayPreservesRoot with rollup entries present: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
}
