// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package decide

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"matrix/cody/internal/llmtest"
)

// TestGenerateProducesNDistinctCandidates drives the REAL llm.Client over real
// SSE: Generate calls the hot client n times and returns one candidate per
// call, in order.
func TestGenerateProducesNDistinctCandidates(t *testing.T) {
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		return llmtest.Say(fmt.Sprintf("candidate-%d", step))
	})
	t.Cleanup(srv.Close)
	client := llmtest.NewClient(t, srv)

	cands, err := Generate(context.Background(), client, "system", "user", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 3 {
		t.Fatalf("got %d candidates, want 3", len(cands))
	}
	for i, c := range cands {
		if c.Index != i {
			t.Fatalf("candidate %d has index %d", i, c.Index)
		}
		if c.Text != fmt.Sprintf("candidate-%d", i) {
			t.Fatalf("candidate %d text = %q", i, c.Text)
		}
	}
}

// TestGenerateDropsEmptyAndErrorsWhenAllBlank proves a blank generation is not
// an option: empty candidates are dropped, and an all-blank run is an error.
func TestGenerateDropsEmptyAndErrorsWhenAllBlank(t *testing.T) {
	// One non-empty in the middle: kept, re-indexed to 0.
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		if step == 1 {
			return llmtest.Say("real")
		}
		return llmtest.Say("   ")
	})
	t.Cleanup(srv.Close)
	cands, err := Generate(context.Background(), llmtest.NewClient(t, srv), "s", "u", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Text != "real" || cands[0].Index != 0 {
		t.Fatalf("kept candidates = %+v, want one 'real' re-indexed to 0", cands)
	}

	blank := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn { return llmtest.Say("") })
	t.Cleanup(blank.Close)
	if _, err := Generate(context.Background(), llmtest.NewClient(t, blank), "s", "u", 2); err == nil {
		t.Fatal("all-blank generation should error, got nil")
	}
}

// TestJudgePicksNamedCandidate proves the cold judge's verdict is honored — the
// judge really runs and its pick is returned.
func TestJudgePicksNamedCandidate(t *testing.T) {
	var sawAdjudicator bool
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "decision adjudicator") {
			sawAdjudicator = true
		}
		return llmtest.Say(`{"pick": 2, "rationale": "best fit for the brief"}`)
	})
	t.Cleanup(srv.Close)
	cands := []Candidate{{0, "alpha"}, {1, "beta"}, {2, "gamma"}}
	dec, err := Judge(context.Background(), llmtest.NewClient(t, srv), "brief", "criteria", cands)
	if err != nil {
		t.Fatal(err)
	}
	if !sawAdjudicator {
		t.Fatal("judge did not run through the decision-adjudicator system prompt")
	}
	if dec.Pick != 2 || dec.Rationale == "" {
		t.Fatalf("decision = %+v, want pick 2 with a rationale", dec)
	}
}

// TestJudgeSingleCandidateShortCircuits proves a lone candidate is returned
// without an adjudicator call (the server errors the test if hit).
func TestJudgeSingleCandidateShortCircuits(t *testing.T) {
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		t.Error("judge called the model for a single candidate")
		return llmtest.Say("{}")
	})
	t.Cleanup(srv.Close)
	dec, err := Judge(context.Background(), llmtest.NewClient(t, srv), "b", "c", []Candidate{{0, "only"}})
	if err != nil || dec.Pick != 0 {
		t.Fatalf("single-candidate judge = %+v, %v", dec, err)
	}
}

// TestJudgeFailsSafeOnBadVerdict proves an out-of-range or unparseable verdict
// defaults to candidate 0 (fail-open) rather than wedging or panicking.
func TestJudgeFailsSafeOnBadVerdict(t *testing.T) {
	cands := []Candidate{{0, "a"}, {1, "b"}}
	for name, reply := range map[string]string{
		"out-of-range": `{"pick": 9}`,
		"unparseable":  "the second one is better, obviously",
		"negative":     `{"pick": -1}`,
	} {
		srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn { return llmtest.Say(reply) })
		dec, err := Judge(context.Background(), llmtest.NewClient(t, srv), "b", "c", cands)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: unexpected error %v", name, err)
		}
		if dec.Pick != 0 {
			t.Fatalf("%s: pick = %d, want fail-safe 0", name, dec.Pick)
		}
	}
}

// TestJudgeNoCandidatesErrors proves an empty candidate set is an error, not a
// silent zero.
func TestJudgeNoCandidatesErrors(t *testing.T) {
	if _, err := Judge(context.Background(), nil, "b", "c", nil); err == nil {
		t.Fatal("judging zero candidates should error")
	}
}
