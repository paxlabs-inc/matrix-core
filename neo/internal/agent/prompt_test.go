// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/recall"
)

// goldenDynInputs is the fixed turn-varying input set used to lock the rendered
// dynamic block byte-for-byte against the pre-split renderer.
func goldenDynInputs() (string, []memory.Snippet, []memory.Pattern, []memory.Snippet, []recall.Hit) {
	pinned := "Identity: Neo\nHard rule: never commit unless asked\nActive goal: ship P1-2"
	retrieved := []memory.Snippet{{Text: "user prefers terse output", Note: "preference"}}
	procedural := []memory.Pattern{{Spec: memory.PatternSpec{Name: "build-test-loop", Steps: []string{"build", "test"}}, Coverage: 3}}
	triggered := []memory.Snippet{{Text: "run go test -race on touched packages"}}
	recalled := []recall.Hit{{Role: "user", Text: "ledger lives in knowledge/analysis"}}
	return pinned, retrieved, procedural, triggered, recalled
}

// goldenDynamicBlock is the EXACT dynamic block the single-block buildSystem
// rendered before the split (captured from the live renderer). The split must
// preserve it byte-for-byte; only its POSITION in the window changes.
const goldenDynamicBlock = "\nIdentity: Neo\nHard rule: never commit unless asked\nActive goal: ship P1-2\nApply to this request (behaviors you've learned that fit what you're doing now):\n- run go test -race on touched packages\n\nStory so far (consolidated working memory; the live conversation overrides it on any conflict):\nWe are executing the MATRIX-ENHANCEMENTS roadmap.\n\nRelevant earlier in this conversation (the live exchange below is more current — it wins on any conflict):\n- User: ledger lives in knowledge/analysis\n\nMemory seed (a few durable items that may relate; call memory_recall for the rest — may be stale, the live conversation wins):\n- user prefers terse output [preference]\n\nProven approaches you've used before (apply if the preconditions match; verify the result after):\n- build-test-loop · steps: build → test (3× proven)\n"

// TestBuildSystem_DynamicContentUnchanged locks the dynamic block byte-for-byte
// against the pre-split golden AND proves the split is lossless:
// systemPrompt()+buildDynamic == buildSystem.
func TestBuildSystem_DynamicContentUnchanged(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.summary = "We are executing the MATRIX-ENHANCEMENTS roadmap."
	pinned, retrieved, procedural, triggered, recalled := goldenDynInputs()

	dyn := a.buildDynamic(pinned, retrieved, procedural, triggered, recalled)
	if dyn != goldenDynamicBlock {
		t.Errorf("dynamic block drifted from golden.\n got: %q\nwant: %q", dyn, goldenDynamicBlock)
	}
	// The stable charter + dynamic block must reproduce the exact bytes the
	// single-block buildSystem renders (lossless split).
	if got := a.systemPrompt() + dyn; got != a.buildSystem(pinned, retrieved, procedural, triggered, recalled) {
		t.Error("systemPrompt()+buildDynamic must equal buildSystem byte-for-byte")
	}
}

// TestBuildSystem_PrefixByteStableAcrossTurns proves the system prefix is
// byte-identical turn-over-turn even as the dynamic inputs change — the
// property that lets the provider reuse its longest-stable-prefix cache.
func TestBuildSystem_PrefixByteStableAcrossTurns(t *testing.T) {
	a := New(Options{Config: config.Default()})

	a.summary = "story one"
	a.activeGoal = "goal one"
	a.working = []llm.Message{llm.UserMessage("first")}
	w1 := a.assembleWindow(a.systemPrompt(), a.buildDynamic("pinned one", nil, nil, nil, nil)+"\n\n[context: 10% used]\n")

	// Turn 2: the dynamic state mutates (summary, goal, transcript, pinned, ctx%).
	a.summary = "story two is quite different"
	a.activeGoal = "goal two"
	a.working = append(a.working, llm.AssistantMessage("reply"), llm.UserMessage("second"))
	w2 := a.assembleWindow(a.systemPrompt(), a.buildDynamic("pinned two", []memory.Snippet{{Text: "x"}}, nil, nil, nil)+"\n\n[context: 80% used]\n")

	if w1[0].Role != llm.RoleSystem || w2[0].Role != llm.RoleSystem {
		t.Fatal("window[0] must be the system prefix")
	}
	if w1[0].Content != w2[0].Content {
		t.Errorf("system prefix changed across turns; cache busts.\nturn1=%q\nturn2=%q", w1[0].Content, w2[0].Content)
	}
	for _, leak := range []string{"pinned one", "pinned two", "story one", "story two", "[context:"} {
		if strings.Contains(w1[0].Content, leak) {
			t.Errorf("stable prefix leaked turn-varying content %q", leak)
		}
	}
	if w1[len(w1)-1].Content == w2[len(w2)-1].Content {
		t.Error("dynamic tails should differ across turns")
	}
}

