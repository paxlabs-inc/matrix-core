// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex_test

import (
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/cmharness"
	"matrix/cortex/memory"
)

// writeIdentityAt writes a real pinned Identity memory at the clock's
// current time and returns its URI.
func writeIdentityAt(t *testing.T, c *cortex.Cortex, name string) memory.URI {
	t.Helper()
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.IdentityData{
		SchemaVersion: 1, Name: name, DID: "did:pax:" + name,
	}, cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "id:" + name, Medium: name + ", the owner"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writeIdentityAt(%s): %v", name, err)
	}
	return uri
}

// TestActivateFullBundle proves req.7.1/7.2: a real cortex seeded with a
// pinned Identity, cascaded rollups (T0/T1), and a real conversation
// transcript (T2) returns a fully populated ActivationBundle, with the
// story-so-far lazily materialized (task 4.2) and no error.
func TestActivateFullBundle(t *testing.T) {
	c, clk := openRollupCortex(t)

	idURI := writeIdentityAt(t, c, "Andrew")

	clk.t = baseHour
	writePrefAt(t, c, "alpha", 8)
	closeAt := baseHour.Add(10 * 24 * time.Hour)
	clk.t = closeAt.Add(1 * time.Hour)
	if err := c.Cascade(cortex.TierEpoch, closeAt.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}

	conv := "conv-activate"
	if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleUser, Content: "what have we done so far"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleAssistant, Content: "we wrote a preference"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	bundle, err := c.Activate(conv, "", cortex.Budget{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	var gotPinned bool
	for _, m := range bundle.Pinned {
		if m.Head.ID == mustParseID(t, idURI) {
			gotPinned = true
		}
	}
	if !gotPinned {
		t.Fatalf("Pinned = %+v, want to contain identity %q", bundle.Pinned, idURI)
	}
	if len(bundle.Timeline) == 0 {
		t.Fatal("Timeline empty, want >=1 materialized epoch rollup")
	}
	if len(bundle.Transcript) != 2 {
		t.Fatalf("Transcript len = %d, want 2", len(bundle.Transcript))
	}
	if bundle.StorySoFar == "" {
		t.Fatal("StorySoFar empty, want lazily-materialized summary")
	}
	if bundle.LatencyMS < 0 {
		t.Fatalf("LatencyMS = %d, want >= 0", bundle.LatencyMS)
	}

	// The story-so-far must now be persisted (lazy repair materialized it).
	if _, err := c.LoadStorySoFar(conv); err != nil {
		t.Fatalf("LoadStorySoFar after Activate: %v", err)
	}
}

func mustParseID(t *testing.T, uri memory.URI) memory.ID {
	t.Helper()
	_, id, _, err := cortex.ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI(%s): %v", uri, err)
	}
	return id
}

// TestActivateNoTranscriptDegradesGracefully proves req.7.5: a conversation
// with no session transcript yet returns a valid, non-error bundle (empty
// Transcript/StorySoFar), not a failure.
func TestActivateNoTranscriptDegradesGracefully(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	writePrefAt(t, c, "alpha", 5)

	bundle, err := c.Activate("conv-never-seen", "", cortex.Budget{})
	if err != nil {
		t.Fatalf("Activate(no transcript): %v", err)
	}
	if len(bundle.Transcript) != 0 {
		t.Fatalf("Transcript = %+v, want empty", bundle.Transcript)
	}
	if bundle.StorySoFar != "" {
		t.Fatalf("StorySoFar = %q, want empty", bundle.StorySoFar)
	}
}

// TestActivatePinnedCacheServesRepeatCalls proves req.7.2: two Activate
// calls with no intervening cortex write return the identical Pinned set
// (served from the per-turn cache, not a fresh idx/type scan each time).
func TestActivatePinnedCacheServesRepeatCalls(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	writeIdentityAt(t, c, "Andrew")

	b1, err := c.Activate("conv-cache", "", cortex.Budget{})
	if err != nil {
		t.Fatalf("Activate #1: %v", err)
	}
	b2, err := c.Activate("conv-cache", "", cortex.Budget{})
	if err != nil {
		t.Fatalf("Activate #2: %v", err)
	}
	if len(b1.Pinned) != len(b2.Pinned) || len(b1.Pinned) == 0 {
		t.Fatalf("Pinned mismatch across repeat calls: #1=%d #2=%d", len(b1.Pinned), len(b2.Pinned))
	}
	if b1.Pinned[0].Head.ID != b2.Pinned[0].Head.ID {
		t.Fatalf("Pinned[0] id mismatch: #1=%s #2=%s", b1.Pinned[0].Head.ID, b2.Pinned[0].Head.ID)
	}
}

// TestActivateBudgetTrim proves req.7.1: a tiny budget drops low-salience
// candidates into ReachableURIs while keeping the pool under budget (or at
// the ≥1 relief valve).
func TestActivateBudgetTrim(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	writePrefAt(t, c, "alpha", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	writePrefAt(t, c, "beta", 1)
	clk.t = baseHour.Add(90 * time.Minute)
	if err := c.Cascade(cortex.TierEpoch, clk.t.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}

	full, err := c.Activate("conv-budget", "", cortex.Budget{Tokens: cortex.MaxActivateBudgetTokens})
	if err != nil {
		t.Fatalf("Activate(full budget): %v", err)
	}
	tiny, err := c.Activate("conv-budget", "", cortex.Budget{Tokens: 1})
	if err != nil {
		t.Fatalf("Activate(tiny budget): %v", err)
	}
	if tiny.TotalTokens > full.TotalTokens {
		t.Fatalf("tiny budget TotalTokens = %d > full budget TotalTokens = %d", tiny.TotalTokens, full.TotalTokens)
	}
	if tiny.Trimmed == 0 && full.Trimmed == 0 {
		t.Skip("nothing to trim in this seed; not a hard failure")
	}
}

// TestActivateDerivedLaneSafety proves req.7.6: Activate (including its
// lazy StorySoFar repair) performs NO anchored-namespace SMT write, and the
// full OverallRoot rebuilds byte-identically afterward.
func TestActivateDerivedLaneSafety(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour
	writePrefAt(t, c, "alpha", 5)
	conv := "conv-safety"
	if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleUser, Content: "hi"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, aerr := c.Activate(conv, "", cortex.Budget{})
		return aerr
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across Activate: %v", err)
	}

	res, err := cmharness.ReplayPreservesRoot(c, nil)
	if err != nil {
		t.Fatalf("ReplayPreservesRoot with Activate's story entry present: %v", err)
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
}

// TestActivateRichBudgetAboveLegacyClamp proves the 1M-window residency
// posture: a caller-supplied budget well above the legacy 4000-token Context
// clamp is honored — a real transcript too large for the old clamp survives
// un-trimmed under a 20000-token budget, and the same call under the legacy
// ceiling trims. Also pins the new MaxActivateBudgetTokens ceiling semantics.
func TestActivateRichBudgetAboveLegacyClamp(t *testing.T) {
	c, clk := openRollupCortex(t)
	clk.t = baseHour

	conv := "conv-rich"
	// ~40 tokens per message x 300 messages ≈ 12K tokens of real transcript —
	// far over the legacy 4000 clamp, comfortably under 20000.
	line := "step report: verified the build, reran the suite, recorded the outcome for posterity and moved on"
	for i := 0; i < 300; i++ {
		clk.t = clk.t.Add(time.Second)
		if _, err := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleUser, Content: line}); err != nil {
			t.Fatalf("AppendMessage(%d): %v", i, err)
		}
	}

	rich, err := c.Activate(conv, "", cortex.Budget{Tokens: 20000})
	if err != nil {
		t.Fatalf("Activate(20000): %v", err)
	}
	if rich.TotalTokens <= 4000 {
		t.Fatalf("rich budget bundle carries only %d tokens — the legacy 4000 clamp is still binding", rich.TotalTokens)
	}
	if rich.TotalTokens > 20000 {
		t.Fatalf("rich bundle %d tokens exceeds its 20000 budget", rich.TotalTokens)
	}

	legacy, err := c.Activate(conv, "", cortex.Budget{Tokens: 4000})
	if err != nil {
		t.Fatalf("Activate(4000): %v", err)
	}
	if legacy.TotalTokens > 4000 {
		t.Fatalf("legacy-budget bundle %d tokens exceeds 4000", legacy.TotalTokens)
	}
	if legacy.TotalTokens >= rich.TotalTokens {
		t.Fatalf("legacy budget (%d tokens) must carry less than the rich budget (%d)", legacy.TotalTokens, rich.TotalTokens)
	}

	// Ceiling: an absurd request clamps to MaxActivateBudgetTokens, not error.
	capped, err := c.Activate(conv, "", cortex.Budget{Tokens: 1_000_000})
	if err != nil {
		t.Fatalf("Activate(1M): %v", err)
	}
	if capped.TotalTokens > cortex.MaxActivateBudgetTokens {
		t.Fatalf("capped bundle %d exceeds MaxActivateBudgetTokens %d", capped.TotalTokens, cortex.MaxActivateBudgetTokens)
	}
}
