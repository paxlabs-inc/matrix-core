// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// bounded_gates_test.go pins the BOUNDED posture of the two gates that could
// previously block forever. Both share the same failure shape: a gate whose
// precondition the model cannot satisfy spends the whole unproductive budget
// and kills a turn over work that was never actually broken.
package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
)

// TestExpectRefusalIsBounded pins the ground-or-hypothesize gate (req.6.1) as
// TEACH-ONCE. On 2026-07-25 a research sub-agent issued web_search without the
// synthetic `expect` field, was refused, re-issued, was refused again, and died
// with "no productive progress after 4 unproductive attempts" — the tool itself
// was fine. The first call of a strategy is still refused with the corrected
// skeleton; the next one dispatches.
func TestExpectRefusalIsBounded(t *testing.T) {
	cfg := config.Default()
	cfg.EpistemicPredictions = true
	a := New(Options{Config: cfg})

	args := map[string]interface{}{"query": "AI agent frameworks landscape"}
	strategy, isProbe := probeStrategy("web-search__web_search", args)
	if !isProbe {
		t.Fatal("precondition: web_search must be probe-class for this gate to apply")
	}

	// Batch 1, first call: refused, and the refusal must TEACH (carry the
	// corrected call skeleton).
	batch1 := map[string]bool{}
	directive, refused := a.refuseUnstatedExpectation("web-search__web_search", args, "", batch1)
	if !refused {
		t.Fatal("the first unstated-expectation probe must be refused")
	}
	if !strings.Contains(directive, "expect") || !strings.Contains(directive, "query") {
		t.Fatalf("the refusal must hand back a corrected call skeleton, got %q", directive)
	}

	// Same BATCH, same strategy (a deduped duplicate): the verdict must stay
	// consistent, otherwise a call and its duplicate get different results.
	if _, refused := a.refuseUnstatedExpectation("web-search__web_search", args, "", batch1); !refused {
		t.Fatal("a sibling call of an already-refused strategy must share the refusal within one batch")
	}

	// NEXT batch (the model re-issued after reading the refusal, still without
	// `expect`): the gate stands down and the call dispatches.
	batch2 := map[string]bool{}
	if _, refused := a.refuseUnstatedExpectation("web-search__web_search", args, "", batch2); refused {
		t.Fatal("a strategy already taught the fix must dispatch, not be refused again")
	}

	// Re-wording the query is the SAME strategy — probeStrategy ignores the
	// varying argument — so it must not buy a fresh refusal.
	reworded := map[string]interface{}{"query": "agent framework market map 2026"}
	if s2, _ := probeStrategy("web-search__web_search", reworded); s2 != strategy {
		t.Fatalf("precondition: reworded query must map to the same strategy (%q vs %q)", s2, strategy)
	}
	if _, refused := a.refuseUnstatedExpectation("web-search__web_search", reworded, "", map[string]bool{}); refused {
		t.Fatal("re-wording the argument must not reset the refusal bound")
	}

	// A call that DOES state its expectation is never refused.
	if _, refused := a.refuseUnstatedExpectation("web-search__web_search", args, "a ranked list of source URLs", map[string]bool{}); refused {
		t.Fatal("a call carrying an expectation must never be refused")
	}
}

// TestExpectRefusalRespectsFreshStrategiesAndDisabledMechanism proves the bound
// is per-strategy (a genuinely new probe still gets its one teaching refusal)
// and that the gate is inert when Mechanism 3 is off.
func TestExpectRefusalRespectsFreshStrategiesAndDisabledMechanism(t *testing.T) {
	cfg := config.Default()
	cfg.EpistemicPredictions = true
	a := New(Options{Config: cfg})

	batch := map[string]bool{}
	searchArgs := map[string]interface{}{"query": "one"}
	if _, refused := a.refuseUnstatedExpectation("web-search__web_search", searchArgs, "", batch); !refused {
		t.Fatal("first probe of a strategy must be refused")
	}
	// A DIFFERENT strategy still earns its own single refusal, even in the same
	// batch — the bound is per strategy, not per batch.
	fetchArgs := map[string]interface{}{"url": "https://example.test/a"}
	if _, isProbe := probeStrategy("fetch__fetch", fetchArgs); !isProbe {
		t.Skip("fetch is not probe-class in this build; per-strategy bound covered by the search legs")
	}
	if _, refused := a.refuseUnstatedExpectation("fetch__fetch", fetchArgs, "", batch); !refused {
		t.Fatal("a fresh strategy must still get its one teaching refusal")
	}

	off := config.Default()
	off.EpistemicPredictions = false
	b := New(Options{Config: off})
	if _, refused := b.refuseUnstatedExpectation("web-search__web_search", searchArgs, "", map[string]bool{}); refused {
		t.Fatal("the gate must be inert when prediction-carrying dispatch is disabled")
	}
}

