// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"math/rand"
	"reflect"
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
)

// This file proves PROPERTY 3 (task 6.3): the three guardrails hold for EVERY
// modification, over a randomized battery of trigger-inducing runs against the
// REAL controller and REAL llm.Message / llm.ToolCall values (no fakes):
//
//   - CONTENT-ONLY (guardrail 1): only an assistant-role Content ever changes;
//     ToolCalls / Role / Name of the edited message, and every tool / user /
//     system message in the transcript, are byte-identical.
//   - DUAL-RECORD (guardrail 3): every fired mod appends exactly one record whose
//     Original is the pre-edit content and whose Mod is non-empty.
//   - BUDGET (req.4.1/4.2/4.4): no mod before min_step, at most one mod per step,
//     and at most max_mods_per_turn per turn.

var casToolPool = []string{
	"fs__read_file", "web__search", "grep_files", "status_check", // verify-kind
	"fs__write_file", "core__deploy", "send_tx", "edit_file", // mutate-kind
	"spawn_subagents", "render_construct", "do_thing", // other-kind
}

// randToolCalls builds a random 1–2 tool batch from the pool.
func randToolCalls(r *rand.Rand) []llm.ToolCall {
	n := 1 + r.Intn(2)
	out := make([]llm.ToolCall, 0, n)
	for i := 0; i < n; i++ {
		name := casToolPool[r.Intn(len(casToolPool))]
		out = append(out, tc(name, `{"k":"v"}`))
	}
	return out
}

// randTranscript builds a random well-formed transcript that ALWAYS ends on an
// assistant message (the controller's edit target is a.working[len-1]). Interior
// messages span every role so the content-only guardrail is exercised against
// user / tool / system / assistant neighbours.
func randTranscript(r *rand.Rand) []llm.Message {
	msgs := []llm.Message{llm.UserMessage("do the task")}
	interior := r.Intn(4)
	for i := 0; i < interior; i++ {
		switch r.Intn(4) {
		case 0:
			msgs = append(msgs, llm.UserMessage("more context"))
		case 1:
			msgs = append(msgs, llm.SystemMessage("a system note"))
		case 2:
			msgs = append(msgs, llm.ToolResult("call", "some_tool", "a tool result"))
		default:
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "earlier thought", ToolCalls: randToolCalls(r)})
		}
	}
	// The tail assistant turn: randomly a bare-answer, a preamble+tools, or
	// straight-to-tools.
	tail := llm.Message{Role: llm.RoleAssistant}
	switch r.Intn(3) {
	case 0:
		tail.Content = "I think that's the final answer."
	case 1:
		tail.Content = "Let me check once more."
		tail.ToolCalls = randToolCalls(r)
	default:
		tail.ToolCalls = randToolCalls(r)
	}
	return append(msgs, tail)
}

// randSignals builds a random signal bundle. Half the time it forces a strong
// loop so mods actually fire across the battery (a battery that never triggers
// would prove nothing).
func randSignals(r *rand.Rand, step int, calls []llm.ToolCall, loopThreshold int) cassandraSignals {
	sig := cassandraSignals{
		step:           step,
		closing:        r.Intn(2) == 0,
		calls:          calls,
		cyclic:         r.Intn(2) == 0,
		semanticRepeat: r.Intn(2) == 0,
		workDone:       r.Intn(2) == 0,
	}
	if r.Intn(2) == 0 {
		sig.effectiveRepeats = loopThreshold + r.Intn(2) // arm the loop side
	} else {
		sig.effectiveRepeats = r.Intn(loopThreshold + 1)
	}
	return sig
}

