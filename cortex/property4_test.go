// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 8.4 — Property 4: recursive recall pages in exact
// specifics within bounds, with time-travel (validates req.8.1, 8.2, 8.3,
// 12.3).
//
// Every assertion runs against a REAL seeded cortex + a REAL cascaded ladder
// (no stub/mock/fake — req.12.7):
//
//   - A decomposable descent (list T0 epoch → resolve day → hour → memory
//     leaf) reaches an exact memory that is NOT resident in the Activate
//     bundle (req.8.1).
//   - The depth cap is enforced: MaxDepth above RecallMaxDepthCap errors, and
//     a MaxDepth too shallow to reach the leaves surfaces nothing + Truncated
//     (req.8.2).
//   - A tiny per-descent token budget truncates the descent (req.8.2).
//   - Bi-temporal as_of returns the then-true view: a memory closed in valid
//     time is reachable at an as_of before the close and filtered at/after it
//     (req.8.3).
package cortex_test

import (
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/cmharness"
	"matrix/cortex/memory"
)

// seedRecallLadder writes a real Preference at baseHour and cascades the full
// hour→day→epoch ladder as of `closeAt` (10 days later, so every window is
// closed). Returns the memory URI and the wall instant used as "now" for a
// descent (baseHour+10d+1h — well outside the 24h T1 horizon).
func seedRecallLadder(t *testing.T, c *cortex.Cortex, clk *mutClock, topic string, importance uint8) (memory.URI, time.Time) {
	t.Helper()
	clk.t = baseHour
	uri := writePrefAt(t, c, topic, importance)
	closeAt := baseHour.Add(10 * 24 * time.Hour)
	clk.t = closeAt.Add(1 * time.Hour)
	if err := c.Cascade(cortex.TierEpoch, closeAt.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}
	return uri, clk.t
}

// bundleContainsMemory reports whether the resolved memory `id` is resident
// anywhere in the activation bundle's Pinned / Recent / (rollup-member)
// Timeline tiers — the test's definition of "already in the working set".
func bundleContainsMemory(b *cortex.ActivationBundle, id memory.ID, uri memory.URI) bool {
	for _, m := range b.Pinned {
		if m.Head.ID == id {
			return true
		}
	}
	for _, ep := range b.Recent {
		if ep.Ref.URI == uri {
			return true
		}
	}
	for i := range b.Timeline {
		for _, ref := range b.Timeline[i].Members {
			if ref.URI == uri {
				return true
			}
		}
	}
	return false
}

// TestProperty4_DescentReachesExactEventNotInBundle proves req.8.1: an exact
// memory that Activate does NOT surface (it is older than the 24h T1 horizon,
// not pinned, not in the transcript, and not a direct member of any epoch
// rollup in the bundle) is reached by the recursive descent, at depth 4
// (epoch→day→hour→memory).
func TestProperty4_DescentReachesExactEventNotInBundle(t *testing.T) {
	c, clk := openRollupCortex(t)
	uri, now := seedRecallLadder(t, c, clk, "quantization", 8)
	id := mustParseID(t, uri)

	// The Activate bundle must NOT already contain the memory.
	bundle, err := c.Activate("conv-recall", "", cortex.Budget{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if bundleContainsMemory(bundle, id, uri) {
		t.Fatalf("precondition failed: memory %s is already resident in the Activate bundle", uri)
	}

	// The recursive descent reaches it by paging down the ladder.
	res, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &now})
	if err != nil {
		t.Fatalf("RecallDescend: %v", err)
	}
	var found *cortex.RecallHit
	for i := range res.Hits {
		if res.Hits[i].URI == uri {
			found = &res.Hits[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("descent did not reach memory %s; hits=%d windows=%d", uri, len(res.Hits), len(res.Windows))
	}
	if found.Depth != 4 {
		t.Fatalf("leaf reached at depth %d, want 4 (epoch→day→hour→memory)", found.Depth)
	}
	if res.MaxDepthReached != 4 {
		t.Fatalf("MaxDepthReached = %d, want 4", res.MaxDepthReached)
	}
	if len(res.Windows) == 0 {
		t.Fatal("Windows empty, want >=1 T0 window visited (provenance)")
	}
}

// TestProperty4_DepthCapEnforced proves req.8.2: MaxDepth above the ceiling
// errors, and a MaxDepth too shallow to reach the leaves surfaces nothing and
// marks the descent Truncated.
func TestProperty4_DepthCapEnforced(t *testing.T) {
	c, clk := openRollupCortex(t)
	_, now := seedRecallLadder(t, c, clk, "quantization", 8)

	// Above the hard ceiling → error.
	if _, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &now, MaxDepth: cortex.RecallMaxDepthCap + 1}); err == nil {
		t.Fatalf("MaxDepth %d must exceed RecallMaxDepthCap and error", cortex.RecallMaxDepthCap+1)
	}

	// Too shallow to reach the memory leaves (epoch members are day-rollup
	// refs; depth 1 cannot descend into them) → no hits, Truncated.
	shallow, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &now, MaxDepth: 1})
	if err != nil {
		t.Fatalf("RecallDescend(MaxDepth=1): %v", err)
	}
	if len(shallow.Hits) != 0 {
		t.Fatalf("MaxDepth=1 reached %d leaf hits, want 0 (leaves sit at depth 4)", len(shallow.Hits))
	}
	if !shallow.Truncated {
		t.Fatal("MaxDepth=1 must mark the result Truncated (descent stopped at the cap)")
	}

	// The default depth DOES reach the leaves — proving the cap, not a bug,
	// suppressed the shallow descent.
	full, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &now})
	if err != nil {
		t.Fatalf("RecallDescend(default depth): %v", err)
	}
	if len(full.Hits) == 0 {
		t.Fatal("default depth reached 0 hits, want >=1 (cap suppression sanity)")
	}
}