// TestWindow_DynamicTailAfterTranscript proves the window order is
// [stable system] + [append-only transcript] + [dynamic tail], with the dynamic
// content in the trailing message, not the prefix.
func TestWindow_DynamicTailAfterTranscript(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.working = []llm.Message{
		llm.UserMessage("do x"),
		llm.AssistantMessage("on it"),
	}
	stable := a.systemPrompt()
	tail := a.buildDynamic("PINNED-GOAL", nil, nil, nil, nil) + "\n\n[context: 42% used]\n"
	w := a.assembleWindow(stable, tail)

	if len(w) != len(a.working)+2 {
		t.Fatalf("window len = %d, want transcript+2 (%d)", len(w), len(a.working)+2)
	}
	if w[0].Role != llm.RoleSystem || w[0].Content != stable {
		t.Error("window[0] must be the stable system prefix")
	}
	if w[1].Content != "do x" || w[2].Content != "on it" {
		t.Errorf("transcript not preserved in order: %+v", w[1:3])
	}
	last := w[len(w)-1]
	if last.Role != llm.RoleSystem {
		t.Error("dynamic tail must be a trailing system message")
	}
	if !strings.Contains(last.Content, "PINNED-GOAL") || !strings.Contains(last.Content, "[context: 42% used]") {
		t.Errorf("dynamic tail missing content: %q", last.Content)
	}
	if strings.Contains(w[0].Content, "PINNED-GOAL") || strings.Contains(w[0].Content, "[context:") {
		t.Error("dynamic content leaked into the stable prefix")
	}
}

// BenchmarkBuildWindow measures the NEW per-turn window assembly (split stable
// prefix + dynamic tail), mirroring the agent loop: build both parts, the
// combined text for budget accounting, then the message window. Compare against
// BenchmarkBuildWindowLegacy for the m5 alloc/ns regression guard.
func BenchmarkBuildWindow(b *testing.B) {
	a := New(Options{Config: config.Default()})
	a.summary = "story so far"
	a.working = []llm.Message{llm.UserMessage("hello"), llm.AssistantMessage("hi"), llm.UserMessage("more")}
	pinned, retrieved, procedural, triggered, recalled := goldenDynInputs()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stable := a.systemPrompt()
		dyn := a.buildDynamic(pinned, retrieved, procedural, triggered, recalled)
		baseSystem := stable + dyn
		_ = a.budgetPct(baseSystem)
		tail := dyn + "\n\n[context: 50% used]\n"
		_ = a.assembleWindow(stable, tail)
	}
}

// BenchmarkBuildWindowLegacy reproduces the PRE-split assembly (single combined
// system message prepended to the transcript) so the regression guard has a
// real before/after baseline. Test-only measurement scaffold.
func BenchmarkBuildWindowLegacy(b *testing.B) {
	a := New(Options{Config: config.Default()})
	a.summary = "story so far"
	a.working = []llm.Message{llm.UserMessage("hello"), llm.AssistantMessage("hi"), llm.UserMessage("more")}
	pinned, retrieved, procedural, triggered, recalled := goldenDynInputs()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		baseSystem := a.buildSystem(pinned, retrieved, procedural, triggered, recalled)
		_ = a.budgetPct(baseSystem)
		system := baseSystem + "\n\n[context: 50% used]\n"
		_ = append([]llm.Message{llm.SystemMessage(system)}, a.working...)
	}
}
