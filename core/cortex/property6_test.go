// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 8.6 — Property 6: the active brain never perturbs the
// anchored world (D11) (validates req.11.1, 11.2, 11.3, 12.6).
//
// This is the CONSOLIDATED replay-safety proof: the task 1.2 baseline harness
// (cmharness) established the invariant on session writes alone; here it is
// exercised with EVERY continuous-memory derived lane active at once — the
// session/transcript store, the temporal ladder (BuildRollup + Cascade), the
// durable story-so-far, and the Activate composer (including its lazy
// story-repair write). Every assertion runs against REAL cortex + a REAL
// journal (no stub/mock/fake — req.12.7):
//
//   - req.11.2 (active vs inactive): two instances driven with the SAME
//     anchored Writes end with BYTE-IDENTICAL anchored SMT roots, even though
//     one of them additionally rides ALL the derived lanes.
//   - req.11.2 (D11 replay): the full-lane instance rebuilds to a
//     byte-identical FULL OverallRoot after dropping the derived indexes.
//   - req.11.1 (per-lane): each individual derived-lane write (session,
//     rollup/cascade, story, activate) leaves both anchored roots unchanged.
//   - req.11.3 holds by construction: this feature adds only derived-lane
//     cortex surfaces; it does not touch the signed MCL walk and grants the
//     Liaison no cortex-write/signing/plan-walk capability (documented in
//     cmharness; there is no MCL walk or key material in cortex to change).
package cortex_test

import (
	"testing"
	"time"

	"centra/core/cortex"
	"centra/core/cortex/cmharness"
	"centra/core/cortex/memory"
	"centra/core/cortex/store"
)