// TestProperty4_PerDescentBudgetEnforced proves req.8.2: a per-descent token
// budget of 1 stops the descent early and marks it Truncated, reaching fewer
// hits than an ample budget over the same seeded ladder.
func TestProperty4_PerDescentBudgetEnforced(t *testing.T) {
	c, clk := openRollupCortex(t)
	// Seed several memories in the same window so a descent has multiple
	// leaves to (partially) reach.
	clk.t = baseHour
	writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	writePrefAt(t, c, "beta", 7)
	clk.t = baseHour.Add(2 * time.Minute)
	writePrefAt(t, c, "gamma", 5)
	closeAt := baseHour.Add(10 * 24 * time.Hour)
	now := closeAt.Add(1 * time.Hour)
	clk.t = now
	if err := c.Cascade(cortex.TierEpoch, closeAt.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}

	ample, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &now, DescentBudgetTokens: cortex.RecallDefaultDescentBudgetTokens})
	if err != nil {
		t.Fatalf("RecallDescend(ample): %v", err)
	}
	tiny, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &now, DescentBudgetTokens: 1})
	if err != nil {
		t.Fatalf("RecallDescend(tiny): %v", err)
	}
	if len(ample.Hits) == 0 {
		t.Fatal("ample budget reached 0 hits, want >=1")
	}
	if !tiny.Truncated {
		t.Fatal("a 1-token per-descent budget must mark the result Truncated")
	}
	if len(tiny.Hits) >= len(ample.Hits) {
		t.Fatalf("tiny budget reached %d hits >= ample %d — budget not enforced", len(tiny.Hits), len(ample.Hits))
	}
}

// TestProperty4_AsOfTimeTravel proves req.8.3: bi-temporal as_of is preserved
// at the leaf — a memory whose valid time is closed is reachable at an as_of
// BEFORE the close and filtered at/after it.
func TestProperty4_AsOfTimeTravel(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	uri := writePrefAt(t, c, "quantization", 8)

	// Close the memory's valid time 2 days after it was written.
	closeAt := baseHour.Add(2 * 24 * time.Hour)
	clk.t = closeAt
	if _, err := c.CloseValidity(uri, time.Time{}, "andrew"); err != nil {
		t.Fatalf("CloseValidity: %v", err)
	}

	// Cascade the ladder as of well after the close (all windows closed).
	cascadeNow := baseHour.Add(10 * 24 * time.Hour)
	clk.t = cascadeNow.Add(1 * time.Hour)
	if err := c.Cascade(cortex.TierEpoch, cascadeNow.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}

	// as_of BEFORE the close: the memory was live → reachable.
	before := baseHour.Add(1 * time.Hour)
	resBefore, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &before})
	if err != nil {
		t.Fatalf("RecallDescend(before close): %v", err)
	}
	if !recallHasURI(resBefore, uri) {
		t.Fatalf("as_of before close should surface %s; hits=%d", uri, len(resBefore.Hits))
	}

	// as_of AT/AFTER the close: the memory's valid interval no longer
	// contains as_of → filtered.
	after := closeAt.Add(1 * time.Hour)
	resAfter, err := c.RecallDescend("", cortex.RecallOpts{AsOf: &after})
	if err != nil {
		t.Fatalf("RecallDescend(after close): %v", err)
	}
	if recallHasURI(resAfter, uri) {
		t.Fatalf("as_of at/after close must filter %s (half-open valid interval)", uri)
	}
}

// recallHasURI reports whether the descent result surfaced uri.
func recallHasURI(res *cortex.RecallResult, uri memory.URI) bool {
	for _, h := range res.Hits {
		if h.URI == uri {
			return true
		}
	}
	return false
}

// TestProperty4_DescentIsReadOnly proves recall descent is a pure read: it
// stages no anchored SMT write and the full OverallRoot rebuilds
// byte-identically afterward (supports req.11 / the off-hot-path posture).
func TestProperty4_DescentIsReadOnly(t *testing.T) {
	c, clk := openRollupCortex(t)
	_, now := seedRecallLadder(t, c, clk, "quantization", 8)

	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, rerr := c.RecallDescend("quant", cortex.RecallOpts{AsOf: &now})
		return rerr
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across RecallDescend: %v", err)
	}
}
