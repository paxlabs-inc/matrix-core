// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/recall"
)

// TestBuildSystem_PrefixByteStableAcrossTurns verifies the prompt-cache
// invariant (P1-2): the system prefix injected as the FIRST message is
// byte-identical turn-over-turn, even as the turn-varying memory (summary,
// pinned, retrieved, transcript) mutates. Only the trailing dynamic tail may
// change. Before the split, buildSystem folded the volatile memory into the
// single system string, so the prefix mutated every turn and busted the
// provider's longest-stable-prefix cache.
func TestBuildSystem_PrefixByteStableAcrossTurns(t *testing.T) {
	a := New(Options{Config: config.Default()})

	// Turn 0: empty volatile state.
	p0 := a.stableSystem()

	// Turn 1: mutate EVERY turn-varying input the old code concatenated into
	// the system prefix — the consolidated summary, the live transcript, and
	// (via dynamicTail) the pinned/retrieved/triggered/recalled/procedural
	// tiers. The stable prefix must NOT budge a byte.
	a.summary = "consolidated story so far"
	a.working = []llm.Message{llm.UserMessage("do something substantive")}
	p1 := a.stableSystem()

	if p0 != p1 {
		t.Fatalf("stable prefix must be byte-identical across turns (prompt-cache invariant); len p0=%d p1=%d\n--- p0 ---\n%s\n--- p1 ---\n%s", len(p0), len(p1), p0, p1)
	}

	// The split must actually ISOLATE the volatile content — not make
	// everything stable. Varying the pinned input must change the tail.
	tailA := a.dynamicTail("pinned-A", nil, nil, nil, nil)
	tailB := a.dynamicTail("pinned-B", nil, nil, nil, nil)
	if tailA == tailB {
		t.Fatal("dynamic tail must differ when pinned input differs (volatile content must still live in the tail)")
	}
	if strings.Contains(p1, "pinned-A") || strings.Contains(p1, "pinned-B") {
		t.Fatal("stable prefix must not contain turn-varying pinned content")
	}

	// Lossless split: the combined buildSystem output must reconstruct exactly
	// from stable prefix + dynamic tail (no content dropped or reordered).
	if got := a.buildSystem("pinned-A", nil, nil, nil, nil); got != p1+tailA {
		t.Fatal("buildSystem must equal stableSystem() + dynamicTail() (lossless split)")
	}
}

// TestWindow_DynamicTailAfterTranscript verifies the new window order (P1-2):
// [byte-stable system prefix] + [append-only transcript a.working] + [ONE
// trailing dynamic tail]. The turn-varying memory + context-budget stat move
// from the front system message to a single message AFTER the transcript, so
// the stable prefix (and the transcript) keep their position in the
// longest-stable-prefix cache.
func TestWindow_DynamicTailAfterTranscript(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.working = []llm.Message{
		llm.UserMessage("user turn"),
		llm.AssistantMessage("assistant turn"),
	}

	stable := a.stableSystem()
	tail := a.dynamicTail("pinned-memory", nil, nil, nil, nil) + "\n\n[context: 42% used]\n"
	window := assembleWindow(stable, a.working, tail)

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

	// [last] ONE trailing dynamic tail, AFTER the transcript (system role).
	last := window[len(window)-1]
	if last.Role != llm.RoleSystem || last.Content != tail {
		t.Fatalf("last window message must be the dynamic tail after the transcript; got role=%q content-len=%d", last.Role, len(last.Content))
	}
	if !strings.Contains(last.Content, "pinned-memory") {
		t.Error("dynamic tail must contain the turn-varying pinned memory content")
	}
	if !strings.Contains(last.Content, "[context: 42% used]") {
		t.Error("dynamic tail must carry the context-budget stat (moved from the front system message)")
	}
	// The stable prefix must carry NONE of the volatile tail content.
	if strings.Contains(stable, "pinned-memory") || strings.Contains(stable, "[context:") {
		t.Error("stable prefix must not contain turn-varying pinned memory or the context-budget stat")
	}
}