func TestCassandraGuardrailBattery(t *testing.T) {
	r := rand.New(rand.NewSource(0xCA55A2D)) // deterministic battery
	fired := 0
	for turn := 0; turn < 400; turn++ {
		a := New(Options{Config: config.Default()})
		a.turn.casModsThisTurn = 0
		a.turn.casCooldown = map[modTrigger]int{}
		a.turn.casRecord = nil
		lt := a.casLoopThreshold()

		steps := 1 + r.Intn(6)
		startStep := r.Intn(4) // sometimes below min_step, to exercise the gate
		modsThisTurn := 0
		for s := 0; s < steps; s++ {
			step := startStep + s
			a.working = randTranscript(r)
			tail := a.working[len(a.working)-1]
			sig := randSignals(r, step, tail.ToolCalls, lt)

			snapshot := make([]llm.Message, len(a.working))
			copy(snapshot, a.working)
			recordsBefore := len(a.turn.casRecord)

			did := a.cassandraStep(sig)

			// BUDGET: no mod before min_step.
			if did && step < a.casMinStep() {
				t.Fatalf("turn %d step %d: a mod fired before min_step (%d)", turn, step, a.casMinStep())
			}
			// BUDGET: at most one mod (one dual-record) per step.
			grew := len(a.turn.casRecord) - recordsBefore
			if grew < 0 || grew > 1 {
				t.Fatalf("turn %d step %d: a step must record 0 or 1 mods, recorded %d", turn, step, grew)
			}
			if did != (grew == 1) {
				t.Fatalf("turn %d step %d: cassandraStep return (%v) must match a recorded mod (%d)", turn, step, did, grew)
			}

			if did {
				fired++
				modsThisTurn++
				last := len(a.working) - 1
				// CONTENT-ONLY: every message except the tail is byte-identical.
				for i := 0; i < last; i++ {
					if !reflect.DeepEqual(snapshot[i], a.working[i]) {
						t.Fatalf("turn %d step %d: guardrail 1 violated — non-tail message %d (role %s) changed", turn, step, i, a.working[i].Role)
					}
				}
				// CONTENT-ONLY: the edited tail is assistant, and only its Content
				// moved — Role / Name / ToolCalls are byte-identical.
				got := a.working[last]
				if got.Role != llm.RoleAssistant {
					t.Fatalf("turn %d step %d: guardrail 1 violated — edited a %s message", turn, step, got.Role)
				}
				if got.Name != snapshot[last].Name || !reflect.DeepEqual(got.ToolCalls, snapshot[last].ToolCalls) {
					t.Fatalf("turn %d step %d: guardrail 1 violated — Name/ToolCalls changed", turn, step)
				}
				// The original content must be preserved as a prefix (additive fold).
				if orig := snapshot[last].Content; orig != "" {
					if len(got.Content) <= len(orig) || got.Content[:len(orig)] != orig {
						t.Fatalf("turn %d step %d: original content not preserved as prefix\norig=%q\ngot=%q", turn, step, orig, got.Content)
					}
				}
				// DUAL-RECORD: the new record matches the edit.
				rec := a.turn.casRecord[len(a.turn.casRecord)-1]
				if rec.Target != last || rec.Original != snapshot[last].Content || rec.Mod == "" {
					t.Fatalf("turn %d step %d: dual-record mismatch: %+v (want target=%d original=%q)", turn, step, rec, last, snapshot[last].Content)
				}
			}
		}
		// BUDGET: never more than max_mods_per_turn in a single turn.
		if modsThisTurn > a.casMaxMods() {
			t.Fatalf("turn %d: %d mods exceeds max_mods_per_turn (%d)", turn, modsThisTurn, a.casMaxMods())
		}
		if len(a.turn.casRecord) != modsThisTurn {
			t.Fatalf("turn %d: dual-record count %d != mods fired %d", turn, len(a.turn.casRecord), modsThisTurn)
		}
	}
	// The battery must have actually exercised the fire path, or it proves nothing.
	if fired == 0 {
		t.Fatal("the guardrail battery never triggered a single mod — it proves nothing")
	}
	t.Logf("guardrail battery: %d mods fired across 400 turns, all three guardrails held", fired)
}
