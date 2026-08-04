// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// closechain.go — the termination guard chain (MORPHEUS req.4): the bare-answer
// close gauntlet reified as ONE ordered table of named guards, each returning an
// explicit verdict, evaluated at ONE site (closeTurn). The order is a proven
// artifact (the table below IS the order, pinned by a table-driven test), and
// finishTurn stays the single delivery choke point — a guard never delivers
// itself; it verdicts, and closeTurn acts.
package agent

import (
	"context"
	"fmt"

	"matrix/neo/internal/llm"
)

// closeVerdict is the explicit verdict a close guard returns (req.4.1).
type closeVerdict string

const (
	// verdictDeliver ends the turn: closeTurn hands the (possibly rewritten)
	// answer to finishTurn, the single delivery choke point.
	verdictDeliver closeVerdict = "deliver"
	// verdictNudge re-enters the loop after a guidance-channel steer was
	// pushed: the model must address the steer before it may close.
	verdictNudge closeVerdict = "nudge-and-continue"
	// verdictSuppress re-enters the loop WITHOUT a steer: this close attempt
	// is suppressed because the window already changed under the model (a
	// drained inbox message, a Cassandra self-heal fold) and it must re-read.
	verdictSuppress closeVerdict = "suppress"
)

// closeDecision is one fired guard's outcome. err, when non-nil, is a terminal
// escalation that ends the attempt regardless of the verdict.
type closeDecision struct {
	verdict closeVerdict
	err     error
}

// closeContext is the would-be close under evaluation. Guards read it and may
// rewrite answer (the identity scrub); the delivered answer is always
// cc.answer, never re-derived from the result.
type closeContext struct {
	res    *llm.ChatResult
	answer string // trimmed final answer, rewritten in place by the identity guard
}

// closeGuard is one named unit of the chain: eval reports whether the guard
// fires for this close and, when it does, the decision. A guard that does not
// fire must leave the agent and context untouched.
type closeGuard struct {
	name string
	eval func(a *Agent, cc *closeContext) (closeDecision, bool)
}

// closeGuardChain is THE ordered termination guard chain (req.4.1/4.2). The
// table order is load-bearing and preserved verbatim from the sequential
// gauntlet it reifies:
//
//  1. inbox_drain        — a mid-turn user message outranks closing (F5)
//  2. truncated_answer   — a length-cut generation is never a final answer
//  3. empty_answer       — nothing produced: steer to act or answer
//  4. unread_overflow    — the read-full discipline blocks the close (req.4.2)
//  5. source_fetch       — search discovery must be read before factual use
//  6. identity_leak      — the compliance canary: audit, scrub, re-anchor
//  7. deliver            — the terminal guard; always fires
//
// Evaluation happens at exactly ONE site (closeTurn → evalCloseChain); the
// first firing guard decides.
var closeGuardChain = []closeGuard{
	{name: "inbox_drain", eval: guardInboxDrain},
	{name: "truncated_answer", eval: guardTruncatedAnswer},
	{name: "empty_answer", eval: guardEmptyAnswer},
	{name: "unread_overflow", eval: guardUnreadOverflow},
	{name: "source_fetch", eval: guardSourceFetch},
	{name: "identity_leak", eval: guardIdentityLeak},
	{name: "deliver", eval: guardDeliver},
}

// evalCloseChain walks the ordered chain and returns the first firing guard's
// name and decision. The terminal deliver guard always fires, so every
// evaluation produces a decision. Contract — mutates: whatever the ONE firing
// guard mutates (transcript nudges, the unproductive counter, cc.answer);
// non-firing guards are side-effect free.
func (a *Agent) evalCloseChain(cc *closeContext) (string, closeDecision) {
	for _, g := range closeGuardChain {
		if dec, fired := g.eval(a, cc); fired {
			return g.name, dec
		}
	}
	// Unreachable — guardDeliver always fires — kept as a defensive fallthrough.
	return "deliver", closeDecision{verdict: verdictDeliver}
}

// guardInboxDrain: a message queued mid-turn (F5) may have arrived during this
// model call, just as the agent is about to finish. Address it instead of
// closing the turn, so a non-interrupting mid-task message is never dropped at
// the turn boundary. Contract — mutates: the working transcript (appends the
// drained user messages); fires only when at least one message was injected.
func guardInboxDrain(a *Agent, _ *closeContext) (closeDecision, bool) {
	injected, err := a.drainInbox(context.Background())
	if err != nil {
		return closeDecision{verdict: verdictSuppress, err: fmt.Errorf("neo: persist queued user message: %w", err)}, true
	}
	if !injected {
		return closeDecision{}, false
	}
	return closeDecision{verdict: verdictSuppress}, true
}

// guardTruncatedAnswer: a truncated generation (finish_reason=length) is never
// a final answer. It receives exactly one synthesis retry, independent of
// earlier tool-repeat or close-guard pressure. A second truncation ends this
// attempt as resumable so the supervisor can continue from durable state.
func guardTruncatedAnswer(a *Agent, cc *closeContext) (closeDecision, bool) {
	if cc.res.FinishReason != "length" {
		return closeDecision{}, false
	}
	if a.turn.finalSynthesisRetries >= 1 {
		return closeDecision{
			verdict: verdictNudge,
			err:     a.governDeath(DeathReasonSynthesis, "could not finish the final answer after one bounded synthesis retry. Progress so far:"),
		}, true
	}
	a.turn.finalSynthesisRetries++
	// Raise the output-token budget one rung so the regenerated answer has room
	// to finish a long synthesis instead of re-truncating at the same limit
	// (synthesis-survives-death). Bounded by maxAnswerTokenBumps.
	a.bumpAnswerBudget()
	a.pushGuidance("Your last message was cut off by the output limit and was NOT delivered to the user — don't inline large payloads in prose; call a tool with compact arguments, or give a concise final answer.")
	return closeDecision{verdict: verdictNudge}, true
}

