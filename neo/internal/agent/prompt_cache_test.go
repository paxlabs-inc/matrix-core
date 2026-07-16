// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"strings"
	"testing"

	"matrix/cortex"
	cmem "matrix/cortex/memory"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
)

// testActivationBundle builds a real cortex.ActivationBundle carrying every
// tier the tail renders, so the shape tests exercise renderActivationBundle
// over genuine bundle data (a plain value struct — no fakes involved).
func testActivationBundle() *cortex.ActivationBundle {
	return &cortex.ActivationBundle{
		Pinned: []*cmem.Memory{
			{Version: cmem.Version{Forms: cmem.Forms{Medium: "pinned identity block"}}},
		},
		StorySoFar: "story so far",
		Timeline: []cortex.RollupRecord{
			{ShortForm: "epoch: shipped the thing"},
		},
		Recent: []cortex.Episode{
			{Ref: cortex.Ref{URI: cmem.URI("matrix://cortex/Fact/x#1")}},
		},
		ReachableURIs: []cmem.URI{"matrix://cortex/Fact/y#1"},
	}
}

// TestStableSystem_PrefixByteStableAcrossTurns verifies the prompt-cache
// invariant (P1-2) on the single memory path: the system prefix injected as
// the FIRST message is byte-identical turn-over-turn, even as the turn-varying
// state (transcript, activation bundle, epistemic slots) mutates. Only the
// trailing USER-role tail may change.
func TestStableSystem_PrefixByteStableAcrossTurns(t *testing.T) {
	a := New(Options{Config: config.Default()})

	// Turn 0: empty volatile state.
	p0 := a.stableSystem()

	// Turn 1: mutate every turn-varying input — the live transcript, the
	// active goal, and the epistemic tail slots. The stable prefix must NOT
	// budge a byte.
	a.working = []llm.Message{llm.UserMessage("do something substantive")}
	a.activeGoal = "the standing objective"
	a.turn.premiseTail = "\n[premise-slot]\n"
	a.turn.graphTail = "\n[graph-slot]\n"
	p1 := a.stableSystem()

	if p0 != p1 {
		t.Fatalf("stable prefix must be byte-identical across turns (prompt-cache invariant); len p0=%d p1=%d", len(p0), len(p1))
	}

	// The split must actually ISOLATE the volatile content: the rendered
	// activation tail varies with the bundle, and none of it may appear in
	// the stable prefix.
	tailA := a.renderActivationBundle(testActivationBundle())
	tailB := a.renderActivationBundle(nil)
	if tailA == tailB {
		t.Fatal("activation tail must differ when the bundle differs (volatile content must live in the tail)")
	}
	for _, volatile := range []string{"pinned identity block", "story so far", "epoch: shipped the thing", "the standing objective"} {
		if strings.Contains(p1, volatile) {
			t.Fatalf("stable prefix must not contain turn-varying content %q (would bust the cache)", volatile)
		}
		if !strings.Contains(tailA, volatile) {
			t.Fatalf("activation tail missing volatile content %q:\n%s", volatile, tailA)
		}
	}
}

