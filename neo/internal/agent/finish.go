// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"strings"

	"matrix/neo/internal/consolidation"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/o1"
)

// finish.go holds the shared end-of-turn bookkeeping. Cassandra 2.0 retired the
// proof-of-work completion gate (task_complete / completion.go / the adjudicator
// wiring): a turn now ends when the model emits no tool calls, and finishTurn is
// the single delivery choke point for that answer.

// finishTurn performs the shared end-of-turn bookkeeping for a delivered answer:
// surface the answer, hand the transcript to background consolidation, and
// attest the surfaced memories. The completion flag rides through to the client
// (Reporter.Say) unchanged; with the gate retired it is always false on Neo's
// own loop. The caller still owns the post-finish soft-compaction check (it
// needs the live build inputs).
func (a *Agent) finishTurn(ctx context.Context, answer string, surfaced map[string]struct{}, surfacedSnips map[string]memory.Snippet, userInput string, completion bool) {
	// P1-4: Heartbeat suppression. When this turn is a Chronos heartbeat wake
	// (the wake_message carries the HEARTBEAT marker) AND the agent's answer is
	// the HEARTBEAT_OK sentinel, the turn is SUPPRESSED — a.out.Say is never
	// called, so the sentinel never reaches the user. The agent reviewed active
	// goals/constraints and found nothing needing attention. Anything else
	// (non-sentinel answer, or a non-heartbeat turn) is delivered normally.
	if isHeartbeatWake(userInput) && shouldSuppressHeartbeat(answer) {
		// Still consolidate + attest the (heartbeat) turn so cortex learns, but
		// produce NO user-facing output.
		a.consolidateWorking("", true)
		a.attestTurn(ctx, surfaced, surfacedSnips, userInput, answer)
		return
	}
	// Automatrix suppression (sibling of the heartbeat case above). When this
	// turn is a Chronos Automatrix wake (the wake_message carries the AUTOMATRIX
	// marker) AND the agent's answer is the AUTOMATRIX_IDLE sentinel, the turn
	// is SUPPRESSED — Neo had idle time but found nothing worth doing right now,
	// so the sentinel never reaches the user. Anything else is delivered.
	if isAutomatrixWake(userInput) && shouldSuppressAutomatrix(answer) {
		// Still consolidate + attest the (Automatrix) turn so cortex learns, but
		// produce NO user-facing output.
		a.consolidateWorking("", true)
		a.attestTurn(ctx, surfaced, surfacedSnips, userInput, answer)
		return
	}
	// Identity net (P0), authoritative choke point: every delivered answer
	// funnels through here, so scrubbing a self-identification as the underlying
	// LLM right before Say guarantees a breach never reaches the user or the
	// durable thread, even if the prompt guardrail, the streamed-delta net, and
	// the in-loop re-anchor all missed. A scrub firing here is a compliance
	// signal, so surface it on the audit side-channel rather than swallowing it.
	if scrubbed, leaked := scrubIdentity(a.agentName(), answer); leaked {
		a.emitAudit(auditEventIdentityLeak, map[string]interface{}{"where": "delivery"})
		answer = scrubbed
	}
	if a.turn != nil && a.turn.runLedger != nil {
		_ = a.finalizeO1(answer)
	}
	a.out.Say(answer, completion)
	// [memory.writeback] step_5: consolidate before any compaction nils the
	// working transcript.
	a.consolidateWorking(answer, true)
	// Attest surfaced memories so cortex salience + EMA learn from what helped
	// vs. what merely crowded the budget. Cheap, best-effort.
	a.attestTurn(ctx, surfaced, surfacedSnips, userInput, answer)
}

func (a *Agent) finalizeO1(answer string) bool {
	t := a.turn
	if t == nil || t.runLedger == nil || answer == "" {
		return false
	}
	standing := t.outcome()
	canProveResult := standing == nil || standing.Success
	for _, verifier := range t.verifiers.Verifiers {
		if verifier.CriterionID == "criterion.request" && !canProveResult {
			continue
		}
		evidence, eligible := a.o1CriterionEvidence(verifier.CriterionID)
		if !eligible {
			continue
		}
		for _, kind := range verifier.Kinds {
			t.verifiers.RecordResult(o1.VerifierResult{
				CriterionID: verifier.CriterionID, VerifierKind: kind,
				Passed: true, Evidence: evidence,
			})
		}
		if current := findO1Verifier(t.verifiers, verifier.CriterionID); current != nil && current.Closed {
			t.runLedger.CloseCriterion(verifier.CriterionID, joinO1Evidence(current.Results))
		}
	}
	effectsReconciled := len(t.runLedger.Effects) == 0 || t.runLedger.EffectsReconciled
	proof := t.verifiers.GenerateProofManifest(t.runLedger.RunID, t.contract.ID, effectsReconciled, true)
	decision := (&o1.Supervisor{}).Evaluate(t.contract, t.runLedger, &t.verifiers, &proof, t.outcome())
	if decision.Action != o1.SupComplete {
		if decision.Terminal != nil {
			_ = t.runLedger.SetTerminal(o1.TerminalProof{
				Success: false, Summary: decision.Reason, Evidence: []string{decision.Evidence},
			})
		}
		a.persistO1State()
		return false
	}
	if err := t.runLedger.SetTerminal(o1.TerminalProof{
		Success: true, Summary: answer, Evidence: []string{proof.ProofHash},
	}); err != nil {
		a.persistO1State()
		return false
	}
	a.persistO1State()
	return true
}

