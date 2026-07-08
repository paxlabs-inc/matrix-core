// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"

	"matrix/neo/internal/memory"
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
		a.consolidateWorking()
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
		a.consolidateWorking()
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
	a.out.Say(answer, completion)
	// [memory.writeback] step_5: consolidate before any compaction nils the
	// working transcript.
	a.consolidateWorking()
	// Attest surfaced memories so cortex salience + EMA learn from what helped
	// vs. what merely crowded the budget. Cheap, best-effort.
	a.attestTurn(ctx, surfaced, surfacedSnips, userInput, answer)
}

// consolidateWorking sweeps the current turn's transcript into cortex (write-
// back option B). Called on EVERY terminal path — success, stall, step-budget
// exhaustion, and model error — so Neo also learns from failed/abandoned turns
// (a failure pattern is durable signal), not only from clean completions.
// Best-effort + idempotent enough: the consolidator de-dupes on write.
func (a *Agent) consolidateWorking() {
	if a.consolidator != nil {
		a.consolidator.Consolidate(renderTranscript(a.working))
	}
}
