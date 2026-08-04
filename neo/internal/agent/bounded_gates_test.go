// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// bounded_gates_test.go pins the bounded close guards that remain after
// execution-time heuristic refusal gates were retired.
package agent

import (
	"strings"
	"testing"
)

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