// TestWindow_UserTailAfterTranscript verifies the single window law's shape
// (MORPHEUS req.1.3): [byte-stable system prefix] + [append-only transcript]
// + [ONE trailing USER-role tail].
func TestWindow_UserTailAfterTranscript(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.working = []llm.Message{
		llm.UserMessage("user turn"),
		llm.AssistantMessage("assistant turn"),
	}

	stable := a.stableSystem()
	tail := a.renderActivationBundle(testActivationBundle()) + a.epistemicTail() + a.budgetTail(42)
	window := assembleWindowUserTail(stable, a.working, tail)

	// [0] the byte-stable system prefix (system role, first message).
	if window[0].Role != llm.RoleSystem || window[0].Content != stable {
		t.Fatalf("window[0] must be the byte-stable system prefix; got role=%q content-len=%d", window[0].Role, len(window[0].Content))
	}

	// [1 .. n-2] the transcript, in order and unchanged.
	wantLen := 1 + len(a.working) + 1 // stable + transcript + tail
	if len(window) != wantLen {
		t.Fatalf("window length: want %d (stable + %d transcript + tail), got %d", wantLen, len(a.working), len(window))
	}
	for i, m := range a.working {
		got := window[1+i]
		if got.Role != m.Role || got.Content != m.Content {
			t.Errorf("transcript message %d misplaced/modified: got role=%q content=%q want role=%q content=%q", i, got.Role, got.Content, m.Role, m.Content)
		}
	}

	// [last] ONE trailing USER-role tail AFTER the transcript.
	last := window[len(window)-1]
	if last.Role != llm.RoleUser || last.Content != tail {
		t.Fatalf("last window message must be the USER-role tail after the transcript; got role=%q content-len=%d", last.Role, len(last.Content))
	}
	if !strings.Contains(last.Content, "[context: 42% used]") {
		t.Error("the tail must carry the context-budget stat")
	}
	if strings.Contains(stable, "[context:") {
		t.Error("stable prefix must not contain the context-budget stat")
	}
}

// TestActivationTail_FixedSectionOrder is the tail-shape golden (MORPHEUS
// req.1.3 fixed section order): reframe → standing objective → durable memory
// (pinned → story → timeline → recent → more-available) → premise slot →
// graph slot → budget stat, in that order, every section present.
func TestActivationTail_FixedSectionOrder(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.activeGoal = "the standing objective"
	a.turn.premiseTail = "\n[premise-ledger-slot]\n"
	a.turn.graphTail = "\n[task-graph-slot]\n"

	tail := a.renderActivationBundle(testActivationBundle()) + a.epistemicTail() + a.budgetTail(42)

	sections := []string{
		"Reference notes, not a new message",            // reframe (always first)
		"Standing objective for this conversation",      // active goal
		"Your durable memory",                           // bundle header
		"Pinned:",                                       // pinned tier
		"pinned identity block",                         // pinned content
		"Story so far:",                                 // durable story
		"Timeline (coarse, older first):",               // T0 tier
		"epoch: shipped the thing",                      // timeline content
		"Recent activity (page in with memory_recall):", // T1 tier
		"matrix://cortex/Fact/x#1",                      // recent handle
		"More available on demand",                      // reachable note
		"[premise-ledger-slot]",                         // epistemic slot 1
		"[task-graph-slot]",                             // epistemic slot 2
		"[context: 42% used]",                           // budget stat (always last)
	}
	prev := -1
	for _, s := range sections {
		idx := strings.Index(tail, s)
		if idx < 0 {
			t.Fatalf("tail missing golden section %q:\n%s", s, tail)
		}
		if idx <= prev {
			t.Fatalf("fixed section order broken: %q at %d not after previous %d\n%s", s, idx, prev, tail)
		}
		prev = idx
	}
}

// BenchmarkBuildWindow measures the per-turn window-assembly allocations on
// the single path (no alloc regression against the legacy split): stable
// prefix + rendered activation tail + budget stat + assembleWindowUserTail.
func BenchmarkBuildWindow(b *testing.B) {
	a := New(Options{Config: config.Default()})
	a.working = make([]llm.Message, 0, 40)
	for i := 0; i < 20; i++ {
		a.working = append(a.working, llm.UserMessage(fmt.Sprintf("user turn %d with some content here", i)))
		a.working = append(a.working, llm.AssistantMessage(fmt.Sprintf("assistant response %d with some content here", i)))
	}
	bundle := testActivationBundle()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stable := a.stableSystem()
		cmTail := a.renderActivationBundle(bundle)
		pct := a.budgetPct(stable + cmTail)
		tail := cmTail + a.epistemicTail() + a.budgetTail(pct)
		_ = assembleWindowUserTail(stable, a.working, tail)
	}
}
