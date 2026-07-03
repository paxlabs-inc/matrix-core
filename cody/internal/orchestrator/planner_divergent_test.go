// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"matrix/cody/internal/llmtest"
)

// candidatePlanJSON is a valid one-task plan whose goal encodes which
// generation produced it, so the test can prove which candidate the judge
// picked.
func candidatePlanJSON(n int) string {
	return fmt.Sprintf(`{"goal":"plan-%d","tasks":[
	  {"id":"t1","title":"seed","goal":"greet.txt exists","acceptance":["greet.txt exists"],
	   "wave":1,"verify":["true"],"deliverable":{"shape":"greet.txt"}}]}`, n)
}

// TestPlanFromModelDivergentJudgesCandidates proves plan-shape authoring runs
// as a decision phase (req 10.2): PlanFromModelDivergent generates N divergent
// candidates through the HOT client and the COLD judge's pick is the plan
// returned. Real llm.Client, real SSE — only the model's decisions are scripted.
func TestPlanFromModelDivergentJudgesCandidates(t *testing.T) {
	var genCalls, judgeCalls int32
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		switch {
		case strings.Contains(system, "decision adjudicator"):
			atomic.AddInt32(&judgeCalls, 1)
			// Pick the middle candidate — proves the judge's verdict, not the
			// first-valid fallback, decides.
			return llmtest.Say(`{"pick": 1, "rationale": "best decomposition"}`)
		case strings.Contains(system, "planner"):
			n := atomic.AddInt32(&genCalls, 1)
			return llmtest.Say(candidatePlanJSON(int(n) - 1))
		}
		t.Errorf("unexpected system prompt: %q", system)
		return llmtest.Say("{}")
	})
	t.Cleanup(srv.Close)
	client := llmtest.NewClient(t, srv)

	plan, err := PlanFromModelDivergent(context.Background(), client, client, "build a greeter", "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&genCalls); n != 3 {
		t.Fatalf("generated %d candidates, want 3 divergent", n)
	}
	if n := atomic.LoadInt32(&judgeCalls); n != 1 {
		t.Fatalf("judge ran %d times, want exactly once", n)
	}
	if plan.Goal != "plan-1" {
		t.Fatalf("returned plan goal = %q, want the judge's pick plan-1", plan.Goal)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("returned plan invalid: %v", err)
	}
}

// TestPlanFromModelDivergentSingleCandidate proves candidates<=1 falls back to
// the single-shot contract (no judge call).
func TestPlanFromModelDivergentSingleCandidate(t *testing.T) {
	var judgeCalls int32
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		if strings.Contains(system, "decision adjudicator") {
			atomic.AddInt32(&judgeCalls, 1)
			return llmtest.Say(`{"pick":0}`)
		}
		return llmtest.Say(candidatePlanJSON(0))
	})
	t.Cleanup(srv.Close)
	client := llmtest.NewClient(t, srv)

	plan, err := PlanFromModelDivergent(context.Background(), client, client, "req", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Goal != "plan-0" {
		t.Fatalf("plan goal = %q", plan.Goal)
	}
	if n := atomic.LoadInt32(&judgeCalls); n != 0 {
		t.Fatalf("judge ran %d times for a single candidate, want 0", n)
	}
}
