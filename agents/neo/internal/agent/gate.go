// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// gate.go — the check-before-act gate (epistemic-core req.5). Dispatch is
// structurally gated on the premise ledger: a self-referential capability
// claim validates against the RESIDENT capability surface deterministically
// at the gate (identity-scrub posture: high-precision selection, fail toward
// checking) — the check discharges the premise to cited or refutes it — and a
// standing refuted premise refuses further dependent dispatches until a
// forced plan-revision step (tools-stripped, reasoning-only) has run. The
// arena failure class — scaffolding against an endpoint the agent does not
// have — cannot recur: the premise dies at the gate, before the first
// dependent action.
package agent

import (
	"context"
	"fmt"
	"strings"

	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/tools"
)

type claimVerdict int

const (
	claimUnknown claimVerdict = iota // no resident surface to check against
	claimCited                       // matches a resident surface fact
	claimRefuted                     // contradicts the resident surface
)

func (a *Agent) validateSelfClaim(p *Premise) (claimVerdict, string) {
	cs := a.capability
	if cs == nil || (len(cs.API) == 0 && len(cs.Is) == 0 && len(cs.IsNot) == 0) {
		return claimUnknown, ""
	}
	stmt := strings.ToLower(p.Statement)

	// Cited: the claim names a concrete resident surface fact (a route token
	// like /chat, /events riding an API line).
	for _, line := range append(append([]string(nil), cs.API...), cs.Is...) {
		for _, tok := range routeTokens(line) {
			if strings.Contains(stmt, tok) {
				return claimCited, line
			}
		}
	}
	// Cited: the claim names an advertised tool — the inventory the surface
	// section derives from a.schemas IS the resident fact.
	for _, s := range a.schemas {
		name := strings.ToLower(strings.TrimSpace(s.Function.Name))
		if name != "" && strings.Contains(stmt, name) {
			return claimCited, "advertised tool: " + s.Function.Name
		}
	}
	// Refuted by an explicit is-not fact sharing a distinctive marker
	// (e.g. the OpenAI-compatible-endpoint refutation).
	for _, line := range cs.IsNot {
		for _, marker := range []string{"openai", "/v1/chat/completions", "/v1/completions", "/v1/models", "completions backend"} {
			if strings.Contains(stmt, marker) && strings.Contains(strings.ToLower(line), marker) {
				return claimRefuted, line
			}
		}
	}
	// Refuted: the claim names a tool-shaped token that matches NO advertised
	// schema — the inventory is complete, so a named-but-absent tool is false.
	if tok := toolShapedToken(stmt); tok != "" {
		return claimRefuted, "your tool inventory is complete and does not contain " + tok
	}
	// Everything else: unverified, not false. A matcher this coarse cannot
	// prove absence; the premise stays a visible ASSUMPTION (never refuses).
	return claimUnknown, ""
}

// toolShapedToken returns a token that looks like a concrete tool name
// (alias__name shape) if the statement carries one that matches no schema.
func toolShapedToken(stmt string) string {
	for _, f := range strings.Fields(stmt) {
		f = strings.Trim(f, ".,;:()\"'`")
		if !strings.Contains(f, "__") || len(f) < 5 {
			continue
		}
		return f
	}
	return ""
}

// routeTokens extracts the concrete route tokens (/chat, /events, …) from a
// surface line.
func routeTokens(line string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(line)) {
		f = strings.Trim(f, ".,;:()\"'")
		if strings.HasPrefix(f, "/") && len(f) > 1 {
			out = append(out, f)
		}
	}
	return out
}

// gateExempt lists the introspection tools the gate NEVER refuses — they ARE
// the checks (memory/self lookups, the plan surface, overflow paging), so
// refusing them would deadlock the discharge path.
func gateExempt(name string) bool {
	switch name {
	case tools.MemoryRecallTool, tools.TodoTool, readOverflowTool:
		return true
	}
	return false
}

// maxRevisionsPerTurn bounds forced revision steps so a pathological ledger
// can never wedge a turn; past the bound the outer failsafes own the loop.
const maxRevisionsPerTurn = 3