// openRollupCortexNamed is openRollupCortex with a caller-chosen actor/store,
// so two matched instances can be driven in lockstep (identical clock + idGen
// semantics) for the active-vs-inactive comparison.
func openRollupCortexNamed(t *testing.T, actor string) (*cortex.Cortex, *mutClock) {
	t.Helper()
	s, err := store.Open(t.TempDir(), actor, nil)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", actor, err)
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

// TestProperty6_AnchoredRootsIdenticalAcrossAllLanes proves req.11.2
// (active-vs-inactive): instance A performs anchored Writes ONLY; instance B
// performs the SAME anchored Writes AND rides every continuous-memory derived
// lane (session append, ladder cascade, story-so-far, Activate). Their anchored
// world-state (memories + edges SMT roots) is byte-identical; the FULL
// OverallRoot differs (B's derived journal leaves grew the MMR) — the honest
// nuance the harness documents.
func TestProperty6_AnchoredRootsIdenticalAcrossAllLanes(t *testing.T) {
	a, aClk := openRollupCortexNamed(t, "actor-inactive")
	b, bClk := openRollupCortexNamed(t, "actor-active")

	const conv = "conv-8-6"
	topics := []struct {
		topic string
		imp   uint8
	}{
		{"quantization", 9},
		{"tone", 7},
		{"verbosity", 5},
		{"format", 6},
	}

	for i, tc := range topics {
		at := baseHour.Add(time.Duration(i) * time.Minute)
		aClk.t = at
		bClk.t = at
		// Identical anchored op on BOTH, in identical order → identical minted
		// memory IDs (SMT keys) and identical version bytes.
		writePrefAt(t, a, tc.topic, tc.imp)
		writePrefAt(t, b, tc.topic, tc.imp)
		// B ALSO rides the derived session lane. AppendMessage uses conv/seq,
		// NOT the idGen, so it must not perturb B's memory-ID allocation.
		if _, err := b.AppendMessage(cortex.Message{
			ConversationID: conv,
			Role:           cortex.RoleUser,
			Content:        "interleaved derived write for " + tc.topic,
		}); err != nil {
			t.Fatalf("B.AppendMessage #%d: %v", i, err)
		}
	}

	// B now drives the REST of the derived lanes: cascade the full ladder,
	// build the durable story-so-far, and run Activate (which may lazily
	// repair the story record). All derived; none may touch the anchored SMT.
	closeAt := baseHour.Add(10 * 24 * time.Hour)
	bClk.t = closeAt.Add(1 * time.Hour)
	if err := b.Cascade(cortex.TierEpoch, closeAt.UnixNano()); err != nil {
		t.Fatalf("B.Cascade: %v", err)
	}
	if _, err := b.BuildStorySoFar(conv); err != nil {
		t.Fatalf("B.BuildStorySoFar: %v", err)
	}
	if _, err := b.Activate(conv, "", cortex.Budget{}); err != nil {
		t.Fatalf("B.Activate: %v", err)
	}

	// req.11.2: anchored world-state (memories + edges roots) byte-identical
	// across the two instances despite B's full derived-lane activity.
	if err := cmharness.CompareAnchoredRoots(a, b); err != nil {
		t.Fatalf("CompareAnchoredRoots (all lanes active vs inactive): %v", err)
	}

	// Honest nuance: the FULL OverallRoot must DIFFER because B's journal MMR
	// grew derived-lane leaves (session/rollup/story) that A's did not.
	ra, err := a.OverallRoot()
	if err != nil {
		t.Fatalf("a.OverallRoot: %v", err)
	}
	rb, err := b.OverallRoot()
	if err != nil {
		t.Fatalf("b.OverallRoot: %v", err)
	}
	if ra == rb {
		t.Fatalf("OverallRoot unexpectedly identical (a=%x b=%x): the derived lanes should have grown B's journal MMR", ra, rb)
	}
}

// TestProperty6_ReplayPreservesRootWithAllLanes proves req.11.2 (D11): a cortex
// carrying anchored Writes AND every continuous-memory derived-lane entry
// (session, rollup, story) in its journal rebuilds to a byte-identical FULL
// OverallRoot after the derived indexes are dropped and rebuilt from canonical
// state.
func TestProperty6_ReplayPreservesRootWithAllLanes(t *testing.T) {
	c, clk := openRollupCortexNamed(t, "actor-replay-all")
	const conv = "conv-8-6-replay"

	// Interleave anchored Writes and derived AppendMessage across a window.
	seeds := []struct {
		topic string
		imp   uint8
	}{{"alpha", 9}, {"beta", 7}, {"gamma", 5}}
	for i, sd := range seeds {
		clk.t = baseHour.Add(time.Duration(i) * time.Minute)
		writePrefAt(t, c, sd.topic, sd.imp)
		if _, err := c.AppendMessage(cortex.Message{
			ConversationID: conv, Role: cortex.RoleUser, Content: "note " + sd.topic,
		}); err != nil {
			t.Fatalf("AppendMessage #%d: %v", i, err)
		}
	}
	if _, err := c.AppendMessage(cortex.Message{
		ConversationID: conv, Role: cortex.RoleToolCall, ToolName: "core_execute",
		ToolArgs: []byte(`{"verb":"launch"}`),
	}); err != nil {
		t.Fatalf("AppendMessage tool_call: %v", err)
	}

	// Build the ladder + the durable story (more derived journal leaves).
	closeAt := baseHour.Add(10 * 24 * time.Hour)
	clk.t = closeAt.Add(1 * time.Hour)
	if err := c.Cascade(cortex.TierEpoch, closeAt.UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}
	if _, err := c.BuildStorySoFar(conv); err != nil {
		t.Fatalf("BuildStorySoFar: %v", err)
	}

	res, err := cmharness.ReplayPreservesRoot(c, clk.now)
	if err != nil {
		t.Fatalf("ReplayPreservesRoot with all derived lanes present: %v", err)
	}
	if res == nil {
		t.Fatal("ReplayPreservesRoot returned nil result")
	}
	if res.JournalLeavesAppended == 0 {
		t.Fatal("JournalLeavesAppended = 0; expected session/rollup/story + write leaves to be re-hashed")
	}
	if res.MemoriesScanned == 0 {
		t.Fatal("MemoriesScanned = 0; expected the anchored Preference writes to be scanned")
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
}

// TestProperty6_EachLaneIsNonPerturbing proves req.11.1 at the per-lane
// granularity: on a single instance seeded with anchored state, EACH derived
// write — a session append, a ladder cascade, a story build, and an Activate —
// leaves both anchored SMT roots byte-identical.
func TestProperty6_EachLaneIsNonPerturbing(t *testing.T) {
	c, clk := openRollupCortexNamed(t, "actor-perlane")
	const conv = "conv-8-6-perlane"

	// Seed anchored state in a known window so the roots are non-empty and the
	// ladder has something to roll up.
	clk.t = baseHour
	writePrefAt(t, c, "quantization", 9)
	clk.t = baseHour.Add(1 * time.Minute)
	writePrefAt(t, c, "tone", 7)

	// Lane 1: session append.
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, e := c.AppendMessage(cortex.Message{ConversationID: conv, Role: cortex.RoleUser, Content: "hello"})
		return e
	}); err != nil {
		t.Fatalf("session-append lane drifted the anchored root: %v", err)
	}

	// Lane 2: ladder cascade.
	closeAt := baseHour.Add(10 * 24 * time.Hour)
	clk.t = closeAt.Add(1 * time.Hour)
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		return c.Cascade(cortex.TierEpoch, closeAt.UnixNano())
	}); err != nil {
		t.Fatalf("ladder-cascade lane drifted the anchored root: %v", err)
	}

	// Lane 3: durable story-so-far.
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, e := c.BuildStorySoFar(conv)
		return e
	}); err != nil {
		t.Fatalf("story-so-far lane drifted the anchored root: %v", err)
	}

	// Lane 4: Activate (composer + any lazy story repair).
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, e := c.Activate(conv, "", cortex.Budget{})
		return e
	}); err != nil {
		t.Fatalf("activate lane drifted the anchored root: %v", err)
	}
}