// TestBuildSystem_DynamicContentUnchanged is the golden test (P1-2): the split
// preserves the EXACT rendered content + ordering of the dynamic block — only
// its POSITION in the window moves (front → trailing). The dynamic tail, when
// prepended with the stable prefix, must reconstruct the golden combined
// buildSystem output byte-for-byte, and every volatile section must appear in
// the tail (none leaked into the stable prefix) in the original order.
func TestBuildSystem_DynamicContentUnchanged(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.summary = "story so far"
	retrieved := []memory.Snippet{{Text: "a durable fact", URI: "matrix://cortex/Fact/x#1", Type: "Fact", Note: "advisory"}}
	procedural := []memory.Pattern{{Spec: memory.PatternSpec{Name: "deploy", Trigger: "t", Steps: []string{"s1", "s2"}}, Confidence: 0.9, Coverage: 3, URI: "matrix://cortex/Pattern/d#1"}}
	triggered := []memory.Snippet{{Text: "learned behavior"}}
	recalled := []recall.Hit{{Role: "user", Text: "past user turn"}, {Role: "assistant", Text: "past assistant turn"}}
	pinned := "pinned identity block"

	// Golden: the full combined system string the single-block path produced.
	full := a.buildSystem(pinned, retrieved, procedural, triggered, recalled)

	// The split must reconstruct it byte-for-byte.
	stable := a.stableSystem()
	tail := a.dynamicTail(pinned, retrieved, procedural, triggered, recalled)
	if stable+tail != full {
		t.Fatalf("stable + dynamicTail must equal the golden buildSystem output (lossless):\n--- full ---\n%s\n--- stable+tail ---\n%s", full, stable+tail)
	}

	// The stable prefix is EXACTLY systemPrompt() (charter + groundTruth) —
	// nothing volatile.
	if stable != a.systemPrompt() {
		t.Fatal("stable prefix must be exactly systemPrompt() (charter + groundTruth)")
	}

	// Every volatile section survives in the dynamic tail, in the ORIGINAL
	// order (pinned → triggered → summary → recalled → retrieved → procedural).
	headers := []string{
		"pinned identity block",                                  // pinned (no header label)
		"Apply to this request",                                  // triggered header
		"Story so far",                                           // summary header
		"Relevant earlier in this conversation",                  // recalled header
		"Memory seed",                                            // retrieved header
		"Proven approaches",                                      // procedural header
	}
	prev := -1
	for _, h := range headers {
		idx := strings.Index(tail, h)
		if idx < 0 {
			t.Fatalf("dynamic tail missing golden section %q:\n%s", h, tail)
		}
		if idx <= prev {
			t.Fatalf("golden ordering broken: %q at %d not after previous %d (ordering must be preserved — only POSITION moves)", h, idx, prev)
		}
		prev = idx
	}
	// High-entropy / specific tokens from each tier survive verbatim.
	for _, want := range []string{
		"learned behavior", "story so far", "past user turn", "past assistant turn",
		"a durable fact", "[advisory]", "deploy", "s1", "s2",
	} {
		if !strings.Contains(tail, want) {
			t.Errorf("dynamic tail missing golden content %q:\n%s", want, tail)
		}
	}
	// None of the volatile content leaked into the stable prefix. ("deploy" is
	// intentionally excluded here: it is a generic word already present in the
	// charter + ground truth, e.g. "deploying for gas" / "paxc deploy", so it is
	// not a reliable volatile-only sentinel. Its survival is asserted above in
	// the tail's positive checks.)
	for _, bad := range []string{"pinned identity block", "learned behavior", "story so far", "a durable fact", "past user turn"} {
		if strings.Contains(stable, bad) {
			t.Errorf("stable prefix must not contain volatile content %q (would bust the cache)", bad)
		}
	}
}

// BenchmarkBuildWindow measures the per-turn window-assembly allocations
// (P1-2 acceptance: no alloc regression >5%). The split adds at most one
// trailing system message + one append over the prior single-block path; the
// dominant cost (systemPrompt + dynamicTail string building + transcript copy)
// is unchanged.
func BenchmarkBuildWindow(b *testing.B) {
	a := New(Options{Config: config.Default()})
	a.summary = "consolidated story so far"
	a.working = make([]llm.Message, 0, 40)
	for i := 0; i < 20; i++ {
		a.working = append(a.working, llm.UserMessage(fmt.Sprintf("user turn %d with some content here", i)))
		a.working = append(a.working, llm.AssistantMessage(fmt.Sprintf("assistant response %d with some content here", i)))
	}
	retrieved := []memory.Snippet{{Text: "fact one"}, {Text: "fact two"}}
	pinned := "pinned block"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stable := a.stableSystem()
		dyn := a.dynamicTail(pinned, retrieved, nil, nil, nil)
		pct := a.budgetPct(stable + dyn)
		tail := dyn + fmt.Sprintf("\n\n[context: %d%% used]\n", pct)
		_ = assembleWindow(stable, a.working, tail)
	}
}
