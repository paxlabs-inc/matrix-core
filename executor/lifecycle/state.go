// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package lifecycle enforces the Intent lifecycle state machine from
// research/02-protocol.md §7. Every transition is explicit + audited;
// invalid transitions are rejected at the boundary.
//
// State diagram (research/02-protocol.md §7 verbatim):
//
//	drafting → proposed → clarifying ⟷ proposed → accepted →
//	executing → {completed | failed | cancelled}
//
//	Plus correction + gate transitions that overlay on executing.
//
// The state machine itself is pure (no I/O, no clocks); persistence is
// the caller's concern. The executor wires Machine.Apply to envelope
// reception + cortex Event memory writes so every state move is both
// audited (via the envelope log) and queryable (via cortex).
//
// v1 maps 15 MCL message kinds → state transitions per the table in
// MessageKindTransition. Not every kind triggers a transition: streaming
// kinds (plan.output) and informational kinds (plan.step) leave state
// unchanged.
package lifecycle

import (
	"fmt"
	"sort"
)

// State is one of 8 lifecycle states. Closed enum.
//
// Terminal states: StateCompleted, StateFailed, StateCancelled.
// No transitions out of a terminal state.
type State string

const (
	// StateDrafting is the initial state after intent.draft is received
	// by the agent. Compiler is running.
	StateDrafting State = "drafting"

	// StateProposed means the typed Intent IR has been returned to the
	// user (intent.compiled sent). User is reviewing.
	StateProposed State = "proposed"

	// StateClarifying means the agent has emitted intent.clarify and is
	// waiting for intent.answer. Can be re-entered if subsequent clarify
	// rounds are needed.
	StateClarifying State = "clarifying"

	// StateAccepted means the user signed intent.accept. The executor
	// has a valid signed IR to run.
	StateAccepted State = "accepted"

	// StateExecuting means the executor is actively walking the plan.
	// PolicyGate transitions overlay on this state without leaving it.
	StateExecuting State = "executing"

	// StateCompleted is terminal: intent.attest with outcome=success
	// has been emitted.
	StateCompleted State = "completed"

	// StateFailed is terminal: intent.fail has been emitted.
	StateFailed State = "failed"

	// StateCancelled is terminal: intent.cancel was honored before
	// completion.
	StateCancelled State = "cancelled"
)

// AllStates is the canonical ordered list of 8 states.
var AllStates = []State{
	StateDrafting,
	StateProposed,
	StateClarifying,
	StateAccepted,
	StateExecuting,
	StateCompleted,
	StateFailed,
	StateCancelled,
}

// validStates is the lookup set built once.
var validStates = func() map[State]bool {
	m := make(map[State]bool, len(AllStates))
	for _, s := range AllStates {
		m[s] = true
	}
	return m
}()

// ValidState reports whether s is one of the 8 known states.
func ValidState(s State) bool {
	return validStates[s]
}

// IsTerminal reports whether s is one of the three terminal states.
// No transitions out of a terminal state are permitted.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled:
		return true
	}
	return false
}

// allowedTransitions encodes the directed graph of legal moves per
// research/02-protocol.md §7. (current, next) → allowed.
//
// Notes:
//   - drafting → proposed       compiler emits intent.compiled
//   - proposed → clarifying     agent emits intent.clarify
//   - clarifying → proposed     user emits intent.answer (re-compile loop)
//   - proposed → accepted       user emits intent.accept
//   - accepted → executing      executor begins plan walk
//   - executing → completed     intent.attest outcome=success
//   - executing → failed        intent.fail
//   - executing → cancelled     intent.cancel honored mid-flight
//   - Any non-terminal → cancelled (user can cancel at any time before terminal)
//
// Correction (intent.correct) lands as either:
//   - executing → executing (non-material) — handled by the executor
//     internally; lifecycle state unchanged
//   - executing → accepted   (material; requires fresh accept loop) —
//     re-derives a new plan, must transit back through accepted
//
// Both directions are encoded so the lifecycle machine doesn't need
// out-of-band knowledge of materiality (the executor passes the
// classified-material flag at apply time).
var allowedTransitions = map[State]map[State]bool{
	StateDrafting: {
		StateProposed:  true,
		StateFailed:    true, // compile error
		StateCancelled: true,
	},
	StateProposed: {
		StateClarifying: true,
		StateAccepted:   true,
		StateCancelled:  true,
		StateFailed:     true,
	},
	StateClarifying: {
		StateProposed:  true,
		StateCancelled: true,
		StateFailed:    true,
	},
	StateAccepted: {
		StateExecuting: true,
		StateCancelled: true,
		StateFailed:    true,
	},
	StateExecuting: {
		StateCompleted: true,
		StateFailed:    true,
		StateCancelled: true,
		StateAccepted:  true, // material intent.correct re-enters accept
		StateExecuting: true, // non-material intent.correct (no-op transition)
	},
	// Terminal states: no outgoing edges
}

// Transition returns true if moving from `from` to `to` is permitted.
// Self-transitions are rejected EXCEPT for the executing→executing
// no-op used by non-material corrections (explicit allow in the table).
func Transition(from, to State) bool {
	if !ValidState(from) || !ValidState(to) {
		return false
	}
	if from.IsTerminal() {
		return false
	}
	next, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	return next[to]
}

// AllowedNext returns the set of states reachable in one transition
// from `from`, sorted alphabetically for deterministic output. Empty
// for terminal states.
func AllowedNext(from State) []State {
	next, ok := allowedTransitions[from]
	if !ok {
		return nil
	}
	out := make([]State, 0, len(next))
	for s := range next {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// formatTransition is a debug-friendly stringer for transitions.
func formatTransition(from, to State) string {
	return fmt.Sprintf("%s → %s", from, to)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
