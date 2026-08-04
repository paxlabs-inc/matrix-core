// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
)

// TestSignalParity_UnifiedReadMatchesCommit proves MORPHEUS req.5.1 signal
// parity on the real state machine: for scripted multi-step batch sequences
// covering every repeat mode (exact, semantic reword, rotating cycle) plus
// genuine breadth, the unified per-step state computed ONCE (computeStepSignals)
// yields exactly the verdicts the stall commit (noteBatch) produces — the same
// repeat flag, the same committed repeat count (effectiveRepeats is the
// one-step-early read of it), the same stall bound — and the Cassandra view is
// a pure projection of the same numbers. The loop's ordering is mirrored
// faithfully: signals are read BEFORE the commit, and the distinct-tool set
// advances after (as act does).
func TestSignalParity_UnifiedReadMatchesCommit(t *testing.T) {
	cfg := config.Default()
	bound := cfg.NoProgressStall
	if bound < 2 {
		t.Fatalf("precondition: NoProgressStall must be >= 2, got %d", bound)
	}

	sameCall := func() []llm.ToolCall { return []llm.ToolCall{tc("web_search", `{"query":"the same query"}`)} }
	reworded := []string{
		`{"query":"check wallet balance status"}`,
		`{"query":"balance status wallet check"}`,
		`{"query":"status check balance wallet"}`,
		`{"query":"wallet status check balance"}`,
		`{"query":"balance wallet status check"}`,
		`{"query":"check status wallet balance"}`,
		`{"query":"wallet balance check status"}`,
	}

	type scenario struct {
		name      string
		batches   [][]llm.ToolCall
		wantStall bool
	}
	var scenarios []scenario

	// Exact spiral: the same byte-identical call until past the bound.
	var exact [][]llm.ToolCall
	for i := 0; i <= bound+1; i++ {
		exact = append(exact, sameCall())
	}
	scenarios = append(scenarios, scenario{"exact_repeat_spiral", exact, true})

	// Semantic reword: same operation, shuffled wording every step.
	var semantic [][]llm.ToolCall
	for i := 0; i <= bound+1 && i < len(reworded); i++ {
		semantic = append(semantic, []llm.ToolCall{tc("web_search", reworded[i])})
	}
	scenarios = append(scenarios, scenario{"semantic_reword_spiral", semantic, true})

	// Rotating cycle: A → B → A → B … introduces no new tool after step 2.
	a1 := func() []llm.ToolCall { return []llm.ToolCall{tc("read_file", `{"path":"alpha.txt"}`)} }
	b1 := func() []llm.ToolCall { return []llm.ToolCall{tc("run_check", `{"target":"alpha.txt"}`)} }
	var cyclic [][]llm.ToolCall
	for i := 0; i <= 2*(bound+2); i++ {
		if i%2 == 0 {
			cyclic = append(cyclic, a1())
		} else {
			cyclic = append(cyclic, b1())
		}
	}
	scenarios = append(scenarios, scenario{"rotating_cycle", cyclic, true})

	// Genuine breadth: a different tool every step — never a repeat.
	var breadth [][]llm.ToolCall
	for i := 0; i <= bound+3; i++ {
		breadth = append(breadth, []llm.ToolCall{tc("tool_"+string(rune('a'+i)), `{"n":1}`)})
	}
	scenarios = append(scenarios, scenario{"genuine_breadth", breadth, false})

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			a := New(Options{Config: cfg})
			stalledSeen := false
			for step, batch := range sc.batches {
				msg := llm.Message{Role: llm.RoleAssistant, ToolCalls: batch}
				s := a.computeStepSignals(step, msg)
				a.turn.signals = s
				preRepeats := a.turn.repeats

				repeat, stalled := a.noteBatch(batch)

				if repeat != s.repeat {
					t.Fatalf("step %d: commit repeat=%v, unified read repeat=%v", step, repeat, s.repeat)
				}
				wantEff := preRepeats
				if s.repeat {
					wantEff = preRepeats + 1
				}
				if s.effectiveRepeats != wantEff {
					t.Fatalf("step %d: effectiveRepeats=%d, want %d (one-step-early read of the commit)", step, s.effectiveRepeats, wantEff)
				}
				wantCommitted := 0
				if s.repeat {
					wantCommitted = s.effectiveRepeats
				}
				if a.turn.repeats != wantCommitted {
					t.Fatalf("step %d: committed repeats=%d, want %d", step, a.turn.repeats, wantCommitted)
				}
				if want := s.repeat && s.effectiveRepeats >= bound; stalled != want {
					t.Fatalf("step %d: stalled=%v, unified read implies %v", step, stalled, want)
				}
				cv := s.cassandraView()
				if cv.effectiveRepeats != s.effectiveRepeats || cv.cyclic != s.cyclic || cv.semanticRepeat != s.semantic ||
					cv.closing != s.closing || cv.workDone != s.workDone {
					t.Fatalf("step %d: cassandraView diverged from the unified state", step)
				}
				// toolBreadth is the pre-batch distinct count; the set has not
				// been advanced yet at read time.
				if s.toolBreadth != len(a.turn.distinctToolSet) {
					t.Fatalf("step %d: toolBreadth=%d, want pre-batch distinct count %d", step, s.toolBreadth, len(a.turn.distinctToolSet))
				}

				// Mirror act: the distinct-tool set advances AFTER the commit.
				for _, c := range batch {
					a.turn.distinctToolSet[c.Function.Name] = struct{}{}
				}
				if stalled {
					stalledSeen = true
					break
				}
			}
			if stalledSeen != sc.wantStall {
				t.Fatalf("scenario %q: stalled=%v, want %v", sc.name, stalledSeen, sc.wantStall)
			}
		})
	}
}

