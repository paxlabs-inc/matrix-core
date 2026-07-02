// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"strings"

	"matrix/cassandra"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// Cassandra — the Neo completion gate.
//
// Neo no longer terminates on the mere ABSENCE of further tool calls. A turn
// that took an action / changed state may end only through the synthetic
// task_complete tool: positive proof of completion, never the silence of "no
// more tools". Neo proves completion by COMPLETING, not by citing evidence — it
// declares the OUTCOME it is returning to the user (summary + honest coverage),
// and Cassandra independently checks that outcome against the task GOAL over the
// real executed transcript (the ground truth — real tool results), never the
// model grading itself and never by matching the words the model happened to
// type. See cassandra.frozen.kvx [thesis], [seams.neo], [verdict],
// [verdict.coherence_guards], invariants i_cass_1/2/3/5/7.

// completionVerdict is the result of adjudicating a task_complete call. On
// accept it carries the user-facing answer; on reject it carries actionable
// feedback fed back to the model as the tool result so the loop continues.
type completionVerdict struct {
	ok       bool
	answer   string
	feedback string
}

// splitCompletion separates a task_complete call from any sibling tool calls in
// the same assistant turn. Returns (nil, calls) when no completion was
// requested. Only the FIRST task_complete is treated as the gate; further ones
// (models rarely emit them) fall into rest and are answered benignly.
func splitCompletion(calls []llm.ToolCall) (*llm.ToolCall, []llm.ToolCall) {
	var cc *llm.ToolCall
	rest := make([]llm.ToolCall, 0, len(calls))
	for i := range calls {
		if cc == nil && calls[i].Function.Name == tools.TaskCompleteTool {
			c := calls[i]
			cc = &c
			continue
		}
		rest = append(rest, calls[i])
	}
	return cc, rest
}

// batchTouchesState reports whether a tool-call batch reaches the irreversible
// seam — core_execute (the MCL pipeline: spend / sign / deploy / settle). The
// reversible Natural toolset (shell, files, browser, web, git) is NOT treated
// as state-touching here: per the spec's placement-by-reversibility, those
// turns ride the light path so reversible work stays frictionless. Only the
// money/chain seam forces a full grounded completion object.
func batchTouchesState(calls []llm.ToolCall) bool {
	for i := range calls {
		if calls[i].Function.Name == tools.CoreExecuteTool {
			return true
		}
	}
	return false
}

// validateCompletion adjudicates a task_complete call. It returns an accept
// verdict (with the answer) or a reject verdict (with feedback). The strict
// flag selects the goal-vs-outcome adjudication path (a state-touching turn, or
// any substantial deliverable under GateAllWork) vs. the light structural path
// for reversible chat. It NEVER blocks the turn on its own error — an
// unparseable call is a soft reject (ask the model to re-issue), and a Cassandra
// hiccup fails open (i_cass_5).
//
// Neo proves completion by COMPLETING, not by citing evidence: it declares the
// outcome (summary + honest coverage) and Cassandra checks that outcome against
// the task GOAL over the real executed transcript. When no adjudicator is wired
// (CLI / tests) the strict path fails open once the object is coherent — there
// is no local word-matching stand-in (that mechanism was meaningless).
func (a *Agent) validateCompletion(ctx context.Context, call *llm.ToolCall, strict, highStakes bool, goal string) completionVerdict {
	args, err := call.ParseArgs()
	if err != nil {
		return completionVerdict{feedback: fmt.Sprintf("could not parse your task_complete arguments (%v). Re-issue it with valid JSON: summary, coverage (\"full\"|\"partial\"), open_gaps[], assumptions[].", err)}
	}

	summary := strings.TrimSpace(stringArg(args["summary"]))
	if summary == "" {
		return completionVerdict{feedback: "task_complete needs a non-empty 'summary' — the final answer the user should read. Either provide it, or keep working if you're not done."}
	}

	coverage := strings.ToLower(strings.TrimSpace(stringArg(args["coverage"])))
	if coverage != "full" && coverage != "partial" {
		return completionVerdict{feedback: "task_complete needs 'coverage' set to \"full\" (every requested deliverable produced) or \"partial\" (something is still unresolved)."}
	}

	openGaps := stringSlice(args["open_gaps"])
	assumptions := stringSlice(args["assumptions"])

	// Coherence guard g1 ([verdict.coherence_guards]): "full" coverage with a
	// non-empty open-gaps list is incoherent — resolve toward MORE work, never
	// a false success (i_cass_7).
	if coverage == "full" && len(openGaps) > 0 {
		return completionVerdict{feedback: "you set coverage=\"full\" but listed open gaps: " + joinGaps(openGaps) + ". Either finish those items and re-call task_complete, or set coverage=\"partial\" and tell the user plainly what remains unresolved."}
	}

	// Light path: a pure conversational turn (no substantive tool work) carries
	// no false-alarm tax — defer to the agent once the object is structurally
	// coherent ([coupling].reversible, placement-by-reversibility, i_cass_5).
	if !strict {
		return completionVerdict{ok: true, answer: summary}
	}

	// Strict path: a state-touching (money/chain) turn, OR — under GateAllWork —
	// any substantial deliverable, has its OUTCOME checked against the GOAL by
	// the Cassandra adjudicator over the executed transcript ([coupling].high_stakes,
	// i_cass_2). highStakes (money/chain) drives the adjudicator's escalation. With
	// no adjudicator wired the strict path FAILS OPEN (i_cass_5): we defer to the
	// agent's honest outcome rather than fall back to meaningless word-matching.
	if a.adjudicator != nil {
		return a.adjudicateCompletion(ctx, goal, summary, coverage, openGaps, assumptions, highStakes)
	}
	return completionVerdict{ok: true, answer: summary}
}

