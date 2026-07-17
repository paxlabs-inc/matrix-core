// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"fmt"
)

// RecoveryAutomaton defines the bounded recovery transitions for a capability.
// Per O1 req.6 ac.3: probe, repair, reconcile, retry, verify, ask, and
// terminal transitions with explicit bounds and state predicates.
type RecoveryAutomaton struct {
	Operation   string               `json:"operation"`
	Initial     RecoveryState        `json:"initial"`
	Transitions []RecoveryTransition `json:"transitions"`
	Bound       int                  `json:"max_attempts"`
}

// RecoveryState is a named state in the recovery automaton.
type RecoveryState string

const (
	StateInitial     RecoveryState = "initial"
	StateProbing     RecoveryState = "probing"
	StateRepairing   RecoveryState = "repairing"
	StateReconciling RecoveryState = "reconciling"
	StateVerifying   RecoveryState = "verifying"
	StateComplete    RecoveryState = "complete"
	StateTerminal    RecoveryState = "terminal"
)

// RecoveryTransition is a single edge in the recovery automaton.
type RecoveryTransition struct {
	From  RecoveryState  `json:"from"`
	To    RecoveryState  `json:"to"`
	Kind  TransitionKind `json:"kind"`
	Guard string         `json:"guard"`
}

// RecoveryInstance tracks a running recovery through the automaton.
type RecoveryInstance struct {
	Automaton RecoveryAutomaton `json:"automaton"`
	Current   RecoveryState     `json:"current"`
	Attempts  int               `json:"attempts"`
	Err       string            `json:"error,omitempty"`
}

// RecoveryDecision is the deterministic transition selected for a concrete
// typed outcome. It is consumed by the production dispatch loop; the model is
// never asked to infer retryability from prose.
type RecoveryDecision struct {
	Transition TransitionKind `json:"transition"`
	Terminal   bool           `json:"terminal"`
	Reason     string         `json:"reason"`
}

// SelectRecoveryTransition chooses the next capability-owned transition from
// authoritative outcome and manifest state. State-changing operations never
// blind-retry after transport uncertainty: they reconcile first.
func SelectRecoveryTransition(manifest OperationManifest, outcome OperationOutcome, attempts int) RecoveryDecision {
	bound := manifest.Recovery.Bound
	if bound <= 0 {
		bound = 1
	}
	if outcome.Success {
		return RecoveryDecision{Terminal: true, Reason: "operation succeeded"}
	}
	if attempts >= bound {
		return RecoveryDecision{Transition: TransitionStop, Terminal: true, Reason: "recovery bound exhausted"}
	}
	stateChanging := manifest.Effects != EffectReadOnly
	if outcome.FailureLayer == LayerTransport && stateChanging {
		return RecoveryDecision{Transition: TransitionReconcile, Reason: "state-changing transport outcome is uncertain"}
	}
	if outcome.FailureLayer == LayerConflict {
		return RecoveryDecision{Transition: TransitionReconcile, Reason: "authoritative state conflict requires reconciliation"}
	}
	if outcome.FailureLayer == LayerValidation || outcome.FailureLayer == LayerProtocol ||
		outcome.FailureLayer == LayerInvocation {
		return RecoveryDecision{Transition: TransitionStop, Terminal: true, Reason: "deterministic invalid operation"}
	}
	if outcome.Retryable {
		return RecoveryDecision{Transition: TransitionRetry, Reason: "typed transient outcome permits bounded retry"}
	}
	return RecoveryDecision{Transition: TransitionStop, Terminal: true, Reason: "outcome has no safe recovery transition"}
}

// NewRecoveryInstance starts a recovery from the automaton's initial state.
func NewRecoveryInstance(auto RecoveryAutomaton) RecoveryInstance {
	return RecoveryInstance{
		Automaton: auto,
		Current:   auto.Initial,
		Attempts:  0,
	}
}

// Advance moves the recovery to the next state via a transition.
// Returns an error if the transition is not allowed or the bound is exhausted.
func (ri *RecoveryInstance) Advance(kind TransitionKind) error {
	return ri.AdvanceWhen(kind, nil)
}