// TestSignalState_CarriesEpistemicReads proves the unified state is
// complete per req.5.1: the ledger reads (refuted / ungrounded-self), the
// epistemic meters (per-strategy mismatches, evidence delta + actions-since-
// growth) and the closing flag all ride the one
// state — read from the turn's REAL source structures, not recomputed copies.
func TestSignalState_CarriesEpistemicAndCounterReads(t *testing.T) {
	a := New(Options{Config: config.Default()})

	// Real ledger: one refuted plan premise, one ungrounded self-assumption.
	a.turn.ledger = &premiseLedger{}
	a.turn.ledger.add(Premise{Statement: "the daemon exposes an OpenAI-compatible API", Status: premiseRefuted})
	a.turn.ledger.add(Premise{Statement: "I can read my own config", Status: premiseAssumption, SelfRef: true})

	// Real meters: a mismatch streak and a non-growing graph.
	a.turn.mismatchMeter = map[string]int{"fetch:api": 2}
	a.turn.graph = &taskGraph{evidenceSet: map[string]struct{}{"k1": {}, "k2": {}}, actionsSinceGrowth: 3}

	s := a.computeStepSignals(7, llm.Message{Role: llm.RoleAssistant, Content: "done"})
	if !s.closing {
		t.Error("a bare-answer step must read as closing")
	}
	if s.refutedPremises != 1 {
		t.Errorf("refutedPremises = %d, want 1", s.refutedPremises)
	}
	if s.ungroundedSelfPremises != 1 {
		t.Errorf("ungroundedSelfPremises = %d, want 1", s.ungroundedSelfPremises)
	}
	if s.mismatch["fetch:api"] != 2 {
		t.Errorf("mismatch read = %d, want 2", s.mismatch["fetch:api"])
	}
	if s.evidenceItems != 2 || s.actionsSinceGrowth != 3 {
		t.Errorf("graph reads = (%d evidence, %d since growth), want (2, 3)", s.evidenceItems, s.actionsSinceGrowth)
	}
	if s.step != 7 {
		t.Errorf("step = %d, want 7", s.step)
	}
}

// productionCallSites returns the file:line of every non-comment, non-test
// occurrence of pattern in the package's production sources, skipping the
// line that DEFINES the named function (a leading "func ").
func productionCallSites(t *testing.T, pattern string) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var sites []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func ") {
				continue
			}
			if strings.Contains(trimmed, pattern) {
				sites = append(sites, fmt.Sprintf("%s:%d", name, i+1))
			}
		}
	}
	return sites
}

// TestSignalRead_SingleComputationSite pins the structural half of req.5.1:
// the repeat verdict (the exact/semantic/cyclic composition) is computed in
// exactly ONE place — readStepSignals. noteBatch and buildCassandraSignals
// must consume it, never recompute it: a second `sigInWindow` caller in
// production would be duplicate bookkeeping reintroduced.
func TestSignalRead_SingleComputationSite(t *testing.T) {
	sites := productionCallSites(t, "sigInWindow(")
	if len(sites) != 1 {
		t.Fatalf("sigInWindow must have exactly ONE production call site (readStepSignals), found %v", sites)
	}
	semSites := productionCallSites(t, "a.semanticRepeat(")
	if len(semSites) != 1 {
		t.Fatalf("semanticRepeat must have exactly ONE production call site (readStepSignals), found %v", semSites)
	}
}
