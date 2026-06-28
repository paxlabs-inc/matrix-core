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

// Cassandra Phase 1 — the Neo completion gate.
//
// Neo no longer terminates on the mere ABSENCE of further tool calls. A turn
// that took an action / changed state may end only through a validated
// completeness object carried by the synthetic task_complete tool: positive
// proof of completion, never the silence of "no more tools". This file owns the
// deterministic, local stand-in for the Cassandra adjudicator (Phase 3 swaps in
// the full grounded verdict); it checks structure, self-contradiction, the
// coherence guards, and cross-references cited evidence against the working
// transcript (the ground truth — real tool results), never the model grading
// itself. See cassandra.frozen.kvx [thesis], [seams.neo], [verdict],
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
// verdict (with the answer) or a reject verdict (with feedback). The
// stateTouched flag selects the strict path (a full grounded verdict over the
// executed transcript) vs. the light structural path for reversible chat. It
// NEVER blocks the turn on its own error — an unparseable call is a soft reject
// (ask the model to re-issue), and a Cassandra hiccup fails open to the
// deterministic check (i_cass_5).
//
// Phase 3: when a Cassandra adjudicator is wired (a.adjudicator != nil), the
// strict path consults the full shared faculty (grounding + unverified claims +
// open unknowns) instead of the Phase-1 deterministic grounding check, which
// remains the fail-open fallback. userRequest is the contract this turn must
// satisfy.
func (a *Agent) validateCompletion(ctx context.Context, call *llm.ToolCall, strict, highStakes bool, userRequest string) completionVerdict {
	args, err := call.ParseArgs()
	if err != nil {
		return completionVerdict{feedback: fmt.Sprintf("could not parse your task_complete arguments (%v). Re-issue it with valid JSON: summary, coverage (\"full\"|\"partial\"), evidence[], open_gaps[], assumptions[].", err)}
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
	evidence := stringSlice(args["evidence"])
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
	// any substantial deliverable, must back its claims with real executed
	// evidence ([coupling].high_stakes, i_cass_2). Prefer the full Cassandra
	// adjudicator; fall back to the deterministic grounding check when none is
	// wired. highStakes (money/chain) drives the adjudicator's escalation; a
	// reversible-but-substantial turn is still grounded-audited, not escalated.
	if a.adjudicator != nil {
		return a.adjudicateCompletion(ctx, userRequest, summary, coverage, evidence, openGaps, assumptions, highStakes)
	}
	if r := a.deterministicStrict(evidence); !r.ok {
		return r
	}
	return completionVerdict{ok: true, answer: summary}
}

// deterministicStrict is the Phase-1 deterministic grounding check, retained as
// the cheap pre-Cassandra path AND the fail-open fallback when the adjudicator
// is unavailable or errors: a state-touching turn may not finish on no evidence
// or on phantom evidence absent from the executed transcript (g3-analog). On
// pass it returns ok with no answer (the caller carries the summary).
func (a *Agent) deterministicStrict(evidence []string) completionVerdict {
	corpus := a.transcriptToolCorpus()
	if len(evidence) == 0 {
		return completionVerdict{feedback: "you took an action this turn but cited no evidence. List the concrete results that back what you did (tool output, file paths, URLs, transaction hashes) in 'evidence' — or keep working until you actually have them. Do not claim success you cannot show."}
	}
	if phantom := firstUngrounded(evidence, corpus); phantom != "" {
		return completionVerdict{feedback: fmt.Sprintf("the evidence %q doesn't match anything in what you actually did this turn. Cite only real tool results from this turn (no invented evidence), or go obtain it before declaring done.", truncate(phantom, 160))}
	}
	return completionVerdict{ok: true}
}

// adjudicateCompletion runs the full Cassandra adjudicator over the executed
// transcript and maps the verdict to the agent's next action (cassandra.frozen.kvx
// [seams.neo], [coupling]). The deterministic priors run first as the cheap
// pre-pass ([adjudicator].priors); the auditor renders the grounded verdict.
//
// Mapping (advisor-not-effector — the agent enacts it):
//   - grounded AND (claimed partial OR Cassandra-coverage full) → finish (Say),
//     surfacing any flagged unknowns to the user ([ux]).
//   - otherwise → tool-result feedback enumerating missing / unverified_claims /
//     open_unknowns → the loop continues.
//
// Honest partials are preserved (i_cass_1): a turn the agent itself declares
// partial finishes as a partial, provided its claims are grounded — Cassandra
// blocks false SUCCESS, not honest incompleteness.
func (a *Agent) adjudicateCompletion(ctx context.Context, userRequest, summary, coverage string, evidence, openGaps, assumptions []string, highStakes bool) completionVerdict {
	digest := a.buildEvidenceDigest()
	priors := cassandra.ScanPriors(cassandra.PriorInput{Evidence: digest})
	a.emitAudit(auditEventAudit, map[string]interface{}{
		"stage":          "started",
		"high_stakes":    highStakes,
		"priors_flagged": priors.FlagsCompletionRisk(),
	})

	v, err := a.adjudicator.Adjudicate(ctx, cassandra.AuditInput{
		Request:    buildAuditContract(userRequest, summary, coverage, evidence, openGaps, assumptions),
		Evidence:   digest,
		HighStakes: highStakes,
		Priors:     priors,
	})
	if err != nil {
		// Fail-open (i_cass_5): a Cassandra hiccup must never break the turn.
		// Fall back to the deterministic grounding check so a state-touching
		// turn still cannot finish on fabricated evidence.
		a.emitAudit(auditEventAudit, map[string]interface{}{"stage": "error", "error": err.Error(), "fallback": "deterministic"})
		if r := a.deterministicStrict(evidence); !r.ok {
			return r
		}
		return completionVerdict{ok: true, answer: summary}
	}

	// g3 backstop ([verdict.coherence_guards]): a cited ref absent from the
	// digest is phantom evidence — forces grounded=false.
	phantom := v.CheckCitations(evidence, digest)
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

	if verdictAccepts(coverage, v, phantom) {
		// F4: the delivered + persisted answer is the CLEAN deliverable only.
		// Any Cassandra-flagged caveats ride the cassandra.* side-channel as a
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
		"phantom":           phantom,
	})
	return completionVerdict{feedback: continueFeedback(v, phantom)}
}