// adjudicateCompletion runs the Cassandra adjudicator: it checks whether the
// OUTCOME Neo is about to return to the user actually satisfies the task's GOAL,
// judged against the real executed transcript (the ground truth — real tool
// results), and maps the verdict to the agent's next action (cassandra.frozen.kvx
// [seams.neo], [coupling]). Neo cites no evidence and proves nothing by hand:
// completion is demonstrated by the transcript, not by matching the words it
// typed. The deterministic priors run first as the cheap pre-pass
// ([adjudicator].priors); the auditor renders the grounded verdict.
//
// Mapping (advisor-not-effector — the agent enacts it):
//   - the outcome satisfies the goal (grounded AND (claimed partial OR
//     Cassandra-coverage full)) → finish (Say), surfacing any flagged unknowns
//     to the user ([ux]).
//   - otherwise → feedback naming what is missing / unverified / still open so
//     Neo can fix it → the loop continues.
//
// Honest partials are preserved (i_cass_1): a turn the agent itself declares
// partial finishes as a partial, provided its outcome matches the transcript —
// Cassandra blocks false SUCCESS, not honest incompleteness. On any adjudicator
// error the turn FAILS OPEN (i_cass_5): a Cassandra hiccup never wedges a turn.
func (a *Agent) adjudicateCompletion(ctx context.Context, goal, summary, coverage string, openGaps, assumptions []string, highStakes bool) completionVerdict {
	digest := a.buildEvidenceDigest()
	priors := cassandra.ScanPriors(cassandra.PriorInput{Evidence: digest})
	a.emitAudit(auditEventAudit, map[string]interface{}{
		"stage":          "started",
		"high_stakes":    highStakes,
		"priors_flagged": priors.FlagsCompletionRisk(),
	})

	v, err := a.adjudicator.Adjudicate(ctx, cassandra.AuditInput{
		Request:    buildAuditContract(goal, summary, coverage, openGaps, assumptions),
		Evidence:   digest,
		HighStakes: highStakes,
		Priors:     priors,
	})
	if err != nil {
		// Fail-open (i_cass_5): a Cassandra hiccup must never break the turn.
		// Completion is verified by the adjudicator against the transcript, not
		// by any local word-matching, so with the adjudicator down we defer to
		// the agent's own honest outcome rather than block it.
		a.emitAudit(auditEventAudit, map[string]interface{}{"stage": "error", "error": err.Error(), "fallback": "fail_open"})
		return completionVerdict{ok: true, answer: strings.TrimSpace(summary)}
	}

	a.emitAudit(auditEventVerdict, map[string]interface{}{
		"grounded":          v.Grounded,
		"coverage":          string(v.Coverage),
		"missing":           v.Missing,
		"unverified_claims": v.UnverifiedClaims,
		"open_unknowns":     v.OpenUnknowns,
		"assumptions":       v.Assumptions,
		"certainty":         v.Certainty,
		"rationale":         v.Rationale,
	})

	if verdictAccepts(coverage, v) {
		// F4: the delivered + persisted answer is the CLEAN outcome only. Any
		// Cassandra-flagged caveats ride the cassandra.* side-channel as a
		// separate subtle affordance — never folded into the answer text
		// (ux_truth). i6 honest-partials are intact: the agent's own summary
		// already states what a partial left unresolved.
		caveats := cappedUnknowns(v)
		if coverage == "partial" {
			a.emitAudit(auditEventPartial, map[string]interface{}{"rationale": v.Rationale, "open_unknowns": caveats})
		} else {
			a.emitAudit(auditEventGate, map[string]interface{}{"decision": "finish", "certainty": v.Certainty, "open_unknowns": caveats})
		}
		return completionVerdict{ok: true, answer: strings.TrimSpace(summary)}
	}

	a.emitAudit(auditEventContinue, map[string]interface{}{
		"missing":           v.Missing,
		"unverified_claims": v.UnverifiedClaims,
		"open_unknowns":     v.OpenUnknowns,
	})
	return completionVerdict{feedback: continueFeedback(v)}
}

// stringArg coerces a loose JSON value to a trimmed string.
func stringArg(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

// stringSlice coerces a loose JSON array (or a single string) into a cleaned
// []string, dropping empties. Tolerates the shapes models actually emit.
func stringSlice(v interface{}) []string {
	switch t := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := strings.TrimSpace(stringArg(e)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := strings.TrimSpace(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	}
	return nil
}

func joinGaps(gaps []string) string {
	cleaned := make([]string, 0, len(gaps))
	for _, g := range gaps {
		if s := strings.TrimSpace(g); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	return strings.Join(cleaned, "; ")
}

// finishTurn performs the shared end-of-turn bookkeeping for an accepted
// completion (gate or light path): surface the answer, hand the transcript to
// background consolidation, and attest the surfaced memories. The caller still
// owns the post-finish soft-compaction check (it needs the live build inputs).
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
