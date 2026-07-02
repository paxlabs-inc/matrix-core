// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"

	"matrix/cassandra"
	"matrix/neo/internal/llm"
)

// cassandra.go — Cassandra wiring for the Neo completion gate.
//
// The shared Cassandra adjudicator (matrix/cassandra) judges state-touching (or,
// under GateAllWork, substantial) turns: it checks whether the OUTCOME Neo is
// about to return to the user satisfies the task GOAL, over the executed
// transcript (ground truth), covering deliverable coverage, grounding,
// unverified claims, assumptions, and open unknowns. Neo cites no evidence and
// nothing is decided by matching the words it typed. The deterministic priors
// remain the cheap pre-pass; a Cassandra hiccup FAILS OPEN so it never breaks
// the turn (cassandra.frozen.kvx [seams.neo], [adjudicator], [coupling], [ux],
// [audit], i_cass_1/4/5).

// Cassandra audit event types streamed onto the run's event stream
// (cassandra.frozen.kvx [audit].events). Pure observability side-channel: the
// adjudicator signs nothing and writes no cortex on the happy path (i_cass_4,
// i_cass_6).
const (
	auditEventGoal     = "cassandra.goal"     // the task goal, logged quietly at dispatch
	auditEventAudit    = "cassandra.audit"    // an audit began / errored
	auditEventVerdict  = "cassandra.verdict"  // the rendered verdict object
	auditEventContinue = "cassandra.continue" // ungrounded/incomplete → loop with feedback
	auditEventPartial  = "cassandra.partial"  // honest partial accepted
	auditEventGate     = "cassandra.gate"     // grounded+full → finish
)

// AuditEvent is one Cassandra audit observation surfaced to the harness so the
// product can SHOW the diligence in human terms ([ux]) without leaking the
// mechanism. Optional; a nil AuditObserver discards them (CLI / tests).
type AuditEvent struct {
	Type   string
	Fields map[string]interface{}
}

// AuditObserver receives every Cassandra audit event as it happens. nil
// disables surfacing; the agent loop stays oblivious to the presentation layer
// (mirrors ToolObserver).
type AuditObserver func(AuditEvent)

func (a *Agent) emitAudit(typ string, fields map[string]interface{}) {
	if a.auditObserver == nil {
		return
	}
	a.auditObserver(AuditEvent{Type: typ, Fields: fields})
}

// llmDecoder adapts Neo's function-calling llm.Client to cassandra.Decoder: a
// single auditor round-trip (system + user prompt in, raw model text out, no
// tools). The neo client already strips reasoning / inline <think> and folds
// the streamed turn, so the returned content is the bare verdict text that
// cassandra.ParseVerdict extracts the JSON object from.
type llmDecoder struct{ client *llm.Client }

// NewLLMDecoder wraps a Neo llm.Client as a cassandra.Decoder so the wiring can
// build a cassandra.Adjudicator over a dedicated cassandra-slot client without
// the cassandra module importing neo/llm.
func NewLLMDecoder(c *llm.Client) cassandra.Decoder { return llmDecoder{client: c} }

func (d llmDecoder) Decode(ctx context.Context, system, user string) (string, error) {
	res, err := d.client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{llm.SystemMessage(system), llm.UserMessage(user)},
	})
	if err != nil {
		return "", err
	}
	return res.Message.Content, nil
}

// maxEvidenceDigestChars bounds the executed-transcript digest handed to the
// auditor so a giant tool result can't blow the auditor's request body.
const maxEvidenceDigestChars = 16000

// buildEvidenceDigest renders THIS turn's real tool results — the ground truth
// the agent's completion claim is audited against ([adjudicator].digest,
// i_cass_2). Only tool-role messages are included (the agent's own narration is
// the CLAIM, folded into the audit contract instead, never the evidence).
func (a *Agent) buildEvidenceDigest() string {
	var b strings.Builder
	for _, m := range a.working {
		if m.Role != llm.RoleTool {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = "tool"
		}
		b.WriteString("[")
		b.WriteString(name)
		b.WriteString("]\n")
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteString("\n\n")
	}
	s := strings.TrimSpace(b.String())
	if len(s) > maxEvidenceDigestChars {
		head := maxEvidenceDigestChars * 3 / 4
		tail := maxEvidenceDigestChars - head
		s = s[:head] + "\n…(evidence digest truncated)…\n" + s[len(s)-tail:]
	}
	return s
}

