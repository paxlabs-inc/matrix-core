// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// The approve-path settle seam: an approved opportunity used to be parked at
// "scheduled" forever — the normal run it dispatched never reported back, so a
// clearly finished task never reached OpportunityDone and never appeared in the
// completion inbox (the empty "Done" tab). settleApprovedOpportunity is the
// run-terminal observer that closes that loop. These tests drive it against the
// REAL Neocortex pager, the REAL durable inbox, and the REAL conversation store —
// no fakes.

import (
	"context"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/task"
)

func newApproveSettleEngine(t *testing.T) (*Engine, *memory.Pager) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-approve-settle-test"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Pager:           pager,
		ConversationDir: t.TempDir(),
		AutomatrixDir:   t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})
	return e, pager
}

// seedScheduledOpportunity captures a pending opportunity and moves it to
// scheduled — exactly the state handleAutomatrixApprove leaves it in when it
// dispatches the gated run.
func seedScheduledOpportunity(t *testing.T, e *Engine, pager *memory.Pager, summary string) memory.OpportunitySpec {
	t.Helper()
	ctx := context.Background()
	uri, err := pager.RememberOpportunity(ctx, memory.OpportunitySpec{
		Summary:              summary,
		Rationale:            "the user asked for this as follow-up work",
		EligibleAutonomous:   false,
		Confidence:           0.9,
		OriginConversationID: "conv-approve-settle",
	})
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	if err := pager.SetOpportunityStatus(ctx, uri, memory.OpportunityScheduled); err != nil {
		t.Fatalf("SetOpportunityStatus(scheduled): %v", err)
	}
	opp, ok, err := pager.OpportunityByURI(ctx, uri)
	if err != nil || !ok {
		t.Fatalf("seeded opportunity %s not found: %v", uri, err)
	}
	return opp
}

// statusOf reads an opportunity's live status back from Neocortex.
func statusOf(t *testing.T, pager *memory.Pager, uri string) memory.OpportunityStatus {
	t.Helper()
	opp, ok, err := pager.OpportunityByURI(context.Background(), uri)
	if err != nil || !ok {
		t.Fatalf("OpportunityByURI(%s): ok=%v err=%v", uri, ok, err)
	}
	return opp.Status
}

// TestSettleApprovedOpportunityDone proves a genuine completion marks the
// opportunity done AND writes the durable completion record with the run's
// final answer — so the finished task shows up in the Done inbox.
func TestSettleApprovedOpportunityDone(t *testing.T) {
	e, pager := newApproveSettleEngine(t)
	opp := seedScheduledOpportunity(t, e, pager, "Map the tier feature-gating strategy to a billing smart contract design")

	// The run's persisted final answer (what the approve run said last).
	e.conv.AppendAssistant("conv-approve-settle", "run-1", "Here is the full billing contract design.\n\nDetails follow.")

	e.settleApprovedOpportunity(opp, "conv-approve-settle", task.StatusDone)

	if got := statusOf(t, pager, opp.URI); got != memory.OpportunityDone {
		t.Fatalf("status = %s, want done", got)
	}
	items := e.AutomatrixInbox().List()
	if len(items) != 1 {
		t.Fatalf("inbox items = %d, want 1", len(items))
	}
	rec := items[0]
	if rec.OpportunitySummary != opp.Summary {
		t.Errorf("record summary = %q, want %q", rec.OpportunitySummary, opp.Summary)
	}
	if rec.ResultSummary != "Here is the full billing contract design." {
		t.Errorf("record result = %q, want the run's final answer lede", rec.ResultSummary)
	}
	if rec.ConversationID != "conv-approve-settle" {
		t.Errorf("record conversation = %q", rec.ConversationID)
	}
}

// TestSettleApprovedOpportunityInterrupted proves a deliberate user stop
// returns the item to the pending queue with no attempts penalty and no
// completion record.
func TestSettleApprovedOpportunityInterrupted(t *testing.T) {
	e, pager := newApproveSettleEngine(t)
	opp := seedScheduledOpportunity(t, e, pager, "Draft the LayerX staking role documentation")

	e.settleApprovedOpportunity(opp, "conv-approve-settle", task.StatusInterrupted)

	if got := statusOf(t, pager, opp.URI); got != memory.OpportunityPending {
		t.Fatalf("status = %s, want pending", got)
	}
	if n := len(e.AutomatrixInbox().List()); n != 0 {
		t.Fatalf("inbox items = %d, want 0 (an interrupted run never announces)", n)
	}
	fresh, ok, err := pager.OpportunityByURI(context.Background(), opp.URI)
	if err != nil || !ok {
		t.Fatalf("OpportunityByURI: ok=%v err=%v", ok, err)
	}
	if fresh.Attempts != opp.Attempts {
		t.Errorf("attempts = %d, want unchanged %d (deliberate stop is not a failure)", fresh.Attempts, opp.Attempts)
	}
}

// TestSettleApprovedOpportunityFailedRePends proves a non-done, non-interrupted
// terminal re-pends the item with attempts++ (bounded by the shared ceiling)
// and writes no completion record.
func TestSettleApprovedOpportunityFailedRePends(t *testing.T) {
	e, pager := newApproveSettleEngine(t)
	opp := seedScheduledOpportunity(t, e, pager, "Outline the validator onboarding checklist")

	e.settleApprovedOpportunity(opp, "conv-approve-settle", task.StatusCeiling)

	if got := statusOf(t, pager, opp.URI); got != memory.OpportunityPending {
		t.Fatalf("status = %s, want pending (failed attempt re-queues)", got)
	}
	if n := len(e.AutomatrixInbox().List()); n != 0 {
		t.Fatalf("inbox items = %d, want 0 (a failed run never announces)", n)
	}
	fresh, ok, err := pager.OpportunityByURI(context.Background(), opp.URI)
	if err != nil || !ok {
		t.Fatalf("OpportunityByURI: ok=%v err=%v", ok, err)
	}
	if fresh.Attempts != opp.Attempts+1 {
		t.Errorf("attempts = %d, want %d", fresh.Attempts, opp.Attempts+1)
	}
}
