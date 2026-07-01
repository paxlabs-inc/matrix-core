// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cmharness_test

import (
	"fmt"
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/cmharness"
	"matrix/cortex/memory"
	"matrix/cortex/store"
)

// fixedClock is a deterministic clock: both instances must mint identical
// version timestamps for the same Writes so the memories-SMT values match.
func fixedClock() time.Time { return time.Unix(1700000000, 0).UTC() }

// newCounterIDGen returns a deterministic idGen closure whose own counter
// starts at zero. Each instance gets its OWN counter with identical semantics,
// so two instances that issue Writes in the same order mint byte-identical
// memory IDs (the SMT keys for the "memories" namespace) in lockstep.
func newCounterIDGen() func() memory.ID {
	var n byte
	return func() memory.ID {
		n++
		var id memory.ID
		id[0] = n
		return id
	}
}

// openDeterministic opens a fresh cortex over its own temp store with the
// shared fixed clock and a fresh counter idGen.
func openDeterministic(t *testing.T, actor string) *cortex.Cortex {
	t.Helper()
	s, err := store.Open(t.TempDir(), actor, nil)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", actor, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return cortex.New(s, cortex.WithClock(fixedClock), cortex.WithIDGen(newCounterIDGen()))
}

// pref builds a real, valid PreferenceData (Topic required by validation).
func pref(topic string, strength float32) memory.PreferenceData {
	return memory.PreferenceData{
		SchemaVersion: 1,
		Topic:         topic,
		Polarity:      memory.PolarityPrefer,
		StrengthVal:   strength,
		Rationale:     "terse > verbose",
	}
}

// writePref performs one real anchored Write (Preference), mutating the
// memories SMT. Both instances call this in identical order with identical
// data so their anchored world-state matches exactly.
func writePref(t *testing.T, c *cortex.Cortex, topic string, strength float32) memory.URI {
	t.Helper()
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, pref(topic, strength), cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "prefers " + topic},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write(%s): %v", topic, err)
	}
	return uri
}

// TestAssertNoAnchoredDrift_AppendMessage proves req.11.1: a pure derived-lane
// write (AppendMessage) leaves both anchored SMT roots byte-identical.
func TestAssertNoAnchoredDrift_AppendMessage(t *testing.T) {
	c := openDeterministic(t, "andrew")

	// Seed some anchored state so the roots are non-empty (a stronger check
	// than an empty store).
	writePref(t, c, "tone", 0.8)
	writePref(t, c, "verbosity", 0.6)

	err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, aerr := c.AppendMessage(cortex.Message{
			ConversationID: "conv-drift",
			Role:           cortex.RoleUser,
			Content:        "does this move the anchored root? it must not.",
		})
		return aerr
	})
	if err != nil {
		t.Fatalf("AssertNoAnchoredDrift across AppendMessage: %v", err)
	}

	// A second derived write (a tool_call with args) must also not drift.
	if err := cmharness.AssertNoAnchoredDrift(c, func() error {
		_, aerr := c.AppendMessage(cortex.Message{
			ConversationID: "conv-drift",
			Role:           cortex.RoleToolCall,
			ToolName:       "core_execute",
			ToolArgs:       []byte(`{"verb":"launch","args":{"symbol":"PAX"}}`),
		})
		return aerr
	}); err != nil {
		t.Fatalf("AssertNoAnchoredDrift across tool_call AppendMessage: %v", err)
	}
}

