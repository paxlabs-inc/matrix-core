// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
)

// These tests exercise the REAL Cassandra 2.0 controller against REAL llm.Message
// / llm.ToolCall values and a REAL Agent — no fakes. They cover the controller
// logic directly (the canonical real-agent.Chat fixtures are the wave-5
// verification pass); every assertion is on genuine code paths and types.

// asstToolTurn is an assistant turn that went straight to a tool (empty content).
func asstToolTurn(name, args string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc(name, args)}}
}

// newCasAgent returns an Agent with the controller enabled and its per-turn state
// reset (as Chat does at the top of every turn).
func newCasAgent(t *testing.T) *Agent {
	t.Helper()
	a := New(Options{Config: config.Default()})
	a.turn.casModsThisTurn = 0
	a.turn.casCooldown = map[modTrigger]int{}
	a.turn.casRecord = nil
	return a
}

// loopSig is a signal bundle describing a tool-repeat loop at the given step.
func loopSig(step, repeats int) cassandraSignals {
	return cassandraSignals{
		step:             step,
		closing:          false,
		calls:            []llm.ToolCall{tc("web_search", `{"query":"x"}`)},
		effectiveRepeats: repeats,
		workDone:         true,
	}
}

func TestCassandraLoopKillInjectsDoubt(t *testing.T) {
	a := newCasAgent(t)
	a.working = []llm.Message{
		llm.UserMessage("find X"),
		asstToolTurn("web_search", `{"query":"x"}`),
	}
	// loop_threshold defaults to NoProgressStall-1 (=3). effectiveRepeats=3 arms
	// the doubt side one step before the hard stall.
	if !a.cassandraStep(loopSig(3, a.casLoopThreshold())) {
		t.Fatal("a loop at the threshold must inject a mod")
	}
	if len(a.turn.casRecord) != 1 {
		t.Fatalf("exactly one dual-record expected, got %d", len(a.turn.casRecord))
	}
	rec := a.turn.casRecord[0]
	if rec.Trigger != trigLoop || rec.Side != sideDoubt {
		t.Errorf("loop must classify as (loop, doubt), got (%s, %s)", rec.Trigger, rec.Side)
	}
	if rec.Original != "" {
		t.Errorf("original content of a straight-to-tools turn is empty, got %q", rec.Original)
	}
	got := a.working[1].Content
	if got == "" || !strings.Contains(strings.ToLower(got), "different approach") {
		t.Errorf("the mod must fold in a first-person doubt/step-back realization, got %q", got)
	}
	// Guardrail 1 (content-only): the tool call, role, and the user message are
	// byte-identical.
	if len(a.working[1].ToolCalls) != 1 || a.working[1].ToolCalls[0].Function.Name != "web_search" {
		t.Error("guardrail 1 violated: ToolCalls must be untouched")
	}
	if a.working[1].Role != llm.RoleAssistant {
		t.Error("guardrail 1 violated: Role must be untouched")
	}
	if a.working[0].Content != "find X" {
		t.Error("guardrail 1 violated: the user message must be untouched")
	}
}

func TestCassandraSilentWhenHealthy(t *testing.T) {
	a := newCasAgent(t)
	a.working = []llm.Message{
		llm.UserMessage("do a thing"),
		asstToolTurn("web_search", `{"query":"x"}`),
	}
	before := a.working[1].Content
	// No drift: not closing, no repeats, no cyclic/semantic → zero mods.
	sig := cassandraSignals{step: 5, closing: false, calls: []llm.ToolCall{tc("web_search", `{"query":"x"}`)}, effectiveRepeats: 0, workDone: true}
	if a.cassandraStep(sig) {
		t.Fatal("a healthy step must NOT modify the window")
	}
	if len(a.turn.casRecord) != 0 {
		t.Errorf("a healthy step records nothing, got %d", len(a.turn.casRecord))
	}
	if a.working[1].Content != before {
		t.Errorf("a healthy step leaves content byte-identical, got %q want %q", a.working[1].Content, before)
	}
}

