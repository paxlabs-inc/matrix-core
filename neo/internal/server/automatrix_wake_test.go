// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Wake-path tests for the AUTOMATRIX idle-wake handler (task 3.3, req
// 4.3–4.7). The pure decider tests exercise the REAL decision/jitter/skip
// logic over REAL memory.OpportunitySpec values with a seeded REAL *rand.Rand —
// no doubles at all. The handler tests drive the REAL Engine + a REAL Neocortex
// pager (under t.TempDir(), hash embedder) so PendingOpportunities is the
// genuine code path; the AutomatrixGovernor is a faithful in-package
// implementation of the seam that stores real state and records real calls
// (the production governor — the daemon opt-in + Chronos alarm lifecycle — is
// task 6.1, a later wave), and the AutomatrixRunner records the picked
// opportunity. The assertions verify the REAL handler's behavior, not canned
// governor output.

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

// stateGovernor is a real, faithful implementation of the AutomatrixGovernor
// seam: it stores genuine opt-in + per-day-counter state and records the
// cancel/reschedule/record calls the handler makes. It is the in-test stand-in
// for the daemon governor (task 6.1) — it does not fabricate decisions, it
// just holds state and tallies the handler's real actions.
type stateGovernor struct {
	enabled      bool
	tasksToday   int
	cancelCalls  int
	startedCalls int
	reschedules  []time.Time
}

func (g *stateGovernor) Enabled(context.Context) bool   { return g.enabled }
func (g *stateGovernor) TasksToday(context.Context) int { return g.tasksToday }
func (g *stateGovernor) RecordTaskStarted(context.Context) error {
	g.startedCalls++
	g.tasksToday++
	return nil
}
func (g *stateGovernor) CancelAlarm(context.Context) error { g.cancelCalls++; return nil }
func (g *stateGovernor) Reschedule(_ context.Context, next time.Time) error {
	g.reschedules = append(g.reschedules, next)
	return nil
}

// newWakeTestEngine builds a real Engine backed by a real Neocortex pager under
// t.TempDir() (hash embedder, fully offline). The RNG is seeded for
// determinism; the skip probability is pinned per test.
func newWakeTestEngine(t *testing.T) (*Engine, *memory.Pager) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-automatrix-wake-test"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	e := NewEngine(EngineOptions{
		Config:     cfg,
		Pager:      pager,
		BackendURL: "http://127.0.0.1:1",
	})
	e.automatrixRand = rand.New(rand.NewSource(1)) // deterministic
	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})
	return e, pager
}

// --- pure decider tests (zero doubles) ---

func baseInput() automatrixWakeInput {
	return automatrixWakeInput{
		enabled:   true,
		busy:      false,
		maxPerDay: 3,
		base:      45 * time.Minute,
		jitter:    30 * time.Minute,
		skipProb:  0, // no skip unless a test opts in
		pending: []memory.OpportunitySpec{{
			Summary:            "Draft the quarterly update doc you mentioned",
			Status:             memory.OpportunityPending,
			EligibleAutonomous: true,
			Confidence:         0.8,
		}},
	}
}

func TestDecideAutomatrixWake_OptedOutCancels(t *testing.T) {
	in := baseInput()
	in.enabled = false
	dec := decideAutomatrixWake(in, rand.New(rand.NewSource(1)))
	if dec.outcome != automatrixOptedOut {
		t.Fatalf("opted-out wake outcome = %v, want automatrixOptedOut", dec.outcome)
	}
}

func TestDecideAutomatrixWake_BusyDefers(t *testing.T) {
	in := baseInput()
	in.busy = true
	dec := decideAutomatrixWake(in, rand.New(rand.NewSource(1)))
	if dec.outcome != automatrixDeferBusy {
		t.Fatalf("busy wake outcome = %v, want automatrixDeferBusy", dec.outcome)
	}
	if dec.next <= 0 {
		t.Error("a deferred wake must still compute a positive next-fire delay")
	}
}

