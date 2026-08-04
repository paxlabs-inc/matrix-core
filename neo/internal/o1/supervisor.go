// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"fmt"
)

// SupervisorAction is the decision the supervisor makes after evaluating
// the ledger, outcomes, recovery automata, and verifiers.
// Per O1 req.10 ac.1: decisions come solely from typed state, not narration.
type SupervisorAction string

const (
	SupContinue  SupervisorAction = "continue"
	SupRepair    SupervisorAction = "repair"
	SupReconcile SupervisorAction = "reconcile"
	SupAskUser   SupervisorAction = "ask_user"
	SupStop      SupervisorAction = "stop"
	SupComplete  SupervisorAction = "complete"
)

// TerminalClass distinguishes the reason a run ended.
// Per O1 req.10 ac.4: distinguish completed work, residual effects,
// missing authorization, external unavailability, irreducible ambiguity,
// and internal failure.
type TerminalClass string

const (
	TermComplete             TerminalClass = "complete"
	TermMissingAuth          TerminalClass = "missing_authorization"
	TermIrreducibleAmbiguity TerminalClass = "irreducible_ambiguity"
	TermExternalUnavail      TerminalClass = "external_unavailability"
	TermSafePartial          TerminalClass = "safe_partial"
	TermInternalFailure      TerminalClass = "internal_failure"
	TermBudgetExhausted      TerminalClass = "budget_exhausted"
)

// SupervisorDecision is the structured output of the supervisor.
type SupervisorDecision struct {
	Action       SupervisorAction `json:"action"`
	Terminal     *TerminalClass   `json:"terminal,omitempty"`
	Reason       string           `json:"reason"`
	Evidence     string           `json:"evidence"`
	RecoveryHint string           `json:"recovery_hint,omitempty"`
}

// Supervisor evaluates the run state and produces a decision.
// Per O1 req.10 ac.1: decisions are driven solely by contract state,
// ledger, outcomes, recovery automata, authorization, and verifier evidence.
type Supervisor struct{}

// Evaluate produces a supervisor decision from the current run state.
func (s *Supervisor) Evaluate(
	contract TaskContract,
	ledger *RunLedger,
	graph *VerifierGraph,
	proof *ProofManifest,
	lastOutcome *OperationOutcome,
) SupervisorDecision {
	// 1. All criteria closed, no hazards, effects reconciled → complete
	if graph != nil && graph.AllClosed() && ledger != nil && len(ledger.OpenHazards) == 0 &&
		proof != nil && proof.IsComplete() {
		complete := TermComplete
		return SupervisorDecision{
			Action:   SupComplete,
			Terminal: &complete,
			Reason:   "all acceptance criteria closed, no open hazards",
			Evidence: "verifier graph all-closed, ledger clean",
		}
	}
	// 2. A genuinely unresolved material input is actionable and must not be
	// hidden behind a later generic retryable convergence outcome.
	if !contract.ReadyForMutation() {
		return SupervisorDecision{
			Action:   SupAskUser,
			Reason:   "task contract has unresolved material ambiguity",
			Evidence: "contract.ReadyForMutation() = false",
		}
	}
	// 3. Last outcome has a recovery transition available → follow it
	if lastOutcome != nil && !lastOutcome.Success && lastOutcome.Retryable {
		if lastOutcome.AllowsTransition(TransitionRepair) {
			return SupervisorDecision{
				Action:       SupRepair,
				Reason:       "last operation failed with repairable outcome",
				Evidence:     RenderOutcome(*lastOutcome),
				RecoveryHint: "follow the capability's repair automaton",
			}
		}
		if lastOutcome.AllowsTransition(TransitionReconcile) {
			return SupervisorDecision{
				Action:       SupReconcile,
				Reason:       "last operation requires state reconciliation",
				Evidence:     RenderOutcome(*lastOutcome),
				RecoveryHint: "reconcile before retry",
			}
		}
		if lastOutcome.AllowsTransition(TransitionRetry) {
			return SupervisorDecision{
				Action:       SupContinue,
				Reason:       "last operation failed but is retryable",
				Evidence:     RenderOutcome(*lastOutcome),
				RecoveryHint: "retry with the same recovery automaton",
			}
		}
	}
	// 4. Last outcome is terminal failure with no transitions → stop
	if lastOutcome != nil && !lastOutcome.Success && lastOutcome.IsTerminal() {
		termClass := classifyTerminal(lastOutcome)
		return SupervisorDecision{
			Action:   SupStop,
			Terminal: &termClass,
			Reason:   "terminal failure with no recovery transitions",
			Evidence: RenderOutcome(*lastOutcome),
		}
	}
	// 5. Default: continue (model should make progress)
	return SupervisorDecision{
		Action:   SupContinue,
		Reason:   "open work remains, no blocking state",
		Evidence: "verifier graph has open criteria",
	}
}

func classifyTerminal(o *OperationOutcome) TerminalClass {
	if o == nil {
		return TermInternalFailure
	}
	switch o.FailureLayer {
	case LayerAuthorization:
		return TermMissingAuth
	case LayerPolicy:
		return TermMissingAuth
	case LayerCancellation:
		return TermSafePartial
	case LayerTransport, LayerProcess:
		return TermExternalUnavail
	default:
		return TermInternalFailure
	}
}

// RenderTerminalMessage produces user-facing terminal prose from verified state.
// Per O1 req.10 ac.4: terminal communication is generated from verified data
// and accurately distinguishes completed work, residual effects, etc.
func RenderTerminalMessage(dec SupervisorDecision, proof *ProofManifest) string {
	if dec.Terminal == nil {
		return dec.Reason
	}
	switch *dec.Terminal {
	case TermComplete:
		if proof != nil {
			return fmt.Sprintf("Task complete. All %d acceptance criteria are closed with verified evidence.", len(proof.Verifiers))
		}
		return "Task complete."
	case TermMissingAuth:
		return "This operation requires your authorization to proceed. " + dec.Reason
	case TermIrreducibleAmbiguity:
		return "The request contains ambiguity that requires your input to resolve. " + dec.Reason
	case TermExternalUnavail:
		return "An external service or dependency is unavailable. " + dec.Reason
	case TermSafePartial:
		return "The task was safely stopped with partial progress. " + dec.Reason
	case TermInternalFailure:
		return "An internal execution issue prevented completion. " + dec.Reason
	case TermBudgetExhausted:
		return "The execution budget was exhausted before the task could complete. " + dec.Reason
	default:
		return dec.Reason
	}
}