// buildAuditContract folds the task GOAL together with the OUTCOME Neo is about
// to return to the user, so the auditor can check whether that outcome actually
// satisfies the goal over the executed transcript ([verdict.fields] — coverage +
// the unverified-claims hallucination surface). Neo cites no evidence; the
// transcript (AuditInput.Evidence) is the ground truth, so completion is proven
// by what actually ran, never by matching the words the agent typed.
func buildAuditContract(goal, summary, coverage string, openGaps, assumptions []string) string {
	var b strings.Builder
	b.WriteString("== THE GOAL (the task Neo was given) ==\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\n--- THE OUTCOME NEO IS ABOUT TO RETURN TO THE USER ---\n")
	b.WriteString("Claimed coverage: ")
	b.WriteString(coverage)
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(summary))
	if len(openGaps) > 0 {
		b.WriteString("\nThe agent admits these open items: ")
		b.WriteString(strings.Join(openGaps, "; "))
	}
	if len(assumptions) > 0 {
		b.WriteString("\nThe agent made these assumptions: ")
		b.WriteString(strings.Join(assumptions, "; "))
	}
	b.WriteString("\n\nDecide whether this outcome satisfies the goal, judged ONLY against the executed transcript below. Any load-bearing factual claim in the outcome with no supporting tool result is an unverified_claim.")
	return b.String()
}

// verdictAccepts is the pure verdict→accept decision the strict gate applies
// ([coupling].high_stakes, i_cass_1). A completion passes only when the OUTCOME
// is GROUNDED in the executed transcript — every load-bearing claim backed by a
// real result (no unverified claims) — AND its coverage is honest: a claimed-
// full turn needs Cassandra to agree coverage is complete, while a claimed
// partial is honest incompleteness that only needs grounding. This preserves
// honest partials (Cassandra blocks false SUCCESS, not honest "I didn't
// finish").
func verdictAccepts(coverage string, v *cassandra.Verdict) bool {
	if v == nil {
		return false
	}
	grounded := v.Grounded && len(v.UnverifiedClaims) == 0
	fullOK := coverage == "partial" || v.CoverageComplete()
	return grounded && fullOK
}

// cappedUnknowns returns the (capped) list of Cassandra-flagged unknowns for an
// accepted completion. F4: the verification signal is a SEPARATE subtle
// affordance — these caveats ride the cassandra.* side-channel events the UI
// renders small, and are NEVER folded into the delivered/persisted answer text
// (ux_truth: the user sees the clean result, never the completeness proof).
// Returns nil when there is nothing flagged.
func cappedUnknowns(v *cassandra.Verdict) []string {
	if v == nil || len(v.OpenUnknowns) == 0 {
		return nil
	}
	flagged := v.OpenUnknowns
	if len(flagged) > 2 {
		flagged = flagged[:2]
	}
	return flagged
}

// continueFeedback turns a not-grounded / incomplete verdict into concrete,
// actionable feedback fed back as the task_complete tool result so the loop
// continues productively (advisor-not-effector, i_cass_4). It tells Neo what is
// wrong and what needs fixing — the present-object negative space
// ([thesis].mechanism): missing deliverables, unverified claims, open unknowns.
func continueFeedback(v *cassandra.Verdict) string {
	var b strings.Builder
	b.WriteString("Not done yet — the outcome you're about to return doesn't fully satisfy the goal. ")
	if len(v.Missing) > 0 {
		b.WriteString("Still missing: " + strings.Join(v.Missing, "; ") + ". ")
	}
	if len(v.UnverifiedClaims) > 0 {
		b.WriteString("These claims aren't backed by anything you actually did — verify them with a tool or drop them: " + strings.Join(v.UnverifiedClaims, "; ") + ". ")
	}
	if len(v.OpenUnknowns) > 0 {
		b.WriteString("Resolve or honestly surface these open unknowns: " + strings.Join(v.OpenUnknowns, "; ") + ". ")
	}
	b.WriteString("Fix these and re-call task_complete, or set coverage=\"partial\" and tell the user plainly what remains unresolved.")
	return b.String()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