// AdvanceWhen moves through an edge only when its named guard is present and
// true in authoritative facts. Unguarded test/local automata remain usable,
// while production recovery cannot silently ignore a state predicate.
func (ri *RecoveryInstance) AdvanceWhen(kind TransitionKind, facts map[string]bool) error {
	if ri.IsTerminal() {
		return fmt.Errorf("recovery for %s is already terminal", ri.Automaton.Operation)
	}
	if ri.Automaton.Bound > 0 && ri.Attempts >= ri.Automaton.Bound {
		return fmt.Errorf("recovery bound (%d) exhausted for %s", ri.Automaton.Bound, ri.Automaton.Operation)
	}
	for _, tr := range ri.Automaton.Transitions {
		if tr.From == ri.Current && tr.Kind == kind {
			if tr.Guard != "" && !facts[tr.Guard] {
				return fmt.Errorf("guard %q is not satisfied for %s", tr.Guard, ri.Automaton.Operation)
			}
			ri.Current = tr.To
			ri.Attempts++
			return nil
		}
	}
	return fmt.Errorf("no transition from %s with kind %s for %s", ri.Current, kind, ri.Automaton.Operation)
}

// ValidateRecoveryAutomaton rejects missing bounds, unknown states, empty
// guards, and non-terminal graphs before a capability can own the automaton.
func ValidateRecoveryAutomaton(auto RecoveryAutomaton) error {
	if auto.Operation == "" || auto.Bound <= 0 {
		return fmt.Errorf("recovery automaton requires operation and positive bound")
	}
	if auto.Initial == StateComplete || auto.Initial == StateTerminal || auto.Initial == "" {
		return fmt.Errorf("recovery automaton has invalid initial state %q", auto.Initial)
	}
	hasTerminal := false
	for i, tr := range auto.Transitions {
		if tr.From == "" || tr.To == "" || tr.Kind == "" || tr.Guard == "" {
			return fmt.Errorf("recovery transition %d is incomplete", i)
		}
		if tr.To == StateComplete || tr.To == StateTerminal {
			hasTerminal = true
		}
	}
	if !hasTerminal {
		return fmt.Errorf("recovery automaton has no terminal transition")
	}
	return nil
}

// IsTerminal returns true when the recovery has reached a terminal state.
func (ri RecoveryInstance) IsTerminal() bool {
	return ri.Current == StateTerminal || ri.Current == StateComplete
}

// PredefinedRecoveryAutomata returns the standard recovery patterns for
// common failure classes.
func PredefinedRecoveryAutomata() map[string]RecoveryAutomaton {
	return map[string]RecoveryAutomaton{
		"network": {
			Operation: "network_fetch",
			Initial:   StateInitial,
			Bound:     3,
			Transitions: []RecoveryTransition{
				{From: StateInitial, To: StateProbing, Kind: TransitionProbe, Guard: "network_available"},
				{From: StateProbing, To: StateInitial, Kind: TransitionRetry, Guard: "backoff_elapsed"},
				{From: StateProbing, To: StateTerminal, Kind: TransitionStop, Guard: "bound_exhausted"},
			},
		},
		"process": {
			Operation: "process_exec",
			Initial:   StateInitial,
			Bound:     3,
			Transitions: []RecoveryTransition{
				{From: StateInitial, To: StateRepairing, Kind: TransitionRepair, Guard: "dependency_missing"},
				{From: StateRepairing, To: StateInitial, Kind: TransitionAuthorizedFix, Guard: "authorized"},
				{From: StateInitial, To: StateProbing, Kind: TransitionRetry, Guard: "transient_failure"},
				{From: StateProbing, To: StateTerminal, Kind: TransitionStop, Guard: "bound_exhausted"},
			},
		},
		"validation": {
			Operation: "artifact_validation",
			Initial:   StateInitial,
			Bound:     2,
			Transitions: []RecoveryTransition{
				{From: StateInitial, To: StateRepairing, Kind: TransitionRepair, Guard: "repairable"},
				{From: StateRepairing, To: StateVerifying, Kind: TransitionRetry, Guard: "repaired"},
				{From: StateVerifying, To: StateComplete, Kind: TransitionProbe, Guard: "valid"},
				{From: StateVerifying, To: StateTerminal, Kind: TransitionStop, Guard: "unrepairable"},
			},
		},
		"conflict": {
			Operation: "state_conflict",
			Initial:   StateInitial,
			Bound:     2,
			Transitions: []RecoveryTransition{
				{From: StateInitial, To: StateReconciling, Kind: TransitionReconcile, Guard: "stale_state"},
				{From: StateReconciling, To: StateInitial, Kind: TransitionRetry, Guard: "reconciled"},
				{From: StateReconciling, To: StateTerminal, Kind: TransitionStop, Guard: "irreconcilable"},
			},
		},
	}
}
