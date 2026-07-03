// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// critique.go — the completeness critic (Phase 10.5).
//
// The Matrix pipeline is plan-once-then-execute-blind: the planner emits a
// frozen plan_tree before any tool runs, and the walker executes it with no
// model in the loop to course-correct. A fast planner that under-generates can
// produce a plan covering only the FIRST sub-task of a multi-step request — and
// because that partial plan walks cleanly, the pipeline would sign intent.attest
// and tell the user "completed". This critic closes that gap: after a clean
// walk, an LLM auditor compares the user's ORIGINAL request against what was
// actually executed (real tool calls + their results), and reports whether every
// requested deliverable was produced. The pipeline then re-plans the missing
// work (DriveCorrectMaterial → re-synthesize → re-walk) up to a bounded number
// of rounds, and — critically — NEVER attests a still-incomplete run as success.
//
// The critic does NO cortex writes and signs NO envelopes; it is a pure
// observability/decision side-channel (like the Liaison), so when a plan is
// complete on the first try the signed envelope chain and the D11 replay
// byte-identity invariant are unchanged. Extra envelopes appear ONLY on a
// genuine re-plan, which is correct — more work was done.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/cassandra"
	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/interpreter"
)

// criticSlot is the gateway slot completeness audits route + meter on. The
// legacy critic borrowed the planner slot; Cassandra declares her OWN slot
// (cassandra.frozen.kvx [adjudicator.slot]) so audit spend is attributed under
// her identity. Must be on the gateway cassandra-slot whitelist
// (gateway rates.FreeTierWhitelist).
const criticSlot = "cassandra"

const (
	// criticNodeResultCap bounds each node's result text in the digest so a
	// single huge tool output cannot blow the critic prompt budget.
	criticNodeResultCap = 600
	// criticDigestCap bounds the whole execution digest.
	criticDigestCap = 8000
)

// criticMod returns the model the completeness critic should use: the
// dedicated criticModel knob when set, else the planner/executor model
// (synthMod). Empty lets critiquePlan fall through to the planner default.
func (d *daemonState) criticMod() string {
	if d.criticModel != "" {
		return d.criticModel
	}
	return d.synthMod()
}

// buildExecutionDigest renders the executed plan tree into a compact,
// dispatch-ordered transcript of what actually ran: each tool_call with its
// args and (truncated) result, and each step with its (truncated) output. This
// is the ground-truth evidence the critic audits against the user's request —
// NOT the plan's intent, but what the walker actually produced (ResultText is
// populated by runtime/walker.go for both tool calls and steps).
func buildExecutionDigest(plan *ir.PlanTree) string {
	if plan == nil {
		return "(no plan executed)"
	}
	var b strings.Builder
	walkPlanRec(&plan.Root, func(n *ir.PlanNode) {
		if n == nil {
			return
		}
		switch n.Kind {
		case ir.NodeToolCall:
			if n.ToolCall == nil {
				return
			}
			b.WriteString("TOOL ")
			b.WriteString(n.ToolCall.ToolRef)
			if len(n.ToolCall.Args) > 0 {
				b.WriteString(" args=")
				b.WriteString(truncate(compactArgs(n.ToolCall.Args), 300))
			}
			b.WriteString("\n  -> ")
			b.WriteString(truncate(oneLine(n.ResultText), criticNodeResultCap))
			b.WriteString("\n")
		case ir.NodeStep:
			b.WriteString("STEP ")
			b.WriteString(n.ID)
			b.WriteString("\n  -> ")
			b.WriteString(truncate(oneLine(n.ResultText), criticNodeResultCap))
			b.WriteString("\n")
		case ir.NodeGate:
			b.WriteString("GATE ")
			b.WriteString(n.ID)
			b.WriteString(" (human decision point)\n")
		}
	})
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "(plan executed but produced no recorded output)"
	}
	return truncate(out, criticDigestCap)
}