func TestDecideAutomatrixWake_CapEnforced(t *testing.T) {
	in := baseInput()
	in.tasksToday = 3 // == maxPerDay
	dec := decideAutomatrixWake(in, rand.New(rand.NewSource(1)))
	if dec.outcome != automatrixCapped {
		t.Fatalf("capped wake outcome = %v, want automatrixCapped", dec.outcome)
	}
	// Under the cap, the same input must run.
	in.tasksToday = 2
	dec = decideAutomatrixWake(in, rand.New(rand.NewSource(1)))
	if dec.outcome != automatrixRun {
		t.Fatalf("under-cap wake outcome = %v, want automatrixRun", dec.outcome)
	}
}

func TestDecideAutomatrixWake_EmptyQueueIdle(t *testing.T) {
	in := baseInput()
	in.pending = nil
	dec := decideAutomatrixWake(in, rand.New(rand.NewSource(1)))
	if dec.outcome != automatrixIdle {
		t.Fatalf("empty-queue wake outcome = %v, want automatrixIdle", dec.outcome)
	}
}

func TestDecideAutomatrixWake_PicksTopEligible(t *testing.T) {
	in := baseInput()
	dec := decideAutomatrixWake(in, rand.New(rand.NewSource(1)))
	if dec.outcome != automatrixRun {
		t.Fatalf("eligible wake outcome = %v, want automatrixRun", dec.outcome)
	}
	if dec.pick.Summary != in.pending[0].Summary {
		t.Errorf("picked %q, want the top-ranked %q", dec.pick.Summary, in.pending[0].Summary)
	}
}

// A financial / non-pending item must never be picked even if it somehow
// reaches the decider (defense in depth — PendingOpportunities already filters).
func TestDecideAutomatrixWake_NeverPicksIneligible(t *testing.T) {
	in := baseInput()
	in.pending = []memory.OpportunitySpec{
		{Summary: "Swap 1 ETH for USDC", Status: memory.OpportunityPending, EligibleAutonomous: false},
		{Summary: "Already scheduled item", Status: memory.OpportunityScheduled, EligibleAutonomous: true},
	}
	dec := decideAutomatrixWake(in, rand.New(rand.NewSource(1)))
	if dec.outcome != automatrixIdle {
		t.Fatalf("outcome = %v, want automatrixIdle (no eligible-pending item)", dec.outcome)
	}
}

// The skip probability genuinely fires near its configured rate.
func TestDecideAutomatrixWake_SkipIsProbabilistic(t *testing.T) {
	in := baseInput()
	in.skipProb = 0.5
	rng := rand.New(rand.NewSource(7))
	const n = 4000
	skips := 0
	for i := 0; i < n; i++ {
		if decideAutomatrixWake(in, rng).outcome == automatrixSkipped {
			skips++
		}
	}
	rate := float64(skips) / float64(n)
	if rate < 0.42 || rate > 0.58 {
		t.Fatalf("skip rate = %.3f, want ~0.50 (probabilistic skip not firing as configured)", rate)
	}
	// skipProb = 0 must never skip.
	in.skipProb = 0
	for i := 0; i < 500; i++ {
		if decideAutomatrixWake(in, rng).outcome == automatrixSkipped {
			t.Fatal("skipProb=0 must never skip")
		}
	}
}

// The rescheduled next-fire always lands inside the configured jitter window
// [base-jitter, base+jitter], floored at the minimum interval.
func TestDecideAutomatrixWake_JitterWithinWindow(t *testing.T) {
	in := baseInput()
	rng := rand.New(rand.NewSource(99))
	lo := in.base - in.jitter
	hi := in.base + in.jitter
	for i := 0; i < 5000; i++ {
		next := decideAutomatrixWake(in, rng).next
		if next < automatrixMinInterval {
			t.Fatalf("next-fire %v below the floor %v", next, automatrixMinInterval)
		}
		if next < lo || next > hi {
			t.Fatalf("next-fire %v outside the jitter window [%v, %v]", next, lo, hi)
		}
	}
}