// TestCompareAnchoredRoots_ActiveVsInactive proves req.11.2 (active-vs-inactive):
// two instances driven with the SAME anchored Writes end up with byte-identical
// anchored roots, even though instance B additionally interleaves
// continuous-memory AppendMessage writes. It also asserts their FULL
// OverallRoot DIFFERS — documenting honestly that the derived lane grows the
// journal MMR (so OverallRoot is NOT byte-identical; only the anchored
// world-state is).
func TestCompareAnchoredRoots_ActiveVsInactive(t *testing.T) {
	// Instance A: continuous-memory lane INACTIVE (Writes only).
	a := openDeterministic(t, "actor-a")
	// Instance B: continuous-memory lane ACTIVE (same Writes + AppendMessage).
	b := openDeterministic(t, "actor-b")

	topics := []struct {
		topic    string
		strength float32
	}{
		{"tone", 0.8},
		{"verbosity", 0.6},
		{"format", 0.5},
		{"tempo", 0.7},
	}

	for i, tc := range topics {
		// Identical anchored op on BOTH, in identical order → identical
		// minted memory IDs (SMT keys) and identical version bytes.
		writePref(t, a, tc.topic, tc.strength)
		writePref(t, b, tc.topic, tc.strength)

		// B ALSO rides the derived lane. AppendMessage uses conv/seq, NOT
		// the idGen, so it must NOT perturb B's subsequent memory-ID
		// allocation. If it did, the next writePref on B would mint a
		// different ID than A and the anchored roots would diverge — this
		// test would catch it.
		if _, err := b.AppendMessage(cortex.Message{
			ConversationID: "conv-active",
			Role:           cortex.RoleAssistant,
			Content:        fmt.Sprintf("interleaved derived write #%d", i),
		}); err != nil {
			t.Fatalf("B.AppendMessage #%d: %v", i, err)
		}
	}

	// req.11.2: anchored world-state (memories + edges roots) byte-identical.
	if err := cmharness.CompareAnchoredRoots(a, b); err != nil {
		t.Fatalf("CompareAnchoredRoots (active vs inactive): %v", err)
	}

	// Honest nuance: the FULL OverallRoot must DIFFER because B's journal
	// MMR grew derived-lane leaves that A's did not.
	ra, err := a.OverallRoot()
	if err != nil {
		t.Fatalf("a.OverallRoot: %v", err)
	}
	rb, err := b.OverallRoot()
	if err != nil {
		t.Fatalf("b.OverallRoot: %v", err)
	}
	if ra == rb {
		t.Fatalf("OverallRoot unexpectedly identical (a=%x b=%x): the derived lane should have grown B's journal MMR", ra, rb)
	}
}

// TestReplayPreservesRoot_WithDerivedEntries proves req.11.2 D11 baseline: a
// cortex carrying BOTH real anchored Writes AND continuous-memory AppendMessage
// entries in its journal rebuilds to a byte-identical FULL OverallRoot after
// dropping the derived indexes. This is the load-bearing, reusable proof.
func TestReplayPreservesRoot_WithDerivedEntries(t *testing.T) {
	c := openDeterministic(t, "andrew")

	// Interleave anchored Writes and derived AppendMessage so the journal
	// contains both kinds of leaf.
	writePref(t, c, "tone", 0.8)
	if _, err := c.AppendMessage(cortex.Message{
		ConversationID: "conv-replay", Role: cortex.RoleUser, Content: "launch the token",
	}); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	writePref(t, c, "verbosity", 0.6)
	if _, err := c.AppendMessage(cortex.Message{
		ConversationID: "conv-replay", Role: cortex.RoleToolCall, ToolName: "core_execute",
		ToolArgs: []byte(`{"verb":"launch"}`),
	}); err != nil {
		t.Fatalf("AppendMessage tool_call: %v", err)
	}
	if _, err := c.AppendMessage(cortex.Message{
		ConversationID: "conv-replay", Role: cortex.RoleToolResult, ToolName: "core_execute",
		Content: "ok: launched at 0xabc",
	}); err != nil {
		t.Fatalf("AppendMessage tool_result: %v", err)
	}
	writePref(t, c, "format", 0.5)

	res, err := cmharness.ReplayPreservesRoot(c, fixedClock)
	if err != nil {
		t.Fatalf("ReplayPreservesRoot with derived entries present: %v", err)
	}
	if res == nil {
		t.Fatalf("ReplayPreservesRoot returned nil result")
	}
	// Sanity: the rebuild actually re-hashed journal leaves (session entries
	// included) and scanned the anchored memories.
	if res.JournalLeavesAppended == 0 {
		t.Fatalf("JournalLeavesAppended = 0; expected the session+write journal leaves to be re-hashed")
	}
	if res.MemoriesScanned == 0 {
		t.Fatalf("MemoriesScanned = 0; expected the anchored Preference writes to be scanned")
	}
	if res.PreOverallRoot != res.PostOverallRoot {
		t.Fatalf("OverallRoot drift across rebuild: pre=%x post=%x", res.PreOverallRoot, res.PostOverallRoot)
	}
}