// compactArgs renders a tool_call's string-valued args as a single compact
// line (keys sorted for determinism by the underlying map iteration is not
// stable, so we sort).
func compactArgs(args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+truncate(oneLine(args[k]), 120))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// oneLine collapses newlines/tabs into spaces so a multi-line tool result
// stays on a readable single line in the digest.
func oneLine(s string) string {
	r := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// criticDecoder adapts an MCL llm client to cassandra.Decoder, preserving the
// reasoning-aware decode and the critic.decode transcript event so the
// re-home's observability is byte-identical to the legacy critic.
type criticDecoder struct {
	client interpreter.LLM
	model  string
	t      *transcript
}

func (c *criticDecoder) Decode(ctx context.Context, system, user string) (string, error) {
	messages := []interpreter.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	t0 := time.Now()
	var (
		raw  string
		derr error
	)
	if rd, ok := c.client.(reasoningDecoder); ok {
		raw, _, derr = rd.DecodeWithReasoning(ctx, messages, "")
	} else {
		raw, derr = c.client.Decode(ctx, messages, "")
	}
	c.t.Event("critic.decode", "verify", map[string]interface{}{
		"ms":    time.Since(t0).Milliseconds(),
		"bytes": len(raw),
		"model": c.model,
		"error": errStr(derr),
	})
	return raw, derr
}

// critiquePlan asks the Cassandra adjudicator whether the executed work
// satisfies the user's original request. executedDigest is the cumulative
// buildExecutionDigest across every walk so far. Returns the verdict, or an
// error if the LLM call / parse failed (the caller fails OPEN on error: a
// critic hiccup must never convert an otherwise-clean walk into a failure).
//
// This is the MCL seam of the shared Cassandra faculty (cassandra.frozen.kvx
// [seams.mcl]): the verdict-shaped logic now lives in matrix/cassandra; the
// MCL gate consults verdict.CoverageComplete(), preserving the legacy
// criticVerdict.Complete behavior byte-for-byte.
func (d *daemonState) critiquePlan(ctx context.Context, prose, executedDigest, intentID, goalID string, t *transcript, acc *intentCostAccumulator) (*cassandra.Verdict, error) {
	cfg := llm.DefaultPlannerModel()
	if m := d.criticMod(); m != "" {
		cfg.Model = m
	}
	// Free-form JSON: the verdict schema is not the plan_tree grammar that
	// DefaultPlannerModel pre-loads, so disable grammar and let cassandra
	// parse the object out of the raw output.
	cfg.GrammarMode = llm.GrammarNone
	cfg.Grammars = nil
	cfg.Seed = d.seed
	if d.llmBaseURL != "" {
		cfg.Endpoint = strings.TrimRight(d.llmBaseURL, "/") + "/v1/chat/completions"
	}
	// Route + meter on the dedicated cassandra slot ([adjudicator.slot]) so
	// completeness-audit spend is attributed to Cassandra, not the planner.
	gw := d.llmConfigFor(criticSlot, "", intentID, goalID, t, acc)
	if gw.GatewayURL != "" {
		cfg.GatewayURL = gw.GatewayURL
		cfg.ActorDID = gw.ActorDID
		cfg.IntentID = intentID
		cfg.GoalID = goalID
		cfg.SlotLabel = criticSlot
		cfg.OnResponseHeaders = gw.CostHook
	}
	client, err := llm.New(&cfg)
	if err != nil {
		return nil, fmt.Errorf("critique: llm.New: %w", err)
	}

	// Single Primary decoder, no Escalate: reproduces the legacy single-model
	// critic exactly. The deterministic priors run as a cheap advisory pre-pass
	// ([adjudicator].priors); they inform the auditor prompt but never decide
	// the gate, so the MCL re-plan behavior is unchanged.
	adj := &cassandra.Adjudicator{Primary: &criticDecoder{client: client, model: cfg.Model, t: t}}
	return adj.Adjudicate(ctx, cassandra.AuditInput{
		Request:  prose,
		Evidence: executedDigest,
		Priors:   cassandra.ScanPriors(cassandra.PriorInput{Evidence: executedDigest}),
	})
}

// buildContinuationNote produces the re-plan directive appended to the planner
// user prompt: what is already done (so it is not repeated) and what remains
// (so the new plan targets exactly the gap).
func buildContinuationNote(executedDigest string, missing []string) string {
	var b strings.Builder
	b.WriteString("\n\n== RE-PLAN (continuation) ==\n")
	b.WriteString("This is a CONTINUATION of an in-progress task. The work below ALREADY ran ")
	b.WriteString("successfully — do NOT repeat any of it:\n")
	b.WriteString(truncate(executedDigest, 4000))
	b.WriteString("\n\nThe following REQUIRED items are still UNSATISFIED. Produce a plan_tree@1 ")
	b.WriteString("that accomplishes ONLY these remaining items, in order, using real tool calls:\n")
	for i, m := range missing {
		b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, m))
	}
	b.WriteString("\nReference outputs already produced above via ${<nodeID>.output} where a ")
	b.WriteString("later step needs an earlier result (e.g. a compiled project_id or a deployed address).\n")
	return b.String()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