func TestJitteredInterval_NoJitterIsBase(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	if got := jitteredInterval(45*time.Minute, 0, rng); got != 45*time.Minute {
		t.Fatalf("jitteredInterval with no jitter = %v, want 45m", got)
	}
	// A zero base with no jitter is floored to the minimum interval.
	if got := jitteredInterval(0, 0, rng); got != automatrixMinInterval {
		t.Fatalf("jitteredInterval(0,0) = %v, want the %v floor", got, automatrixMinInterval)
	}
}

// --- handler tests (real Engine + real pager + faithful seam impls) ---

func TestMaybeHandleAutomatrixWake_DetectsMarker(t *testing.T) {
	e, _ := newWakeTestEngine(t)
	// No governor wired: a wake is still recognized (returns true) but does
	// nothing (fails closed).
	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Error("the canonical AUTOMATRIX wake message must be recognized as a wake")
	}
	if e.MaybeHandleAutomatrixWake(context.Background(), "c", "hey can you help me with this doc") {
		t.Error("a normal user message must NOT be treated as an AUTOMATRIX wake")
	}
}

func TestHandleAutomatrixWake_OptedOutCancels(t *testing.T) {
	e, _ := newWakeTestEngine(t)
	gov := &stateGovernor{enabled: false}
	e.SetAutomatrixGovernor(gov)

	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Fatal("wake not recognized")
	}
	if gov.cancelCalls != 1 {
		t.Errorf("opted-out wake must cancel the alarm exactly once, got %d", gov.cancelCalls)
	}
	if len(gov.reschedules) != 0 {
		t.Error("an opted-out wake must NOT reschedule (the alarm is cancelled)")
	}
	if gov.startedCalls != 0 {
		t.Error("an opted-out wake must never start a task")
	}
}

func TestHandleAutomatrixWake_BusyDefers(t *testing.T) {
	e, pager := newWakeTestEngine(t)
	// A genuinely eligible opportunity is queued — but the session is busy.
	if _, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary: "Draft the quarterly update doc you mentioned", EligibleAutonomous: true, Confidence: 0.8,
	}); err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	gov := &stateGovernor{enabled: true}
	e.SetAutomatrixGovernor(gov)
	var ran int
	e.SetAutomatrixRunner(func(context.Context, memory.OpportunitySpec) error { ran++; return nil })
	// Simulate a run already in flight for the user (the REAL busy signal).
	e.registerRun(&run{id: "busy-run", convID: "other"})

	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Fatal("wake not recognized")
	}
	if ran != 0 {
		t.Error("a busy session must defer — no proactive run may start")
	}
	if len(gov.reschedules) != 1 {
		t.Errorf("a busy wake must reschedule exactly once, got %d", len(gov.reschedules))
	}
	if gov.cancelCalls != 0 {
		t.Error("a busy wake must not cancel the alarm")
	}
}

func TestHandleAutomatrixWake_CapDefers(t *testing.T) {
	e, pager := newWakeTestEngine(t)
	if _, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary: "Draft the quarterly update doc you mentioned", EligibleAutonomous: true, Confidence: 0.8,
	}); err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	gov := &stateGovernor{enabled: true, tasksToday: e.cfg.AutomatrixMaxTasksPerDay}
	e.SetAutomatrixGovernor(gov)
	e.automatrixSkipProb = 0 // isolate the cap from the skip
	var ran int
	e.SetAutomatrixRunner(func(context.Context, memory.OpportunitySpec) error { ran++; return nil })

	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Fatal("wake not recognized")
	}
	if ran != 0 {
		t.Error("the per-day cap must prevent another proactive run")
	}
	if len(gov.reschedules) != 1 {
		t.Errorf("a capped wake must still reschedule, got %d", len(gov.reschedules))
	}
}