// transcriptToolCorpus concatenates this turn's real tool results — the ground
// truth against which cited evidence is checked. Lower-cased for case-tolerant
// matching.
func (a *Agent) transcriptToolCorpus() string {
	var b strings.Builder
	for _, m := range a.working {
		if m.Role == llm.RoleTool {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return strings.ToLower(b.String())
}

// firstUngrounded returns the first evidence item that has no support in the
// tool corpus, or "" when every item is grounded. Grounding centers on
// FACT-BEARING tokens — digit-bearing identifiers (block numbers, amounts, tx
// hashes, ids) are the hallucination surface that matters (e.g. a fabricated
// "block height 123456"). The rule: if an item cites any fact-bearing token,
// at least ONE of them must appear in the executed transcript, else the item
// is phantom evidence (coherence guard g3-analog). Items with no fact-bearing
// token fall back to a forgiving any-salient-token match (fail-open bias,
// i_cass_5) so prose-only evidence isn't falsely rejected.
func firstUngrounded(evidence []string, corpus string) string {
	for _, e := range evidence {
		item := strings.ToLower(strings.TrimSpace(e))
		if item == "" {
			continue
		}
		if strings.Contains(corpus, item) {
			continue
		}
		tokens := salientTokens(item)
		if len(tokens) == 0 {
			continue // ungroundable shape — don't penalize
		}
		strong := factBearing(tokens)
		if len(strong) > 0 {
			if !anyContained(corpus, strong) {
				return e // cited a concrete fact absent from the transcript
			}
			continue
		}
		// No concrete fact cited: forgiving match on any salient token.
		if !anyContained(corpus, tokens) {
			return e
		}
	}
	return ""
}

// salientTokens extracts lower-cased alphanumeric tokens of length >=4 from s —
// the high-signal substrings (identifiers, hashes, paths, numbers) worth
// matching against the transcript.
func salientTokens(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 4 {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// factBearing keeps the tokens that carry a concrete fact: those containing a
// digit (block numbers, amounts, ids, 0x-prefixed hashes). These are the
// claims most prone to fabrication and are checked strictly.
func factBearing(tokens []string) []string {
	var out []string
	for _, tk := range tokens {
		if strings.ContainsAny(tk, "0123456789") {
			out = append(out, tk)
		}
	}
	return out
}

// anyContained reports whether the corpus contains any of the tokens.
func anyContained(corpus string, tokens []string) bool {
	for _, tk := range tokens {
		if strings.Contains(corpus, tk) {
			return true
		}
	}
	return false
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
