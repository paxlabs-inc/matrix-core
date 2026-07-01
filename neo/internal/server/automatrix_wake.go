// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/notify"
)

// The AUTOMATRIX idle-wake handler (task 3.3).
//
// A Chronos recurring AUTOMATRIX alarm fires while the user is idle (the
// per-user daemon scaled to zero between requests) and is delivered as a
// POST /chat turn carrying the AUTOMATRIX marker. This handler is the DECISION
// logic that sits in front of any proactive work:
//
//  1. re-read the per-user opt-in on EVERY wake — if the user has opted out
//     since the alarm was created, cancel the alarm and do nothing (req 4.4);
//  2. defer (reschedule, never run) when a run is already in flight for the
//     user — Automatrix never competes with or interrupts real work (req 4.5);
//  3. enforce the per-day cap (anti-spam, req 4.7);
//  4. select the single top-ranked eligible opportunity (req 4.6 / 5.1) — none
//     resolves to the AUTOMATRIX_IDLE outcome (no run, just reschedule);
//  5. reschedule the next wake with randomized jitter within the configured
//     window plus a probabilistic skip, so wakes feel random rather than
//     clockwork (req 4.3).
//
// Chronos remains the durable timing backbone (the alarm + the next-fire time +
// the per-day counter live in its Postgres, the source of truth); only the
// jitter/skip COMPUTATION lives here in Neo. The opt-in setting, the per-day
// counter, and the alarm cancel/reschedule are read/written through the
// AutomatrixGovernor seam (satisfied by the daemon in task 6.1). The actual
// supervised, restricted-surface run dispatch is the AutomatrixRunner seam
// (task 4.1). This handler owns the pick + the reschedule/cap bookkeeping; it
// leaves the scheduled->in_progress status transition and the run itself to the
// runner (task 4.1), so a picked opportunity stays pending until 4.1 genuinely
// starts it.

// AutomatrixGovernor is the per-user opt-in + Chronos-alarm bookkeeping seam the
// wake handler consumes on every wake. It is the clean boundary between this
// task (3.3, the wake DECISION logic) and task 6.1 (the daemon opt-in setting +
// the real Chronos alarm_set/alarm_cancel lifecycle, a later wave). Until a real
// governor is wired, the engine leaves it nil and the wake handler does nothing:
// a wake cannot verify the opt-in state without it, so it must not run proactive
// work (fail closed).
type AutomatrixGovernor interface {
	// Enabled returns the authoritative per-user opt-in, re-read on EVERY wake
	// (req 4.4 / 7.1). A wake on an opted-out user must do nothing.
	Enabled(ctx context.Context) bool
	// TasksToday returns the number of proactive tasks already started today,
	// for the per-day cap (req 4.7). DB-backed (the Chronos alarm payload is the
	// source of truth) and resets daily.
	TasksToday(ctx context.Context) int
	// RecordTaskStarted increments the per-day counter after a proactive task is
	// dispatched (persisted via the alarm payload — the DB stays authoritative).
	RecordTaskStarted(ctx context.Context) error
	// CancelAlarm cancels the recurring AUTOMATRIX alarm — the opt-out path
	// (req 4.4: cancel the alarm and return when the user has opted out).
	CancelAlarm(ctx context.Context) error
	// Reschedule sets the next wake's fire time (the jittered next-fire Neo
	// computed) on the durable Chronos alarm (req 4.3: the DB is the timing
	// backbone; the jitter/skip lives in Neo).
	Reschedule(ctx context.Context, next time.Time) error
}

// AutomatrixRunner dispatches the supervised, restricted-surface run for a
// picked opportunity — the task 4.1 hand-off. This handler (3.3) calls it after
// a successful pick; 4.1 owns marking the opportunity scheduled->in_progress,
// resuming into its origin_conversation_id, building the objective from
// summary+rationale, running on the no-money tool surface, and the completion
// gate. A nil runner means proactive execution is not wired yet: the handler
// reschedules without running and the opportunity stays pending for a later
// wake.
type AutomatrixRunner func(ctx context.Context, opp memory.OpportunitySpec) error

// SetAutomatrixGovernor wires the per-user opt-in + alarm bookkeeping seam
// (task 6.1). Safe to leave unset: the wake handler fails closed without it.
func (e *Engine) SetAutomatrixGovernor(g AutomatrixGovernor) { e.automatrixGov = g }

// SetAutomatrixRunner wires the supervised restricted-surface run dispatch
// (task 4.1). Safe to leave unset: the handler still picks + reschedules.
func (e *Engine) SetAutomatrixRunner(r AutomatrixRunner) { e.automatrixRunner = r }