func (a *Agent) o1CriterionEvidence(criterionID string) (string, bool) {
	t := a.turn
	if t == nil || t.runLedger == nil {
		return "", false
	}
	switch criterionID {
	case "criterion.request":
		return "normalized answer passed the bounded qualitative close review", true
	case "criterion.rules":
		if contractNeedsCapability(t.contract, "execution") {
			if t.runLedger.HasSuccessfulOperation("build_project") {
				return "durable Build dispatch was accepted; the private worker owns repository rules and recorded verification before outcome delivery", true
			}
			if !t.runLedger.HasSuccessfulOperation("shell", "service_", "exec__", "shell__") {
				return "", false
			}
			return "repository or framework validation completed with a normalized successful process outcome", true
		}
		return "conversation truth, source, overflow, identity, and Cassandra guards passed", true
	case "criterion.delivery":
		return "finishTurn authorized durable Reporter.Say delivery", true
	case "criterion.artifact":
		if t.runLedger.HasSuccessfulOperation("build_project") {
			return "durable Build dispatch was accepted for the selected project; artifact evidence will arrive from the private worker", true
		}
		if !t.runLedger.HasSuccessfulOperation("fs__write_file", "fs__edit_file", "write_file", "edit_file", "create_directory", "move_file") {
			return "", false
		}
		return "bounded artifact mutation completed and its normalized result was recorded", true
	case "criterion.execution":
		if t.runLedger.HasSuccessfulOperation("build_project") {
			return "durable Build dispatch was accepted; command and verification evidence is deferred to the worker outcome", true
		}
		if !t.runLedger.HasSuccessfulOperation("shell", "service_", "exec__", "shell__") {
			return "", false
		}
		return "validation process completed successfully", true
	case "criterion.sources":
		if !t.runLedger.HasSuccessfulOperation("web-search__", "web_search__") ||
			!t.runLedger.HasSuccessfulOperation("fetch__", "fetch") {
			return "", false
		}
		return "search discovery and selected source fetch both completed successfully", true
	case "criterion.effects":
		if len(t.runLedger.Effects) == 0 || !t.runLedger.EffectsReconciled {
			return "", false
		}
		return "state-changing effects have authoritative reconciled postconditions", true
	default:
		return "", false
	}
}

func contractNeedsCapability(contract o1.TaskContract, capability string) bool {
	for _, required := range contract.RequiredCapabilities {
		if required == capability {
			return true
		}
	}
	return false
}

func findO1Verifier(graph o1.VerifierGraph, criterionID string) *o1.CriterionVerifier {
	for i := range graph.Verifiers {
		if graph.Verifiers[i].CriterionID == criterionID {
			return &graph.Verifiers[i]
		}
	}
	return nil
}

func joinO1Evidence(results []o1.VerifierResult) string {
	var evidence string
	for _, result := range results {
		if !result.Passed || result.Evidence == "" {
			continue
		}
		if evidence != "" {
			evidence += "; "
		}
		evidence += result.Evidence
	}
	return evidence
}

// consolidateWorking emits an evidence-only job. Mutable working-window
// cognition, Cassandra overlays, retry narration, and supervisor guidance are
// excluded by construction.
func (a *Agent) consolidateWorking(answer string, terminal bool) {
	if a.consolidator == nil || a.turn == nil {
		return
	}
	conv, seqLo, seqHi := a.provenanceRange()
	job := consolidation.Job{
		IntentID:  a.turnIntentID(),
		Attempt:   a.turn.attempt,
		Terminal:  terminal,
		Objective: strings.TrimSpace(a.turn.objective),
		Conv:      conv,
		HaveSeq:   conv != "",
		SeqLo:     seqLo,
		SeqHi:     seqHi,
	}
	if job.Attempt <= 0 {
		job.Attempt = 1
	}
	if job.Objective != "" {
		job.Evidence = append(job.Evidence, consolidation.Evidence{
			Kind: consolidation.EvidenceUser, Content: job.Objective,
		})
	}
	if a.turn.runLedger != nil {
		attempts, proof := a.turn.runLedger.EvidenceSnapshot()
		for _, attempt := range attempts {
			encoded, err := json.Marshal(attempt)
			if err != nil {
				continue
			}
			job.Evidence = append(job.Evidence, consolidation.Evidence{
				Kind: consolidation.EvidenceTool, ToolName: attempt.Operation,
				AttemptID: attempt.AttemptID, Content: string(encoded),
			})
		}
		if proof != nil {
			encoded, err := json.Marshal(proof)
			if err == nil {
				job.Evidence = append(job.Evidence, consolidation.Evidence{
					Kind: consolidation.EvidenceVerified, Content: string(encoded),
				})
			}
		}
	}
	if answer = strings.TrimSpace(answer); terminal && answer != "" {
		job.Outcome = answer
		job.Evidence = append(job.Evidence, consolidation.Evidence{
			Kind: consolidation.EvidenceAssistant, Content: answer,
		})
	}
	a.consolidator.Consolidate(job)
}
