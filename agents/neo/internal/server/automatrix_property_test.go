// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Property 3 — wakes are idle-only, non-competing, opt-in-gated, bounded.
//
// Task 7.3 asks for the PROPERTY-level proof tying the wake clauses together as
// one coherent property, on top of the per-clause cases in
// automatrix_wake_test.go (which this file does NOT duplicate). Property 3 is a
// conjunction of four guarantees on the wake handler; this file proves them as
// universally-quantified invariants:
//
//	opt-in-gated  — a wake never runs proactive work unless the per-user opt-in
//	                is ON (and an opted-out wake CANCELS the alarm, never runs);
//	non-competing — a wake never runs while a real run is in flight (busy-defer);
//	bounded       — a wake never runs once the per-day cap is reached;
//	idle-only     — the ONLY thing a wake ever dispatches is a pending,
//	                eligible-autonomous opportunity; otherwise it idles/defers,
//	                and EVERY non-opt-out path reschedules the next jittered fire.
//
// Two complementary proofs, both over REAL code: (1) a seeded randomized sweep
// over the REAL pure decideAutomatrixWake across the whole input space, and (2)
// a sequential end-to-end walk of ONE real Engine (real Neocortex pager under
// t.TempDir(), faithful in-package governor seam, recording runner) through the
// full state machine, asserting the cumulative invariant after every transition.

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"centra/agents/neo/internal/agent"
	"centra/agents/neo/internal/memory"
)

// TestProperty3_Decider_InvariantSweep drives the REAL decideAutomatrixWake over
// a large seeded sweep of the input space and asserts the four Property-3
// guarantees hold for EVERY drawn input — not just the hand-picked fixtures the
// per-clause tests use. The randomized dimensions are the exact gates the
// policy consumes: opt-in, busy, today's count vs the cap, the skip
// probability, the jitter window, and a randomly-shaped pending queue (mixing
// eligible/ineligible and pending/non-pending items).
//
// Validates: Requirements 4.4 (opt-in re-check), 4.5 (busy-defer),
// 4.6 (idle when no eligible-pending), 4.7 (per-day cap).
func TestProperty3_Decider_InvariantSweep(t *testing.T) {
	rng := rand.New(rand.NewSource(0xA17031A))
	const iterations = 20000

	summaries := []string{
		"Draft the quarterly update doc you mentioned",
		"Summarize the three articles you saved",
		"Outline the talk you said you owe",
		"Swap 1 ETH for USDC", // financial-shaped; only ever appears as ineligible
	}

	for i := 0; i < iterations; i++ {
		in := automatrixWakeInput{
			enabled:    rng.Intn(2) == 0,
			busy:       rng.Intn(2) == 0,
			tasksToday: rng.Intn(6),
			maxPerDay:  rng.Intn(5), // 0 means uncapped
			base:       time.Duration(rng.Intn(120)) * time.Minute,
			jitter:     time.Duration(rng.Intn(60)) * time.Minute,
			skipProb:   []float64{0, 0.2, 0.5, 1}[rng.Intn(4)],
		}
		// Build a randomly-shaped queue: 0..4 items, each independently
		// pending/non-pending and eligible/ineligible.
		nq := rng.Intn(5)
		var hasEligiblePending bool
		for j := 0; j < nq; j++ {
			status := memory.OpportunityPending
			if rng.Intn(2) == 0 {
				status = []memory.OpportunityStatus{
					memory.OpportunityScheduled, memory.OpportunityInProgress,
					memory.OpportunityDone, memory.OpportunityDismissed,
				}[rng.Intn(4)]
			}
			eligible := rng.Intn(2) == 0
			in.pending = append(in.pending, memory.OpportunitySpec{
				Summary:            summaries[rng.Intn(len(summaries))],
				Status:             status,
				EligibleAutonomous: eligible,
				Confidence:         rng.Float32(),
			})
			if status == memory.OpportunityPending && eligible {
				hasEligiblePending = true
			}
		}

		dec := decideAutomatrixWake(in, rng)

		// --- opt-in-gated: opt-out is absolute (req 4.4) ---
		if !in.enabled {
			if dec.outcome != automatrixOptedOut {
				t.Fatalf("iter %d: opted-out wake outcome = %v, want automatrixOptedOut", i, dec.outcome)
			}
			// An opted-out decision never carries a run.
			continue
		}

		// --- a run is the ONLY work-dispatching outcome; constrain it hard ---
		if dec.outcome == automatrixRun {
			// non-competing (req 4.5)
			if in.busy {
				t.Fatalf("iter %d: ran while busy — Property 3 non-competing violated", i)
			}
			// bounded (req 4.7)
			if in.maxPerDay > 0 && in.tasksToday >= in.maxPerDay {
				t.Fatalf("iter %d: ran at/over the per-day cap (%d/%d) — bounded violated", i, in.tasksToday, in.maxPerDay)
			}
			// idle-only / safe pick (req 4.6 + req 3.3 defense in depth): the
			// pick MUST be a pending, eligible-autonomous opportunity.
			if dec.pick.Status != memory.OpportunityPending || !dec.pick.EligibleAutonomous {
				t.Fatalf("iter %d: ran a non-eligible/non-pending pick %+v", i, dec.pick)
			}
			if !hasEligiblePending {
				t.Fatalf("iter %d: ran with no eligible-pending item in the queue", i)
			}
		}

		// --- idle resolves only when there is genuinely nothing to pick ---
		if dec.outcome == automatrixIdle && hasEligiblePending {
			t.Fatalf("iter %d: idled despite an eligible-pending opportunity", i)
		}

		// --- busy/cap, when they are the binding constraint, must defer ---
		if in.busy && dec.outcome == automatrixRun {
			t.Fatalf("iter %d: busy wake must never run", i)
		}

		// --- every non-opt-out path reschedules a positive, floored,
		//     in-window next fire (req 4.3 timing backbone) ---
		if dec.next < automatrixMinInterval {
			t.Fatalf("iter %d: next-fire %v below floor %v", i, dec.next, automatrixMinInterval)
		}
		lo := in.base - in.jitter
		if lo < automatrixMinInterval {
			lo = automatrixMinInterval
		}
		hi := in.base + in.jitter
		if hi < automatrixMinInterval {
			hi = automatrixMinInterval
		}
		if dec.next < lo || dec.next > hi {
			t.Fatalf("iter %d: next-fire %v outside floored jitter window [%v,%v]", i, dec.next, lo, hi)
		}
	}
}

