// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_paxeer_spend.go — PaxeerSpendPolicy enforcement for the public
// per-user runtime (daemon_pipeline.go runMessage path).
//
// Mirrors enforceGideonGuardrails / runGideonGate from
// daemon_gideon_pipeline.go: walks every NodeToolCall in the
// synthesized plan, evaluates each against PaxeerSpendPolicy, and
// returns the first hard verdict (deny on malformed value, gate on
// per-call or aggregate cap exceedance). On a per-call or aggregate
// gate, opens the same httpGateHandler used for synthesized gate nodes
// so the panel / channel can answer it via POST
// /intents/<id>/gates/<nid>/answer. A denied or timed-out gate fails
// the intent before any tool dispatch.
//
// Pre-pass performs zero cortex writes — replay byte-identity invariant
// is untouched.

import (
	"context"
	"fmt"

	"matrix/mcl/ir"
)

// enforcePaxeerSpend gates a synthesized plan against the daemon's
// PaxeerSpendPolicy. Returns:
//
//   - (nil, nil)               → every paxeer-net write is within cap, or
//     the policy is disabled, or the plan
//     has no paxeer-net writes.
//   - (*SpendEvaluation, nil)  → blocking verdict (deny | denied gate);
//     caller signs intent.fail with
//     reason="policy_denied".
//   - (nil, error)             → fatal evaluation error (gate broker
//     hiccup); the daemon stays up but
//     bubbles the error to /messages.
func (d *daemonState) enforcePaxeerSpend(ctx context.Context, intentID string, plan *ir.PlanTree, t *transcript) (*SpendEvaluation, error) {
	if d.paxeerSpend == nil || plan == nil {
		return nil, nil
	}

	var calls []*ir.PlanNode
	collectToolCalls(&plan.Root, &calls)

	perCall := make([]SpendEvaluation, 0, len(calls))
	for _, node := range calls {
		tc := node.ToolCall
		if !d.paxeerSpend.AppliesTo(tc.ToolRef) {
			continue
		}
		ev := d.paxeerSpend.Evaluate(tc.ToolRef, tc.Args)
		fields := map[string]interface{}{
			"intent_id": intentID,
			"node_id":   node.ID,
			"tool":      ev.Tool,
			"decision":  ev.Decision.String(),
			"rule":      ev.Rule,
			"rule_arg":  ev.RuleArg,
		}
		if ev.ValueWei != nil {
			fields["value_wei"] = ev.ValueWei.String()
		}
		t.Event("paxeer.spend.eval", "walk", fields)

		switch ev.Decision {
		case SpendAllow:
			perCall = append(perCall, ev)
			continue
		case SpendDeny:
			t.Event("paxeer.spend.deny", "walk", map[string]interface{}{
				"intent_id": intentID,
				"node_id":   node.ID,
				"rule":      ev.Rule,
				"reason":    ev.Reason,
			})
			return &ev, nil
		case SpendGate:
			approved, gerr := d.runPaxeerSpendGate(ctx, intentID, node.ID, ev, t)
			if gerr != nil {
				return nil, gerr
			}
			if !approved {
				blocked := ev
				blocked.Reason = "spend gate not approved: " + ev.Reason
				return &blocked, nil
			}
			// Approved → this call is cleared. Still feed its value
			// into the aggregate so the per-plan total reflects it.
			perCall = append(perCall, ev)
		}
	}

	// Aggregate gate fires when the sum of approved per-call values
	// exceeds AggregateCapWei. Same broker, same fail-closed posture.
	agg := d.paxeerSpend.EvaluateAggregate(perCall)
	if agg.Decision == SpendGate {
		t.Event("paxeer.spend.aggregate.eval", "walk", map[string]interface{}{
			"intent_id":       intentID,
			"rule":            agg.Rule,
			"aggregate_value": agg.ValueWei.String(),
		})
		approved, gerr := d.runPaxeerSpendGate(ctx, intentID, "aggregate", agg, t)
		if gerr != nil {
			return nil, gerr
		}
		if !approved {
			blocked := agg
			blocked.Reason = "aggregate spend gate not approved: " + agg.Reason
			return &blocked, nil
		}
	}

	return nil, nil
}

// runPaxeerSpendGate opens a forced approval gate for a per-call or
// aggregate spend cap exceedance through the same gateBroker /
// httpGateHandler used for synthesized gate nodes. Returns whether it
// was approved.
//
// When no gateBroker is wired the gate cannot be answered, so it
// fail-closes (denies). A spend over cap is NEVER auto-approved.
func (d *daemonState) runPaxeerSpendGate(ctx context.Context, intentID, nodeID string, ev SpendEvaluation, t *transcript) (bool, error) {
	if d.gateBroker == nil {
		t.Event("paxeer.spend.gate.unavailable", "walk", map[string]interface{}{
			"intent_id": intentID,
			"node_id":   nodeID,
			"reason":    "no gate broker; spend gate fail-closed denied",
		})
		return false, nil
	}
	gateNodeID := nodeID + "-paxeer-spend-gate"
	value := "0"
	if ev.ValueWei != nil {
		value = ev.ValueWei.String()
	}
	gateNode := &ir.PlanNode{
		ID:   gateNodeID,
		Kind: ir.NodeGate,
		Gate: &ir.GatePayload{
			RuleRef: "matrix://rule/paxeer/" + ev.Rule,
			Question: fmt.Sprintf(
				"PAXEER SPEND OVER CAP — tool=%q rule=%q value=%s wei (%s). "+
					"This will move value on Paxeer mainnet (chain 125). Approve?",
				ev.Tool, ev.Rule, value, ev.Reason),
			Options: []string{"yes", "no"},
		},
	}
	handler := newHTTPGateHandler(d.gateBroker, intentID, d.actor.UserURI, t, d.gateTimeout)
	decision, err := handler.HandleGate(ctx, gateNode)
	if err != nil {
		return false, err
	}
	t.Event("paxeer.spend.gate.decided", "walk", map[string]interface{}{
		"intent_id": intentID,
		"node_id":   gateNodeID,
		"approved":  decision.Approved,
		"answer":    decision.Answer,
	})
	return decision.Approved, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