func TestCassandraMinStepAndPerTurnCap(t *testing.T) {
	a := newCasAgent(t)
	a.working = []llm.Message{llm.UserMessage("g"), asstToolTurn("web_search", "{}")}
	// Before min_step (default 2): silent even under a loop.
	if a.cassandraStep(loopSig(1, a.casLoopThreshold())) {
		t.Fatal("no mod may fire before min_step")
	}
	// At the per-turn cap: silent.
	a.turn.casModsThisTurn = a.casMaxMods()
	if a.cassandraStep(loopSig(4, a.casLoopThreshold())) {
		t.Fatal("no mod may fire once the per-turn cap is spent")
	}
}

func TestCassandraCooldownPerTrigger(t *testing.T) {
	a := newCasAgent(t)
	a.working = []llm.Message{llm.UserMessage("g"), asstToolTurn("web_search", "{}")}
	if !a.cassandraStep(loopSig(3, a.casLoopThreshold())) {
		t.Fatal("first loop mod must fire")
	}
	// Same trigger class one step later is within the cooldown window (default 2).
	a.working = append(a.working, asstToolTurn("web_search", "{}"))
	if a.cassandraStep(loopSig(4, a.casLoopThreshold())) {
		t.Fatal("the same trigger within the cooldown window must stay silent")
	}
}

func TestCassandraGuardrailRefusesNonAssistant(t *testing.T) {
	a := newCasAgent(t)
	// The last message is a TOOL result, not an assistant turn: the edit primitive
	// must refuse it structurally (guardrail 1).
	a.working = []llm.Message{
		llm.UserMessage("g"),
		asstToolTurn("web_search", "{}"),
		llm.ToolResult("web_search", "web_search", "the result"),
	}
	before := a.working[2].Content
	if a.cassandraEdit(len(a.working)-1, "some doubt", trigLoop, sideDoubt, 3) {
		t.Fatal("cassandraEdit must refuse a non-assistant target")
	}
	if a.working[2].Content != before {
		t.Error("a refused edit must not mutate the tool message")
	}
	if len(a.turn.casRecord) != 0 {
		t.Error("a refused edit records nothing")
	}
}

func TestCassandraAuditEmitted(t *testing.T) {
	var events []AuditEvent
	a := New(Options{Config: config.Default(), AuditObserver: func(ev AuditEvent) { events = append(events, ev) }})
	a.turn.casCooldown = map[modTrigger]int{}
	a.working = []llm.Message{llm.UserMessage("g"), asstToolTurn("web_search", "{}")}
	if !a.cassandraStep(loopSig(3, a.casLoopThreshold())) {
		t.Fatal("expected a mod to fire")
	}
	if len(events) != 1 || events[0].Type != auditEventMod {
		t.Fatalf("expected one %s audit event, got %+v", auditEventMod, events)
	}
	f := events[0].Fields
	for _, k := range []string{"step", "trigger", "side", "target_index", "original_content", "cassandra_mod"} {
		if _, ok := f[k]; !ok {
			t.Errorf("audit event missing field %q", k)
		}
	}
	if f["trigger"] != string(trigLoop) || f["side"] != string(sideDoubt) {
		t.Errorf("audit fields wrong: trigger=%v side=%v", f["trigger"], f["side"])
	}
}

func TestCassandraFoldPreservesOriginalContent(t *testing.T) {
	a := newCasAgent(t)
	// An assistant turn WITH a preamble: the mod is folded AFTER the original,
	// which is preserved verbatim (additive, metacognition-only).
	a.working = []llm.Message{
		llm.UserMessage("g"),
		{Role: llm.RoleAssistant, Content: "Checking the balance again.", ToolCalls: []llm.ToolCall{tc("web_search", "{}")}},
	}
	if !a.cassandraStep(loopSig(3, a.casLoopThreshold())) {
		t.Fatal("expected a mod")
	}
	got := a.working[1].Content
	if !strings.HasPrefix(got, "Checking the balance again.") {
		t.Errorf("original content must be preserved as the prefix, got %q", got)
	}
	if got == "Checking the balance again." {
		t.Error("the mod must have been folded in after the original")
	}
}