func TestHandleAutomatrixWake_EmptyQueueIdle(t *testing.T) {
	e, _ := newWakeTestEngine(t)
	gov := &stateGovernor{enabled: true}
	e.SetAutomatrixGovernor(gov)
	e.automatrixSkipProb = 0
	var ran int
	e.SetAutomatrixRunner(func(context.Context, memory.OpportunitySpec) error { ran++; return nil })

	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Fatal("wake not recognized")
	}
	if ran != 0 {
		t.Error("an empty queue resolves to idle — no run")
	}
	if len(gov.reschedules) != 1 {
		t.Errorf("an idle wake must reschedule, got %d", len(gov.reschedules))
	}
	if gov.startedCalls != 0 {
		t.Error("an idle wake must not advance the per-day counter")
	}
}

func TestHandleAutomatrixWake_PicksAndDispatches(t *testing.T) {
	e, pager := newWakeTestEngine(t)
	const summary = "Draft the quarterly update doc you mentioned"
	if _, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary: summary, Rationale: "user said they owe a quarterly update", EligibleAutonomous: true, Confidence: 0.9,
		OriginConversationID: "conv-origin",
	}); err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	gov := &stateGovernor{enabled: true}
	e.SetAutomatrixGovernor(gov)
	e.automatrixSkipProb = 0 // deterministic: never skip this fire

	var picked []memory.OpportunitySpec
	e.SetAutomatrixRunner(func(_ context.Context, opp memory.OpportunitySpec) error {
		picked = append(picked, opp)
		return nil
	})

	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Fatal("wake not recognized")
	}
	if len(picked) != 1 {
		t.Fatalf("the top eligible opportunity must be handed to the runner exactly once, got %d", len(picked))
	}
	if picked[0].Summary != summary {
		t.Errorf("runner got %q, want %q", picked[0].Summary, summary)
	}
	if picked[0].OriginConversationID != "conv-origin" {
		t.Errorf("the pick must carry origin_conversation_id for the 4.1 resume, got %q", picked[0].OriginConversationID)
	}
	if gov.startedCalls != 1 {
		t.Errorf("a dispatched task must advance the per-day counter once, got %d", gov.startedCalls)
	}
	if len(gov.reschedules) != 1 {
		t.Errorf("a run wake must reschedule the next fire once, got %d", len(gov.reschedules))
	}
	// The rescheduled fire lands in the future, inside the jitter window.
	next := gov.reschedules[0]
	lo := time.Now().Add(15 * time.Minute) // base 45m - jitter 30m
	hi := time.Now().Add(75*time.Minute + time.Second)
	if next.Before(lo) || next.After(hi) {
		t.Errorf("next fire %v outside the expected jittered window", next)
	}
}

// With no runner wired (task 4.1 not yet present), a pick must NOT advance the
// per-day counter (nothing ran) but must still reschedule.
func TestHandleAutomatrixWake_PickWithoutRunnerReschedulesOnly(t *testing.T) {
	e, pager := newWakeTestEngine(t)
	if _, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary: "Draft the quarterly update doc you mentioned", EligibleAutonomous: true, Confidence: 0.8,
	}); err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	gov := &stateGovernor{enabled: true}
	e.SetAutomatrixGovernor(gov)
	e.automatrixSkipProb = 0
	// no runner wired

	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Fatal("wake not recognized")
	}
	if gov.startedCalls != 0 {
		t.Error("no task ran (no runner) — the per-day counter must not advance")
	}
	if len(gov.reschedules) != 1 {
		t.Errorf("a wake must still reschedule even without a runner, got %d", len(gov.reschedules))
	}
}

// No governor wired => fail closed: a recognized wake does nothing.
func TestHandleAutomatrixWake_NoGovernorFailsClosed(t *testing.T) {
	e, pager := newWakeTestEngine(t)
	if _, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary: "Draft the quarterly update doc you mentioned", EligibleAutonomous: true, Confidence: 0.8,
	}); err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	var ran int
	e.SetAutomatrixRunner(func(context.Context, memory.OpportunitySpec) error { ran++; return nil })
	if !e.MaybeHandleAutomatrixWake(context.Background(), "c", agent.AutomatrixWakeMessage) {
		t.Fatal("wake not recognized")
	}
	if ran != 0 {
		t.Error("with no opt-in source, a wake must run nothing (fail closed)")
	}
}