// TestSourceFetchGuardStandsDownAfterCap pins the close guard's bounded
// posture. When every fetch of a discovered URL fails, webEvidenceReady can
// never become true, so an unbounded guard withholds a FINISHED answer until
// the shared unproductive cap kills the turn — the 2026-07-22 loopty-loop
// shape, which was closed for the unread-overflow guard and left open here.
func TestSourceFetchGuardStandsDownAfterCap(t *testing.T) {
	a := chainAgent(t, nil, nil)
	// A search discovered sources; no fetch of them ever succeeded.
	a.noteWebEvidence("web-search__web_search", nil, `{"results":[{"url":"https://example.test/a"}]}`, false)
	if a.webEvidenceReady() {
		t.Fatal("precondition: discovered-but-unread sources must arm the guard")
	}
	const answer = "here is the finished analysis"

	for i := 0; i < sourceFetchNudgeCap; i++ {
		name, dec := a.evalCloseChain(&closeContext{res: bareResult(answer, "stop"), answer: answer})
		if name != "source_fetch" || dec.verdict != verdictNudge {
			t.Fatalf("attempt %d: fired %q/%q, want source_fetch/nudge", i+1, name, dec.verdict)
		}
		if dec.err != nil {
			t.Fatalf("attempt %d: unexpected escalation: %v", i+1, dec.err)
		}
	}

	// Every steer must be honest that the answer was withheld.
	var nudges []string
	for _, m := range a.working {
		if m.IsGuidance() {
			nudges = append(nudges, m.Content)
		}
	}
	if len(nudges) != sourceFetchNudgeCap {
		t.Fatalf("got %d guidance nudges, want %d", len(nudges), sourceFetchNudgeCap)
	}
	for _, n := range nudges {
		if !strings.Contains(n, "NOT delivered") {
			t.Fatalf("the source-fetch steer must state non-delivery, got %q", n)
		}
	}

	// Past the cap the guard stands down even though evidence is STILL not
	// ready — the finished answer ships instead of the turn dying.
	if a.webEvidenceReady() {
		t.Fatal("precondition: the sources must still be unread")
	}
	name, dec := a.evalCloseChain(&closeContext{res: bareResult(answer, "stop"), answer: answer})
	if name != "deliver" || dec.verdict != verdictDeliver {
		t.Fatalf("past the cap: fired %q/%q, want deliver/deliver", name, dec.verdict)
	}
}

// TestSourceFetchGuardStillFiresWhenEvidenceIsAchievable proves the bound did
// not defeat the discipline: a successful fetch of a discovered URL satisfies
// the guard outright, and the guard is silent when no search ran at all.
func TestSourceFetchGuardStillFiresWhenEvidenceIsAchievable(t *testing.T) {
	a := chainAgent(t, nil, nil)
	a.noteWebEvidence("web-search__web_search", nil, `{"results":[{"url":"https://example.test/a"}]}`, false)
	a.noteWebEvidence("fetch__fetch", map[string]interface{}{"url": "https://example.test/a"}, "page contents", false)
	if !a.webEvidenceReady() {
		t.Fatal("a fetched discovered URL must satisfy the evidence gate")
	}
	const answer = "grounded answer"
	name, dec := a.evalCloseChain(&closeContext{res: bareResult(answer, "stop"), answer: answer})
	if name != "deliver" || dec.verdict != verdictDeliver {
		t.Fatalf("with evidence read: fired %q/%q, want deliver/deliver", name, dec.verdict)
	}

	// A FAILED fetch is not evidence — the guard must still fire the first time.
	b := chainAgent(t, nil, nil)
	b.noteWebEvidence("web-search__web_search", nil, `{"results":[{"url":"https://example.test/b"}]}`, false)
	b.noteWebEvidence("fetch__fetch", map[string]interface{}{"url": "https://example.test/b"}, "403 forbidden", true)
	if b.webEvidenceReady() {
		t.Fatal("a FAILED fetch must not count as evidence")
	}
	name, _ = b.evalCloseChain(&closeContext{res: bareResult(answer, "stop"), answer: answer})
	if name != "source_fetch" {
		t.Fatalf("a failed fetch must still arm the guard once, fired %q", name)
	}
}
