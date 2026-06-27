// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"

	"matrix/cassandra"
	"matrix/neo/internal/llm"
)

// cassandra.go — Cassandra Phase 3 wiring for the Neo completion gate.
//
// Phase 1 shipped a deterministic local validator. Phase 3 swaps in the full
// shared Cassandra adjudicator (matrix/cassandra) on state-touching turns: a
// grounded verdict over the executed transcript (ground truth) that broadens
// the audit from deliverable coverage to grounding, unverified claims,
// assumptions, and open unknowns. The deterministic priors remain the cheap
// pre-pass, and the deterministic grounding check remains the fail-open
// fallback so a Cassandra hiccup never breaks the turn (cassandra.frozen.kvx
// [seams.neo], [adjudicator], [coupling], [ux], [audit], i_cass_1/4/5).

// Cassandra audit event types streamed onto the run's event stream
// (cassandra.frozen.kvx [audit].events). Pure observability side-channel: the
// adjudicator signs nothing and writes no cortex on the happy path (i_cass_4,
// i_cass_6).
const (
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

// buildAuditContract folds the user's request together with the agent's claimed
// completion object so the auditor can check the CLAIM against the executed
// transcript ([verdict.fields].unverified_claims — the hallucination surface).
func buildAuditContract(userRequest, summary, coverage string, evidence, openGaps, assumptions []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(userRequest))
	b.WriteString("\n\n--- THE AGENT NOW CLAIMS THIS TURN IS DONE ---\n")
	b.WriteString("Claimed coverage: ")
	b.WriteString(coverage)
	b.WriteString("\nThe answer the agent is about to give the user:\n")
	b.WriteString(strings.TrimSpace(summary))
	if len(evidence) > 0 {
		b.WriteString("\nThe agent cites this evidence: ")
		b.WriteString(strings.Join(evidence, "; "))
	}
	if len(openGaps) > 0 {
		b.WriteString("\nThe agent admits these open items: ")
		b.WriteString(strings.Join(openGaps, "; "))
	}
	if len(assumptions) > 0 {
		b.WriteString("\nThe agent made these assumptions: ")
		b.WriteString(strings.Join(assumptions, "; "))
	}
	b.WriteString("\n\nVerify the agent's claimed answer against the executed transcript. Any load-bearing factual claim with no supporting tool result is an unverified_claim.")
	return b.String()
}

// verdictAccepts is the pure verdict→accept decision the high-stakes gate
// applies ([coupling].high_stakes, i_cass_1). A completion claim passes only
// when it is GROUNDED — every load-bearing claim backed by real evidence (no
// unverified claims, no phantom citations) — AND its coverage is honest: a
// claimed-full turn needs Cassandra to agree coverage is complete, while a
// claimed partial is honest incompleteness that only needs grounding. This
// preserves honest partials (Cassandra blocks false SUCCESS, not honest
// "I didn't finish").
func verdictAccepts(coverage string, v *cassandra.Verdict, phantom []string) bool {
	if v == nil {
		return false
	}
	grounded := v.Grounded && len(v.UnverifiedClaims) == 0 && len(phantom) == 0
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
// continues productively (advisor-not-effector, i_cass_4). It enumerates the
// present-object negative space ([thesis].mechanism): missing deliverables,
// unverified claims, phantom citations, and open unknowns.
func continueFeedback(v *cassandra.Verdict, phantom []string) string {
	var b strings.Builder
	b.WriteString("Not done yet — your completion claim doesn't hold up against what you actually did this turn. ")
	if len(v.Missing) > 0 {
		b.WriteString("Still missing: " + strings.Join(v.Missing, "; ") + ". ")
	}
	if len(v.UnverifiedClaims) > 0 {
		b.WriteString("These claims have no supporting tool result — verify them with a tool or drop them: " + strings.Join(v.UnverifiedClaims, "; ") + ". ")
	}
	if len(phantom) > 0 {
		b.WriteString("This cited evidence does not appear in your transcript: " + strings.Join(phantom, "; ") + ". ")
	}
	if len(v.OpenUnknowns) > 0 {
		b.WriteString("Resolve or honestly surface these open unknowns: " + strings.Join(v.OpenUnknowns, "; ") + ". ")
	}
	b.WriteString("Keep working and address these, or set coverage=\"partial\" and tell the user plainly what remains unresolved.")
	return b.String()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