// checkBeforeAct is the dispatch gate (req.5.1/5.2/5.3), run against every
// tool batch before it dispatches. It first RUNS the cheap check every
// unchecked self-referential premise admits — validation against the resident
// capability surface — discharging or refuting each (a contradiction is a
// mismatch event, rendered as structured guidance, never silently retained).
// If any refuted premise then stands unrevised, the non-exempt calls of the
// batch are refused with a directive naming the premise and the check, and a
// tools-stripped plan-revision step is forced before further dependent
// dispatches. Returns the calls that may dispatch and whether a refusal
// happened.
func (a *Agent) checkBeforeAct(calls []llm.ToolCall) ([]llm.ToolCall, bool) {
	if !a.epistemicPremisesOn() || a.turn.ledger == nil {
		return calls, false
	}

	// The check runs at the gate: every unchecked self-claim validates
	// against the resident surface NOW (one attention read, zero tool calls).
	for _, p := range a.turn.ledger.ungroundedSelf() {
		verdict, citation := a.validateSelfClaim(p)
		switch verdict {
		case claimCited:
			a.premiseDischarge(p, "self-model", citation)
		case claimRefuted:
			a.premiseRefute(p, citation)
			// A self-claim contradiction is a mismatch event (req.5.3).
			a.pushGuidance(fmt.Sprintf(
				"Self-claim contradiction: your plan asserts %q, but your resident capability surface says otherwise (%s). The claim is refuted — do not retain it.",
				p.Statement, citation))
		}
	}

	refuted := a.turn.ledger.unrevisedRefuted()
	if len(refuted) == 0 || a.turn.revisionsThisTurn >= maxRevisionsPerTurn {
		return calls, false
	}

	// A refuted premise stands: refuse the dependent dispatches (exempt
	// introspection tools still run — they are the discharge path).
	var allowed []llm.ToolCall
	refusedAny := false
	var names []string
	for _, p := range refuted {
		names = append(names, fmt.Sprintf("P%d %q (refuted: %s)", p.ID, p.Statement, citationLine(p.Citation)))
	}
	directive := "not dispatched (check-before-act): your plan rests on a REFUTED premise — " + strings.Join(names, "; ") +
		". Revise the plan around the refuted premise first; you will get a reasoning-only step to do it."
	for _, call := range calls {
		if gateExempt(call.Function.Name) {
			allowed = append(allowed, call)
			continue
		}
		refusedAny = true
		a.working = append(a.working, llm.ToolResult(call.ID, call.Function.Name, directive))
	}
	if refusedAny {
		a.turn.revisionPending = "A premise your plan depends on was refuted: " + strings.Join(names, "; ") +
			". This step has NO tools on purpose: think it through and write your REVISED plan — what does the refutation change, and what grounded approach replaces it? Your premises will be re-read from what you write."
	}
	return allowed, refusedAny
}

// forcedRevisionStep runs the tools-stripped reasoning-only step (req.5.2 /
// 6.3 / 7.2): the model must revise the plan in text before any further
// dispatch. The revision output becomes the new plan — the ledger re-extracts
// from it (plan change), refuted premises are marked revised, and the
// convergence meter resets to give the revised strategy a fresh read. Returns
// an error only on transport failure.
func (a *Agent) forcedRevisionStep(ctx context.Context, window []llm.Message, onDelta func(llm.Delta)) error {
	reason := a.turn.revisionPending
	a.turn.revisionPending = ""
	a.turn.revisionsThisTurn++
	g := a.pushGuidance(reason)
	window = append(window, g)
	res, err := a.chatWithRetry(ctx, a.providerRequest(ctx, window, nil, onDelta, 0))
	if err != nil {
		return err
	}
	content := strings.TrimSpace(res.Message.Content)
	if content != "" {
		a.working = append(a.working, llm.Message{Role: llm.RoleAssistant, Content: content})
	}
	// The revision IS the plan change: refuted premises are answered, the
	// ledger re-extracts from the revised plan, and the per-strategy mismatch
	// meters + convergence read reset so the revised approach gets a fresh
	// measurement (a revision that repeats the failure re-trips the bounds).
	a.premisePlanChanged()
	a.premiseObservePlan(ctx, content)
	a.turn.mismatchMeter = nil
	if a.turn.graph != nil {
		a.turn.graph.actionsSinceGrowth = 0
		a.renderGraphTail()
	}
	return nil
}
