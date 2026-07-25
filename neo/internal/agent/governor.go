// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// governor.go — ONE governor (MORPHEUS req.5): the agent's self-correction is
// a single designed system over the unified per-step signal state
// (signals.go), with a PROVEN fire order and one terminal death verdict.
//
// The fire order (req.5.2), enforced by the stage each read is wired into:
//
//  1. EPISTEMIC first — governEpistemic at generate entry: a pending forced
//     revision (armed by the mismatch limit, the convergence window, or a
//     check-before-act refusal) runs as a tools-stripped reasoning step
//     BEFORE any further model batch or dispatch. The check-before-act gate
//     itself (checkBeforeAct, act stage) refuses dependent dispatches while a
//     refuted premise stands.
//  2. CASSANDRA voice second — governVoice at deliberate: the silent in-place
//     mod over the unified state's controller projection, bounded by
//     min_step / max_mods_per_turn / per-trigger cooldown.
//  3. OUTER FAILSAFES last — governFailsafes in act (the hard NoProgressStall
//     commit) plus the step budget (Chat) and the unproductive cap
//     (escalateGuidance) — the bounds that end the turn when the designed
//     layers above did not converge it.
//
// Every bound and cooldown is preserved verbatim: mismatch limit
// (EpistemicMismatchLimit), convergence window (EpistemicConvergenceWindow),
// maxRevisionsPerTurn, Cassandra min_step/max_mods/cooldown, NoProgressStall,
// the step budget, and the unproductive cap. A clean, healthy run is
// byte-identical to the governor disabled (silence-when-healthy, req.5.3).
package agent

import (
	"fmt"

	"matrix/neo/internal/llm"
	"matrix/neo/internal/o1"
)

// governEpistemic is fire position 1: reports the pending forced-revision
// directive (armed by the epistemic bounds) that must run as a tools-stripped
// reasoning-only step BEFORE any further dispatch. Contract — mutates nothing;
// generate consumes it at entry and forcedRevisionStep clears it.
func (a *Agent) governEpistemic() (directive string, due bool) {
	return a.turn.revisionPending, a.turn.revisionPending != ""
}

// governVoice is fire position 2: the Cassandra silent-voice read over the
// unified signal state's controller projection. A no-op (false) when the
// controller is disabled — the healthy path stays byte-identical. Contract —
// mutates: the just-appended assistant message's Content (in place) and the
// Cassandra per-turn state, only when a trigger fires.
func (a *Agent) governVoice(s *stepSignals) bool {
	if !a.cfg.CassandraEnabled {
		return false
	}
	return a.cassandraStep(s.cassandraView())
}

// governFailsafes is fire position 3: the outer no-progress failsafe — commits
// the stall bookkeeping from the unified state (noteBatch) and returns the
// repeat/stall verdict. The step budget and the unproductive cap, the other
// two outer failsafes, are enforced at their own sites (Chat's loop bound and
// escalateGuidance) and terminate through the same governDeath verdict.
func (a *Agent) governFailsafes(calls []llm.ToolCall) (repeat, stalled bool) {
	return a.noteBatch(calls)
}

// governDeath is the governor's ONE terminal verdict (req.5.4): every
// loop-affecting death — the hard stall, the step budget, the unproductive cap
// — funnels through here. One place decides and records why a turn ended:
// consolidate the transcript exactly once, record the structured death (the
// supervisor's LastDeath read and the next-turn failure-context injection ride
// it), and return the honest ErrIncomplete. message carries the reason prose
// (ending with a colon); the where-it-stands digest is appended.
func (a *Agent) governDeath(reason, message string) error {
	a.consolidateWorking()
	digest := a.lastToolSummary()
	a.recordDeath(reason, digest)
	if a.turn != nil && a.turn.runLedger != nil {
		outcome := o1.NewOutcome("agent_turn").Fail(o1.LayerApplication, "internal_convergence_failure").
			Retryable(true).AllowTransition(o1.TransitionRetry).
			Evidence(string(reason) + ": " + oneLine(digest)).MustBuild()
		a.turn.commitOutcomeIfStandingIsNotTerminal(outcome)
		_, _ = a.turn.runLedger.RecordAttempt(o1.AttemptRecord{
			Operation: "agent_turn", PreState: a.turn.runLedger.RunID,
			PostState: a.turn.runLedger.RunID, Outcome: outcome, Evidence: outcome.Evidence,
		})
		a.persistO1State()
	}
	return fmt.Errorf("%w: %s %s", ErrIncomplete, message, oneLine(digest))
}
