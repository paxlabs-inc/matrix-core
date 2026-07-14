// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// closechain.go — the termination guard chain (MORPHEUS req.4): the bare-answer
// close gauntlet reified as ONE ordered table of named guards, each returning an
// explicit verdict, evaluated at ONE site (closeTurn). The order is a proven
// artifact (the table below IS the order, pinned by a table-driven test), and
// finishTurn stays the single delivery choke point — a guard never delivers
// itself; it verdicts, and closeTurn acts.
package agent

import "matrix/neo/internal/llm"

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
// escalation (the unified unproductive-attempt bound was exceeded) that ends
// the turn regardless of the verdict — escalateGuidance already consolidated
// and recorded the death.
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
	casMod bool   // Cassandra folded doubt into this step's assistant message
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
//  5. identity_leak      — the compliance canary: audit, scrub, re-anchor
//  6. cassandra_self_heal— folded doubt must be re-read before delivering
//  7. deliver            — the terminal guard; always fires
//
// Evaluation happens at exactly ONE site (closeTurn → evalCloseChain); the
// first firing guard decides.
var closeGuardChain = []closeGuard{
	{name: "inbox_drain", eval: guardInboxDrain},
	{name: "truncated_answer", eval: guardTruncatedAnswer},
	{name: "empty_answer", eval: guardEmptyAnswer},
	{name: "unread_overflow", eval: guardUnreadOverflow},
	{name: "identity_leak", eval: guardIdentityLeak},
	{name: "cassandra_self_heal", eval: guardCassandraSelfHeal},
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
	if !a.drainInbox() {
		return closeDecision{}, false
	}
	return closeDecision{verdict: verdictSuppress}, true
}

// guardTruncatedAnswer: a truncated generation (finish_reason=length) is NEVER
// a final answer — the cut-off text is half-formed monologue/payload (the
// model may have been inlining a large blob) and saying it raw leaks internal
// thoughts into the chat. Nudge for a compact retry via the guidance channel,
// bounded so a model that keeps getting cut off escalates rather than
// re-nudging forever. Contract — mutates: the working transcript (guidance)
// and the unified unproductive counter.
func guardTruncatedAnswer(a *Agent, cc *closeContext) (closeDecision, bool) {
	if cc.res.FinishReason != "length" {
		return closeDecision{}, false
	}
	if a.pushGuidanceNudge("Your last message was cut off by the output limit — don't inline large payloads in prose; call a tool with compact arguments, or give a concise final answer.", &a.turn.unproductive) {
		return closeDecision{verdict: verdictNudge, err: a.escalateGuidance(a.turn.unproductive)}, true
	}
	return closeDecision{verdict: verdictNudge}, true
}

// guardEmptyAnswer: empty AND no tools → anti-premature steer to continue via
// the guidance channel, bounded so a model that keeps returning nothing
// escalates rather than looping forever. Contract — mutates: the working
// transcript (guidance) and the unified unproductive counter.
func guardEmptyAnswer(a *Agent, cc *closeContext) (closeDecision, bool) {
	if cc.answer != "" {
		return closeDecision{}, false
	}
	if a.pushGuidanceNudge("Continue: either call a tool to make progress, or give the final answer.", &a.turn.unproductive) {
		return closeDecision{verdict: verdictNudge, err: a.escalateGuidance(a.turn.unproductive)}, true
	}
	return closeDecision{verdict: verdictNudge}, true
}

// guardUnreadOverflow: read-full discipline (req.4.2) — a tool result too
// large to show inline was spilled to an overflow file. Don't let the turn end
// on a bare answer while that output is still unread; steer (via the guidance
// channel) to read it first, so the answer can't be drawn from a truncated
// result. Contract — mutates: the working transcript (guidance) and the
// unified unproductive counter.
func guardUnreadOverflow(a *Agent, _ *closeContext) (closeDecision, bool) {
	if !a.overflowUnread() {
		return closeDecision{}, false
	}
	if a.pushGuidanceNudge(a.overflowUnreadNudge(), &a.turn.unproductive) {
		return closeDecision{verdict: verdictNudge, err: a.escalateGuidance(a.turn.unproductive)}, true
	}
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
// cc.answer (the scrub), the working transcript (guidance), and the unified
// unproductive counter; emits the identity.leak audit event.
func guardIdentityLeak(a *Agent, cc *closeContext) (closeDecision, bool) {
	scrubbed, leaked := scrubIdentity(a.agentName(), cc.answer)
	if !leaked {
		return closeDecision{}, false
	}
	a.emitAudit(auditEventIdentityLeak, map[string]interface{}{"where": "answer"})
	cc.answer = scrubbed
	if a.pushGuidanceNudge(identityReanchorNudge(a.agentName()), &a.turn.unproductive) {
		return closeDecision{verdict: verdictDeliver}, true
	}
	return closeDecision{verdict: verdictNudge}, true
}

// guardCassandraSelfHeal: Cassandra 2.0 self-heal on a would-be close
// (req.6.2) — if the controller folded doubt into this bare answer (premature/
// unverified close, or a close that bailed out of a loop), re-loop so the
// model re-reads its OWN doubt and re-verifies instead of finishing. The
// delivered answer is untouched (finishTurn reads the result copy, separate
// from the edited a.working entry); it is bounded by the per-turn mod cap +
// per-trigger cooldown so it can never loop forever, and never fabricates a
// completion — it only asks the agent to check. Contract — mutates: nothing.
func guardCassandraSelfHeal(_ *Agent, cc *closeContext) (closeDecision, bool) {
	if !cc.casMod {
		return closeDecision{}, false
	}
	return closeDecision{verdict: verdictSuppress}, true
}

// guardDeliver is the terminal guard: nothing blocked the close, so the answer
// ships. It always fires, making the chain total — every evaluation produces a
// decision. Contract — mutates: nothing (delivery itself happens in closeTurn
// via finishTurn, the single choke point).
func guardDeliver(_ *Agent, _ *closeContext) (closeDecision, bool) {
	return closeDecision{verdict: verdictDeliver}, true
}