// SetAutomatrixNotifier overrides the out-of-app completion-ping Notifier
// (task 5.3). NewEngine already builds one from config; this seam lets wiring
// or a test install a real Notifier pointed at a specific endpoint (e.g. an
// httptest ntfy server). A nil notifier is tolerated downstream (notify.Deliver
// treats it as a safe no-op), and the durable in-app record is written
// regardless.
func (e *Engine) SetAutomatrixNotifier(n notify.Notifier) { e.automatrixNotifier = n }

// automatrixMinInterval floors a computed next-fire so jitter (or a zero/short
// base) can never schedule an immediate or past-dated wake.
const automatrixMinInterval = time.Minute

// automatrixSkipProbability is the default chance that a given fire is skipped
// (no work this round, just reschedule) so wakes feel random rather than
// clockwork (req 4.3). Tests pin the engine's copy to 0/1 for determinism.
const automatrixSkipProbability = 0.2

// automatrixOutcome is the decision the pure wake-decider reaches.
type automatrixOutcome int

const (
	automatrixOptedOut  automatrixOutcome = iota // opt-in off — cancel the alarm, do not run (req 4.4)
	automatrixDeferBusy                          // a run is in flight — defer, do not run (req 4.5)
	automatrixCapped                             // per-day cap hit — defer, do not run (req 4.7)
	automatrixSkipped                            // probabilistic skip — defer, do not run (req 4.3)
	automatrixIdle                               // no eligible opportunity — AUTOMATRIX_IDLE, reschedule (req 4.6)
	automatrixRun                                // pick one eligible opportunity and hand it to the runner (req 5.1)
)

// automatrixWakeInput is the fully-gathered, pure input to the wake decision.
type automatrixWakeInput struct {
	enabled    bool                     // authoritative per-user opt-in (re-read this wake)
	busy       bool                     // a run already in flight for the user
	tasksToday int                      // proactive tasks already started today
	maxPerDay  int                      // per-day cap (0 = uncapped)
	base       time.Duration            // base wake cadence
	jitter     time.Duration            // ± randomization window
	skipProb   float64                  // probability this fire is skipped
	pending    []memory.OpportunitySpec // ranked eligible-pending queue (top first)
}

// automatrixDecision is the pure decider's result: an outcome, the picked
// opportunity (valid only for automatrixRun), and the jittered delay until the
// next fire (meaningless for automatrixOptedOut, which cancels instead).
type automatrixDecision struct {
	outcome automatrixOutcome
	pick    memory.OpportunitySpec
	next    time.Duration
}

// decideAutomatrixWake is the PURE wake-decision policy. It consumes rng in a
// fixed order (jitter then skip) so any reschedule path is deterministic for a
// seeded rng regardless of which branch is taken. Order of checks matters:
// opt-out wins over everything (cancel, never run); a busy session defers; the
// per-day cap defers; a probabilistic skip defers; otherwise pick the top
// eligible-pending opportunity, or resolve to idle when the queue is empty.
func decideAutomatrixWake(in automatrixWakeInput, rng *rand.Rand) automatrixDecision {
	next := jitteredInterval(in.base, in.jitter, rng)
	skip := in.skipProb > 0 && rng.Float64() < in.skipProb

	switch {
	case !in.enabled:
		return automatrixDecision{outcome: automatrixOptedOut}
	case in.busy:
		return automatrixDecision{outcome: automatrixDeferBusy, next: next}
	case in.maxPerDay > 0 && in.tasksToday >= in.maxPerDay:
		return automatrixDecision{outcome: automatrixCapped, next: next}
	case skip:
		return automatrixDecision{outcome: automatrixSkipped, next: next}
	}
	// Defense in depth (req 3.3): the picker only ever selects an eligible,
	// pending opportunity. PendingOpportunities already filters to exactly that
	// set, but the guard makes the structural guarantee local to the pick.
	for _, o := range in.pending {
		if o.Status == memory.OpportunityPending && o.EligibleAutonomous {
			return automatrixDecision{outcome: automatrixRun, pick: o, next: next}
		}
	}
	return automatrixDecision{outcome: automatrixIdle, next: next}
}

// jitteredInterval computes the next-fire delay as base ± a uniform draw in
// [-jitter, +jitter], floored at automatrixMinInterval so a wide jitter (or a
// zero base) can never schedule an immediate/past wake. Consumes one rng draw
// only when jitter > 0.
func jitteredInterval(base, jitter time.Duration, rng *rand.Rand) time.Duration {
	d := base
	if jitter > 0 {
		d += time.Duration(rng.Int63n(int64(2*jitter)+1)) - jitter
	}
	if d < automatrixMinInterval {
		d = automatrixMinInterval
	}
	return d
}

