// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// withheld_answer_test.go pins the 2026-07-22 loopty-loop fixes: a close-guard
// rejection must never (a) loop unbounded on the unread-overflow guard,
// (b) gate the close on a memory-recall spill, or (c) write the withheld
// answer into the durable cortex transcript as if the user had seen it.
package agent

import (
	"context"
	"strings"
	"testing"

	"matrix/cortex"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// TestCloseGuardChain_OverflowGuardStandsDownAfterCap pins the unread-overflow
// guard's bounded posture: it may block a close at most overflowNudgeCap times
// per turn — each steer honestly stating the answer was NOT delivered — and
// then stands down so the composed answer ships. The unbounded form withheld
// complete answers until the unified unproductive cap killed the whole turn.
func TestCloseGuardChain_OverflowGuardStandsDownAfterCap(t *testing.T) {
	a := chainAgent(t, nil, nil)
	unreadOverflow(t, a)
	const answer = "here is the finished report"

	for i := 0; i < overflowNudgeCap; i++ {
		name, dec := a.evalCloseChain(&closeContext{res: bareResult(answer, "stop"), answer: answer})
		if name != "unread_overflow" || dec.verdict != verdictNudge {
			t.Fatalf("attempt %d: fired %q/%q, want unread_overflow/nudge", i+1, name, dec.verdict)
		}
		if dec.err != nil {
			t.Fatalf("attempt %d: unexpected escalation: %v", i+1, dec.err)
		}
	}

	// The steer must tell the model the truth: the answer was withheld.
	var nudges []string
	for _, m := range a.working {
		if m.IsGuidance() {
			nudges = append(nudges, m.Content)
		}
	}
	if len(nudges) != overflowNudgeCap {
		t.Fatalf("got %d guidance nudges, want %d", len(nudges), overflowNudgeCap)
	}
	for _, n := range nudges {
		if !strings.Contains(n, "NOT delivered") || !strings.Contains(n, "ovf-1") {
			t.Fatalf("overflow steer must state non-delivery and name the token, got %q", n)
		}
	}

	// Past its cap the guard stands down even though the overflow is STILL
	// unread — the answer delivers instead of the turn dying.
	if !a.overflowUnread() {
		t.Fatal("precondition: the overflow must still be unread")
	}
	name, dec := a.evalCloseChain(&closeContext{res: bareResult(answer, "stop"), answer: answer})
	if name != "deliver" || dec.verdict != verdictDeliver {
		t.Fatalf("past the cap: fired %q/%q, want deliver/deliver", name, dec.verdict)
	}
}

// TestCapToolResult_RecallSpillDoesNotGateClose pins the gate exemption: an
// oversized memory_recall result still spills to a readable overflow file, but
// its entry starts read — prior context must not hold a finished answer
// hostage. An evidence-bearing tool's spill still arms the guard.
func TestCapToolResult_RecallSpillDoesNotGateClose(t *testing.T) {
	a := New(Options{Config: config.Default()})
	t.Cleanup(func() {
		if a.turn.overflow != nil {
			a.turn.overflow.cleanup()
		}
	})
	big := strings.Repeat("m", maxToolResultChars*2)

	got := a.capToolResult(tools.MemoryRecallTool, big)
	if !strings.Contains(got, "truncation_notice") {
		t.Fatal("a recall spill must still carry the truncation notice")
	}
	if a.overflowUnread() {
		t.Fatal("a memory_recall spill must not arm the unread-overflow close guard")
	}
	if chunk, _, _, ok := a.turn.overflow.read("ovf-1", 0, 100); !ok || chunk == "" {
		t.Fatal("the recall spill must remain readable through read_overflow")
	}

	if _ = a.capToolResult("fetch", big); !a.overflowUnread() {
		t.Fatal("an evidence-tool spill must still arm the unread-overflow close guard")
	}
}

// TestBareAnswer_DurableRecordOnlyOnDelivery drives deliberate/closeTurn over a
// REAL cortex pager and proves the durable-transcript contract: a bare answer
// enters cortex only when it actually delivers. The old order (record at
// deliberate, judge at close) wrote every guard-rejected answer into durable
// memory as if the user had seen it — the model then recalled "I already
// answered" forever after.
func TestBareAnswer_DurableRecordOnlyOnDelivery(t *testing.T) {
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-withheld"
	cfg.CassandraEnabled = false
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	const conv = "conv-withheld"
	a := New(Options{Config: cfg, Pager: pager, Tools: &tools.Manager{}, ConvID: conv})
	t.Cleanup(func() {
		if a.turn.overflow != nil {
			a.turn.overflow.cleanup()
		}
	})

	assistants := func() []string {
		t.Helper()
		msgs, terr := pager.Transcript(conv, 0, 100)
		if terr != nil {
			t.Fatalf("pager.Transcript: %v", terr)
		}
		var out []string
		for _, m := range msgs {
			if m.Role == cortex.RoleAssistant {
				out = append(out, m.Content)
			}
		}
		return out
	}

	// A bare answer at deliberate is NOT recorded — its fate is undecided.
	res := bareResult("Here is the finished report.", "stop")
	a.deliberate(0, res, true)
	if got := assistants(); len(got) != 0 {
		t.Fatalf("a bare answer must not be durably recorded at deliberate, got %v", got)
	}

	// A guard-rejected close leaves it unrecorded: the user never saw it.
	unreadOverflow(t, a)
	if finished, cerr := a.closeTurn(context.Background(), res, false, "produce the report"); finished || cerr != nil {
		t.Fatalf("closeTurn under unread overflow = (%v, %v), want rejected (false, nil)", finished, cerr)
	}
	if got := assistants(); len(got) != 0 {
		t.Fatalf("a withheld answer must never enter the durable transcript, got %v", got)
	}

	// Reading the overflow clears the guard; the SAME answer now delivers and
	// is recorded exactly once.
	if _, isErr := a.readOverflow(map[string]interface{}{"token": "ovf-1"}); isErr {
		t.Fatal("read_overflow of the armed token failed")
	}
	finished, cerr := a.closeTurn(context.Background(), res, false, "produce the report")
	if !finished || cerr != nil {
		t.Fatalf("closeTurn after the read = (%v, %v), want delivered (true, nil)", finished, cerr)
	}
	if got := assistants(); len(got) != 1 || got[0] != "Here is the finished report." {
		t.Fatalf("a delivered answer must be durably recorded exactly once, got %v", got)
	}

	// A tool-calling turn is still recorded at deliberate (unchanged path).
	call := bareResult("", "tool_calls")
	call.Message.ToolCalls = []llm.ToolCall{{
		ID:       "call_0",
		Type:     "function",
		Function: llm.FunctionCall{Name: "fetch", Arguments: `{"url":"https://example.test"}`},
	}}
	a.deliberate(1, call, true)
	msgs, terr := pager.Transcript(conv, 0, 100)
	if terr != nil {
		t.Fatalf("pager.Transcript: %v", terr)
	}
	foundCall := false
	for _, m := range msgs {
		if m.Role == cortex.RoleToolCall && m.ToolName == "fetch" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatal("a tool-calling turn must still be durably recorded at deliberate")
	}
}
