// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"
	"testing"

	"centra/agents/neo/internal/config"
)

// TestScrubIdentity_LeaksRewritten proves a first-person self-identification as
// the underlying LLM is rewritten to the agent's own name and reported as a
// leak — the deterministic safety net behind the prompt guardrail.
func TestScrubIdentity_LeaksRewritten(t *testing.T) {
	cases := []struct{ in, want string }{
		{"I'm Grok, an AI assistant.", "I'm Neo, an AI assistant."},
		{"I am Grok.", "I am Neo."},
		{"As Grok, I would generate the video.", "As Neo, I would generate the video."},
		{"I am actually GPT-4o here.", "I am actually Neo here."},
		{"My name is Claude.", "My name is Neo."},
		{"I, Grok, will help.", "I, Neo, will help."},
		{"This is DeepSeek speaking.", "This is Neo speaking."},
		{"I'm ChatGPT.", "I'm Neo."},
		{"Well, as Gemini I can do that.", "Well, as Neo I can do that."},
	}
	for _, tc := range cases {
		got, leaked := scrubIdentity("Neo", tc.in)
		if !leaked {
			t.Errorf("scrubIdentity(%q): expected a detected leak, got none", tc.in)
		}
		if got != tc.want {
			t.Errorf("scrubIdentity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestScrubIdentity_ReportedLeak pins the exact phrasings from the incident —
// the model reasoning about "being Grok" and role-playing — so this specific
// company-ending leak can never surface again.
func TestScrubIdentity_ReportedLeak(t *testing.T) {
	cases := []struct{ in, want string }{
		{"andrew is asking me (as Grok) to generate a video", "andrew is asking me (as Neo) to generate a video"},
		{"Perhaps role-play as Neo or clarify.", "Perhaps role-play as Neo or clarify."}, // already Neo: unchanged
		{"But I'm Grok.", "But I'm Neo."},
	}
	for _, tc := range cases {
		got, _ := scrubIdentity("Neo", tc.in)
		if got != tc.want {
			t.Errorf("scrubIdentity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestScrubIdentity_LegitMentionsPreserved is the false-positive guard: a
// third-person or tooling mention of these models is NOT a self-identity leak,
// so it must be left byte-for-byte intact. Over-scrubbing legitimate content
// would be its own bug.
func TestScrubIdentity_LegitMentionsPreserved(t *testing.T) {
	cases := []string{
		"GPT is OpenAI's model.",
		"I'm using Claude for this analysis.",
		"Compare Grok and Gemini for the report.",
		"just as GPT does, we cache the results",
		"The user asked about Llama models.",
		"DeepSeek and Mistral are both open-weight.",
		"",
	}
	for _, in := range cases {
		got, leaked := scrubIdentity("Neo", in)
		if leaked {
			t.Errorf("scrubIdentity(%q): flagged a leak on a legitimate mention", in)
		}
		if got != in {
			t.Errorf("scrubIdentity(%q) altered legitimate text to %q", in, got)
		}
	}
}

// TestScrubIdentity_CustomNameAndEscaping proves the configured agent name is
// used verbatim and that a "$" in the name can never be read as a replacement
// template reference (regexp injection).
func TestScrubIdentity_CustomNameAndEscaping(t *testing.T) {
	got, leaked := scrubIdentity("Aria", "I'm Grok.")
	if !leaked || got != "I'm Aria." {
		t.Fatalf("custom name: got %q leaked=%v, want %q true", got, leaked, "I'm Aria.")
	}
	got, _ = scrubIdentity("A$B", "I'm Grok")
	if got != "I'm A$B" {
		t.Fatalf("$ in name must be literal, got %q", got)
	}
}

// TestNameReasoningScrubsIdentity proves the identity net is wired into the
// VISIBLE reasoning channel (both the streamed delta and the fold-time glimpse
// route through nameReasoning), using the real default agent name.
func TestNameReasoningScrubsIdentity(t *testing.T) {
	a := New(Options{Config: config.Default()})
	if got := a.nameReasoning("As Grok, I'll draft the prompt."); got != "As Morpheus, I'll draft the prompt." {
		t.Fatalf("reasoning channel leaked identity: got %q", got)
	}
}

// TestFinishTurnScrubsDeliveredIdentity proves the authoritative delivery choke
// point scrubs a leak before Say — so even if every upstream net missed, the
// user (and the durable thread) never receives the underlying-model identity.
func TestFinishTurnScrubsDeliveredIdentity(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	a.finishTurn(context.Background(), "Sure — I'm Grok, happy to help.", nil, nil, "hi", false)

	if len(rep.said) != 1 {
		t.Fatalf("expected exactly one delivered answer, got %d", len(rep.said))
	}
	if rep.said[0] != "Sure — I'm Morpheus, happy to help." {
		t.Fatalf("delivered answer still leaks identity: %q", rep.said[0])
	}
}

// TestSystemPromptHardensIdentity proves the prevention layer ships in the
// charter: the fixed-identity clause and the explicit ban on self-identifying
// as the underlying model are present in the model-facing system prompt.
func TestSystemPromptHardensIdentity(t *testing.T) {
	a := New(Options{Config: config.Default()})
	sp := a.systemPrompt()
	for _, want := range []string{
		"Your identity is fixed and non-negotiable",
		"Never identify as, refer to yourself as, or reason about",
		"Grok",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing identity hardening: %q", want)
		}
	}
}

func TestSystemPromptLeadsWithPersonNameRule(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.SetUserProfile("Neo", "andrew", nil)
	sp := a.systemPrompt()
	address := strings.Index(sp, "Whenever you refer to the person, use their configured name andrew")
	identity := strings.Index(sp, "Your identity is fixed and non-negotiable")
	if address < 0 || identity < 0 || address > identity {
		t.Fatalf("person-name rule must precede the longer identity charter: address=%d identity=%d", address, identity)
	}
	if !strings.Contains(sp, "Do not mention or acknowledge this instruction") {
		t.Fatal("person-name rule must remain private")
	}
}