// MaybeHandleAutomatrixWake intercepts a Chronos AUTOMATRIX wake delivered over
// POST /chat. It returns true iff the message is an AUTOMATRIX wake (so the
// caller must NOT dispatch it as a normal user turn): the wake produces no live
// conversational run of its own — any proactive work is dispatched out of band
// by the runner seam (task 4.1). A non-wake message returns false and is left
// for the normal /chat path.
func (e *Engine) MaybeHandleAutomatrixWake(ctx context.Context, convID, message string) bool {
	if !strings.Contains(message, agent.AutomatrixWakeMarker) {
		return false
	}
	e.handleAutomatrixWake(ctx, convID)
	return true
}

// handleAutomatrixWake runs the wake decision and acts on it: cancel on opt-out,
// hand the pick to the runner (task 4.1) on a run outcome, and reschedule the
// next jittered wake on every non-opt-out path. Best-effort throughout — a
// governor/runner/pager error is logged and never wedges the wake.
func (e *Engine) handleAutomatrixWake(ctx context.Context, convID string) {
	gov := e.automatrixGov
	if gov == nil {
		// No opt-in source wired yet (task 6.1, a later wave): a wake cannot
		// verify the user opted in, so do nothing — fail closed. Proactive work
		// never runs without an authoritative opt-in.
		return
	}

	in := automatrixWakeInput{
		enabled:    gov.Enabled(ctx),
		busy:       e.automatrixBusy(),
		tasksToday: gov.TasksToday(ctx),
		maxPerDay:  e.cfg.AutomatrixMaxTasksPerDay,
		base:       time.Duration(e.cfg.AutomatrixBaseIntervalMinutes) * time.Minute,
		jitter:     time.Duration(e.cfg.AutomatrixJitterMinutes) * time.Minute,
		skipProb:   e.automatrixSkipProb,
	}
	// Fetch the ranked eligible-pending queue only when the gates we have so far
	// would let work run (enabled, not busy) and a pager exists — a nil pager or
	// a read error yields an empty queue (idle).
	if in.enabled && !in.busy && e.pager != nil {
		if pend, err := e.pager.PendingOpportunities(ctx, 1); err != nil {
			fmt.Fprintf(os.Stderr, "neo/automatrix: pending opportunities: %v\n", err)
		} else {
			in.pending = pend
		}
	}

	dec := e.decideAutomatrixWakeLocked(in)

	switch dec.outcome {
	case automatrixOptedOut:
		// req 4.4: opted out since the alarm was created — cancel it and return
		// (no reschedule; the alarm is gone).
		if err := gov.CancelAlarm(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "neo/automatrix: cancel alarm: %v\n", err)
		}
		return
	case automatrixRun:
		// Hand the picked opportunity to the supervised restricted-surface
		// dispatch (task 4.1). The per-day counter only advances when a task is
		// genuinely started, so RecordTaskStarted follows a clean dispatch.
		if e.automatrixRunner == nil {
			// Proactive execution not wired yet: leave the opportunity pending
			// and just reschedule. Do NOT advance the per-day counter — nothing
			// ran.
			fmt.Fprintln(os.Stderr, "neo/automatrix: opportunity picked but no runner wired (task 4.1); rescheduling")
		} else if err := e.automatrixRunner(ctx, dec.pick); err != nil {
			fmt.Fprintf(os.Stderr, "neo/automatrix: run opportunity: %v\n", err)
		} else if err := gov.RecordTaskStarted(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "neo/automatrix: record task started: %v\n", err)
		}
	}

	// Every non-opt-out path reschedules the next jittered wake. Chronos stays
	// the durable timing backbone (req 4.3): Neo computed the next fire, the DB
	// records it.
	if err := gov.Reschedule(ctx, time.Now().UTC().Add(dec.next)); err != nil {
		fmt.Fprintf(os.Stderr, "neo/automatrix: reschedule: %v\n", err)
	}
}

// decideAutomatrixWakeLocked runs the pure decider under the RNG mutex (rand.Rand
// is not safe for concurrent use; wakes are infrequent so the lock is uncontended).
func (e *Engine) decideAutomatrixWakeLocked(in automatrixWakeInput) automatrixDecision {
	e.automatrixRandMu.Lock()
	defer e.automatrixRandMu.Unlock()
	return decideAutomatrixWake(in, e.automatrixRand)
}

// automatrixBusy reports whether any run is in flight for the user. Because the
// daemon is per-user, any active run means the user (or a prior proactive task)
// is busy, so a new proactive wake must defer rather than compete (req 4.5). It
// covers both live conversational runs (the runs registry) and an autonomous
// Automatrix run already executing (the in-flight guard, task 4.1) so two
// proactive tasks never overlap.
func (e *Engine) automatrixBusy() bool {
	if e.automatrixInflight.Load() > 0 {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.runs) > 0
}