func TestCassandraTwoSidedAssuranceOnRepeatedRead(t *testing.T) {
	a := newCasAgent(t)
	a.working = []llm.Message{llm.UserMessage("g"), asstToolTurn("fs__read_file", `{"path":"/x"}`)}
	// A MILD repeat (below the loop threshold) of a READ/verify tool is
	// over-verification → the ASSURANCE side (commit and move on), proving the
	// controller is two-sided, not a one-sided doubt-spammer.
	sig := cassandraSignals{
		step:           3,
		closing:        false,
		calls:          []llm.ToolCall{tc("fs__read_file", `{"path":"/x"}`)},
		semanticRepeat: true,
		workDone:       true,
	}
	if !a.cassandraStep(sig) {
		t.Fatal("a mild repeated read must trigger the assurance side")
	}
	rec := a.turn.casRecord[0]
	if rec.Side != sideAssurance || rec.Trigger != trigOverVerify {
		t.Errorf("a repeated read must classify as (over_verify, assurance), got (%s, %s)", rec.Trigger, rec.Side)
	}
}

// TestBuildCassandraSignalsDetectsRepeat proves the signal builder derives the
// repeat read from the loop's live state one step early (matching noteBatch's
// verdict), so the loop trigger can fire before the hard stall.
func TestBuildCassandraSignalsDetectsRepeat(t *testing.T) {
	a := New(Options{Config: config.Default()})
	calls := []llm.ToolCall{tc("web_search", `{"query":"x"}`)}
	msg := llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}
	prevSig := batchSignature(calls)
	distinct := map[string]struct{}{"web_search": {}}
	// Current batch is byte-identical to the previous → a repeat: effectiveRepeats
	// = committed repeats (2) + 1.
	sig := a.buildCassandraSignals(4, msg, 2, []string{prevSig}, prevSig, calls, distinct, false)
	if sig.effectiveRepeats != 3 {
		t.Errorf("a repeated batch must yield committed+1 effective repeats, got %d", sig.effectiveRepeats)
	}
	// A DISTINCT batch resets: effectiveRepeats stays at the committed count.
	other := []llm.ToolCall{tc("fs__read_file", `{"path":"/y"}`)}
	omsg := llm.Message{Role: llm.RoleAssistant, ToolCalls: other}
	sig2 := a.buildCassandraSignals(4, omsg, 2, []string{prevSig}, prevSig, calls, distinct, false)
	if sig2.effectiveRepeats != 2 {
		t.Errorf("a distinct batch must not add to the repeat count, got %d", sig2.effectiveRepeats)
	}
}

func TestCassandraEpisodicUngroundedExactPredicate(t *testing.T) {
	a := newCasAgent(t)
	cases := []struct {
		name    string
		pending bool
		closing bool
		want    modTrigger
	}{
		{name: "detected ungrounded closing", pending: true, closing: true, want: trigEpisodicUngrounded},
		{name: "not detected", pending: false, closing: true},
		{name: "not closing", pending: true, closing: false},
		{name: "ordinary", pending: false, closing: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trig, _ := a.cassandraClassify(cassandraSignals{step: 3, episodicPending: tc.pending, closing: tc.closing})
			if trig != tc.want {
				t.Fatalf("trigger=%q want %q", trig, tc.want)
			}
		})
	}
}

func TestCassandraEpisodicUngroundedContentOnlyDualRecord(t *testing.T) {
	a := newCasAgent(t)
	a.working = []llm.Message{llm.UserMessage("Remember when?"), llm.AssistantMessage("It happened last week.")}
	beforeUserRole, beforeUserContent := a.working[0].Role, a.working[0].Content
	if !a.cassandraStep(cassandraSignals{step: 3, closing: true, episodicPending: true}) {
		t.Fatal("episodic failsafe did not fire")
	}
	if len(a.turn.casRecord) != 1 || a.turn.casRecord[0].Trigger != trigEpisodicUngrounded || a.turn.casRecord[0].Original != "It happened last week." {
		t.Fatalf("dual record=%+v", a.turn.casRecord)
	}
	if a.working[0].Role != beforeUserRole || a.working[0].Content != beforeUserContent {
		t.Fatal("user turn was modified")
	}
	if !strings.Contains(a.working[1].Content, "memory_recall") || !strings.HasPrefix(a.working[1].Content, "It happened last week.") {
		t.Fatalf("folded assistant content=%q", a.working[1].Content)
	}
}