// guardEmptyAnswer: empty AND no tools → anti-premature steer to continue via
// the guidance channel, bounded so a model that keeps returning nothing
// escalates rather than looping forever. Its bound is independent of all
// other close guards and tool-repeat detection.
func guardEmptyAnswer(a *Agent, cc *closeContext) (closeDecision, bool) {
	if cc.answer != "" {
		return closeDecision{}, false
	}
	if a.pushGuidanceNudge("Continue: either call a tool to make progress, or give the final answer.", &a.turn.emptyAnswerNudges) {
		return closeDecision{verdict: verdictNudge, err: a.escalateGuidance(a.turn.emptyAnswerNudges)}, true
	}
	return closeDecision{verdict: verdictNudge}, true
}

// overflowNudgeCap bounds how many closes the unread-overflow guard may block
// per turn before it stands down and lets the composed answer deliver.
const overflowNudgeCap = 2

// guardUnreadOverflow: read-full discipline (req.4.2) — a tool result too
// large to show inline was spilled to an overflow file. Don't let the turn end
// on a bare answer while that output is still unread; steer (via the guidance
// channel) to read it first, so the answer can't be drawn from a truncated
// result. The steer is honest about non-delivery (the held-back answer is in
// the window but the user never saw it) and BOUNDED: past overflowNudgeCap the
// guard stands down and the answer delivers — the identity-leak posture. The
// unbounded form drove the 2026-07-22 loopty-loop death spiral: complete
// answers were silently withheld until an unrelated shared cap killed the
// turn. Contract — mutates the working transcript and the per-turn overflow
// nudge counter; fires only while under its own cap.
func guardUnreadOverflow(a *Agent, _ *closeContext) (closeDecision, bool) {
	if !a.overflowUnread() {
		return closeDecision{}, false
	}
	if a.turn.overflowNudges >= overflowNudgeCap {
		return closeDecision{}, false
	}
	a.turn.overflowNudges++
	a.pushGuidance(a.overflowUnreadNudge())
	return closeDecision{verdict: verdictNudge}, true
}

// sourceFetchNudgeCap bounds how many closes the source-fetch guard may block
// per turn before it stands down and lets the composed answer deliver. It
// mirrors overflowNudgeCap for the same reason: the guard's precondition is not
// always ACHIEVABLE. When every fetch of a discovered URL fails (paywall, bot
// wall, dead host), no amount of steering can make webEvidenceReady true, and
// an unbounded guard then withholds a finished answer indefinitely. Standing
// down is honest:
// the model is told the answer was held back and why, so a delivered answer
// carries its own caveat about unread sources.
const sourceFetchNudgeCap = 2

func guardSourceFetch(a *Agent, _ *closeContext) (closeDecision, bool) {
	if a.webEvidenceReady() {
		return closeDecision{}, false
	}
	if a.turn.sourceFetchNudges >= sourceFetchNudgeCap {
		return closeDecision{}, false
	}
	a.turn.sourceFetchNudges++
	a.pushGuidance("Your answer was NOT delivered to the user — it is being held back. Search results are source discovery, not evidence. Read at least one relevance-validated result URL with fetch before making a factual claim; if no relevant result exists, say that plainly without substituting another entity or evidence class. Then give your final answer again.")
	return closeDecision{verdict: verdictNudge}, true
}

// guardIdentityLeak: the identity-compliance canary (P0) — if the settled
// answer broke character and self-identified as the underlying LLM, treat it
// as a charter breach, not a typo: a model that won't hold its own name is
// signalling it may ignore the harder rules too. Surface it on the audit
// side-channel (never blind), scrub the answer, and re-anchor via the guidance
// channel so the model regenerates a compliant answer. Once the nudge budget
// is spent the honest, SCRUBBED answer is delivered rather than looping
// forever (finishTurn re-scrubs at delivery regardless). Contract — mutates:
// cc.answer (the scrub) and the working transcript (guidance); emits the
// identity.leak audit event. Its retry bound is independent.
func guardIdentityLeak(a *Agent, cc *closeContext) (closeDecision, bool) {
	scrubbed, leaked := scrubIdentity(a.agentName(), cc.answer)
	if !leaked {
		return closeDecision{}, false
	}
	a.emitAudit(auditEventIdentityLeak, map[string]interface{}{"where": "answer"})
	cc.answer = scrubbed
	if a.pushGuidanceNudge(identityReanchorNudge(a.agentName()), &a.turn.identityNudges) {
		return closeDecision{verdict: verdictDeliver}, true
	}
	return closeDecision{verdict: verdictNudge}, true
}

// guardDeliver is the terminal guard: nothing blocked the close, so the answer
// ships. It always fires, making the chain total — every evaluation produces a
// decision. Contract — mutates: nothing (delivery itself happens in closeTurn
// via finishTurn, the single choke point).
func guardDeliver(_ *Agent, _ *closeContext) (closeDecision, bool) {
	return closeDecision{verdict: verdictDeliver}, true
}