// TestProperty3_Handler_EndToEndChain walks ONE real Engine through the full
// Property-3 state machine in sequence — opted-out → busy → capped → idle →
// runnable — and asserts the cumulative invariant after each transition,
// against the REAL handler, a REAL Neocortex pager, a faithful in-package governor
// seam, and a recording runner. This is the single coherent end-to-end proof
// that the clauses compose: at no point before the fully-permitted state does a
// proactive run occur, and the opt-out path is the only one that cancels rather
// than reschedules.
//
// Validates: Requirements 4.4, 4.5, 4.6, 4.7.
func TestProperty3_Handler_EndToEndChain(t *testing.T) {
	ctx := context.Background()
	e, pager := newWakeTestEngine(t)
	e.automatrixSkipProb = 0 // isolate the gating clauses from the probabilistic skip

	const summary = "Draft the quarterly update doc you mentioned"
	if _, err := pager.RememberOpportunity(ctx, memory.OpportunitySpec{
		Summary: summary, Rationale: "user said they owe a quarterly update",
		EligibleAutonomous: true, Confidence: 0.9, OriginConversationID: "conv-origin",
	}); err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}

	gov := &stateGovernor{}
	e.SetAutomatrixGovernor(gov)
	var runs []memory.OpportunitySpec
	e.SetAutomatrixRunner(func(_ context.Context, opp memory.OpportunitySpec) error {
		runs = append(runs, opp)
		return nil
	})

	wake := func() {
		if !e.MaybeHandleAutomatrixWake(ctx, "c", agent.AutomatrixWakeMessage) {
			t.Fatal("the canonical AUTOMATRIX wake must be recognized")
		}
	}

	// Stage 1 — OPTED OUT: opt-in-gated. Cancels, never runs, never reschedules.
	gov.enabled = false
	wake()
	if len(runs) != 0 {
		t.Fatalf("stage opted-out: a proactive run started while opted out (%d)", len(runs))
	}
	if gov.cancelCalls != 1 {
		t.Fatalf("stage opted-out: want exactly 1 alarm cancel, got %d", gov.cancelCalls)
	}
	if len(gov.reschedules) != 0 {
		t.Fatalf("stage opted-out: must not reschedule (alarm cancelled), got %d", len(gov.reschedules))
	}

	// Stage 2 — ENABLED BUT BUSY: non-competing. Defers (reschedules), no run.
	gov.enabled = true
	e.registerRun(&run{id: "live-user-run", convID: "other"})
	wake()
	if len(runs) != 0 {
		t.Fatalf("stage busy: ran while a user run was in flight (%d)", len(runs))
	}
	if len(gov.reschedules) != 1 {
		t.Fatalf("stage busy: want 1 reschedule, got %d", len(gov.reschedules))
	}
	if gov.cancelCalls != 1 {
		t.Fatalf("stage busy: must not cancel, cancels=%d", gov.cancelCalls)
	}
	e.unregisterRun("live-user-run")

	// Stage 3 — IDLE & AT CAP: bounded. Defers (reschedules), no run.
	gov.tasksToday = e.cfg.AutomatrixMaxTasksPerDay
	wake()
	if len(runs) != 0 {
		t.Fatalf("stage capped: ran at the per-day cap (%d)", len(runs))
	}
	if len(gov.reschedules) != 2 {
		t.Fatalf("stage capped: want 2 reschedules total, got %d", len(gov.reschedules))
	}
	if gov.startedCalls != 0 {
		t.Fatalf("stage capped: counter advanced without a run (%d)", gov.startedCalls)
	}

	// Stage 4 — IDLE & UNDER CAP BUT EMPTY QUEUE: idle-only. Reschedules, no run.
	gov.tasksToday = 0
	// Dismiss the only opportunity so the eligible-pending queue is empty.
	pend, err := pager.PendingOpportunities(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(pend) != 1 {
		t.Fatalf("expected exactly 1 eligible-pending opportunity before dismiss, got %d", len(pend))
	}
	if err := pager.SetOpportunityStatus(ctx, pend[0].URI, memory.OpportunityDismissed); err != nil {
		t.Fatalf("SetOpportunityStatus dismissed: %v", err)
	}
	wake()
	if len(runs) != 0 {
		t.Fatalf("stage empty-queue: ran with no eligible opportunity (%d)", len(runs))
	}
	if len(gov.reschedules) != 3 {
		t.Fatalf("stage empty-queue: want 3 reschedules total, got %d", len(gov.reschedules))
	}
	if gov.startedCalls != 0 {
		t.Fatalf("stage empty-queue: counter advanced without a run (%d)", gov.startedCalls)
	}

	// Stage 5 — FULLY PERMITTED: enabled, idle, under cap, eligible item present.
	// This is the ONLY state that dispatches, and it dispatches exactly the
	// pending eligible opportunity, advances the counter, and reschedules.
	if _, err := pager.RememberOpportunity(ctx, memory.OpportunitySpec{
		Summary: summary, Rationale: "user said they owe a quarterly update",
		EligibleAutonomous: true, Confidence: 0.95, OriginConversationID: "conv-origin",
	}); err != nil {
		t.Fatalf("RememberOpportunity (stage 5): %v", err)
	}
	wake()
	if len(runs) != 1 {
		t.Fatalf("stage permitted: want exactly 1 dispatched run, got %d", len(runs))
	}
	if runs[0].Summary != summary || !runs[0].EligibleAutonomous || runs[0].Status != memory.OpportunityPending {
		t.Fatalf("stage permitted: dispatched a wrong/ineligible pick %+v", runs[0])
	}
	if runs[0].OriginConversationID != "conv-origin" {
		t.Errorf("stage permitted: pick must carry origin_conversation_id for the resume, got %q", runs[0].OriginConversationID)
	}
	if gov.startedCalls != 1 {
		t.Fatalf("stage permitted: a dispatched task must advance the per-day counter once, got %d", gov.startedCalls)
	}
	if len(gov.reschedules) != 4 {
		t.Fatalf("stage permitted: want 4 reschedules total, got %d", len(gov.reschedules))
	}

	// Cumulative invariant across the whole walk: exactly one run total, and it
	// occurred only in the fully-permitted state; the alarm was cancelled once
	// (the single opt-out), and every non-opt-out wake rescheduled.
	if gov.cancelCalls != 1 {
		t.Errorf("over the whole walk, want exactly 1 cancel (the opt-out), got %d", gov.cancelCalls)
	}
}
