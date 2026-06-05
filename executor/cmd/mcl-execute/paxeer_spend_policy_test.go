// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"math/big"
	"testing"
)

// TestPaxeerPerCallCap exercises the per-call ceiling: every gated
// write tool whose value-arg exceeds PerCallCapWei must gate; values
// at or below the cap pass; non-monetary writes pass; reads pass.
func TestPaxeerPerCallCap(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()

	gateCases := []struct {
		name    string
		toolURI string
		args    map[string]string
		argKey  string
	}{
		{"transfer_amount_wei", "matrix://tool/mcp/paxeer-net/transfer@0.1.0",
			map[string]string{"to": "0xabc", "amount_wei": "2000000000000000000"}, "amount_wei"}, // 2 PAX
		{"transfer_amount_decimal_alias", "matrix://tool/mcp/paxeer-net/transfer@0.1.0",
			map[string]string{"to": "0xabc", "amount": "1500000000000000001"}, "amount"}, // 1.5 PAX
		{"transfer_amount_hex", "matrix://tool/mcp/paxeer-net/transfer@0.1.0",
			map[string]string{"to": "0xabc", "amount_wei": "0x1bc16d674ec80000"}, "amount_wei"}, // 2 PAX in hex
		{"approve_amount_wei", "matrix://tool/mcp/paxeer-net/approve@0.1.0",
			map[string]string{"spender": "0xdef", "amount_wei": "10000000000000000000"}, "amount_wei"}, // 10 PAX
		{"stream_open_cap_wei", "matrix://tool/mcp/paxeer-net/stream_open@0.1.0",
			map[string]string{"payee": "0xff", "cap_wei": "5000000000000000001"}, "cap_wei"},
		{"schedule_job_deposit_wei", "matrix://tool/mcp/paxeer-net/schedule_job@0.1.0",
			map[string]string{"target": "0xff", "deposit_wei": "1500000000000000000"}, "deposit_wei"},
		{"delegate_amount_wei", "matrix://tool/mcp/paxeer-net/delegate@0.1.0",
			map[string]string{"validator": "paxvaloper1abc", "amount_wei": "3000000000000000000"}, "amount_wei"},
		{"redelegate_amount_wei", "matrix://tool/mcp/paxeer-net/redelegate@0.1.0",
			map[string]string{"src": "v1", "dst": "v2", "amount_wei": "2000000000000000000"}, "amount_wei"},
		{"contract_write_value_wei", "matrix://tool/mcp/paxeer-net/contract_write@0.1.0",
			map[string]string{"to": "0xff", "data": "0x", "value_wei": "1000000000000000001"}, "value_wei"}, // 1 + 1 wei
	}
	for _, tc := range gateCases {
		t.Run("gate/"+tc.name, func(t *testing.T) {
			ev := p.Evaluate(tc.toolURI, tc.args)
			if ev.Decision != SpendGate {
				t.Fatalf("got %s, want gate (rule=%s reason=%s)", ev.Decision, ev.Rule, ev.Reason)
			}
			if ev.Rule != PaxeerRulePerCallCap {
				t.Fatalf("got rule %q, want %q", ev.Rule, PaxeerRulePerCallCap)
			}
			if ev.RuleArg != tc.argKey {
				t.Fatalf("got RuleArg %q, want %q", ev.RuleArg, tc.argKey)
			}
			if ev.ValueWei == nil || ev.ValueWei.Sign() <= 0 {
				t.Fatalf("expected non-zero ValueWei, got %v", ev.ValueWei)
			}
		})
	}

	allowCases := []struct {
		name    string
		toolURI string
		args    map[string]string
	}{
		// At cap (1 PAX exactly) — allowed.
		{"transfer_at_cap", "matrix://tool/mcp/paxeer-net/transfer@0.1.0",
			map[string]string{"to": "0xabc", "amount_wei": "1000000000000000000"}},
		// Below cap.
		{"transfer_dust", "matrix://tool/mcp/paxeer-net/transfer@0.1.0",
			map[string]string{"to": "0xabc", "amount_wei": "1"}},
		// Non-monetary writes — no value-arg mapping → pass.
		{"stream_close", "matrix://tool/mcp/paxeer-net/stream_close@0.1.0",
			map[string]string{"id": "42"}},
		{"cancel_job", "matrix://tool/mcp/paxeer-net/cancel_job@0.1.0",
			map[string]string{"id": "1"}},
		{"undelegate", "matrix://tool/mcp/paxeer-net/undelegate@0.1.0",
			map[string]string{"validator": "v1", "amount_wei": "10000000000000000000"}}, // not in ValueArgs map
		// Reads — pass through.
		{"chain_info", "matrix://tool/mcp/paxeer-net/chain_info@0.1.0",
			map[string]string{}},
		{"price", "matrix://tool/mcp/paxeer-net/price@0.1.0",
			map[string]string{"asset": "pax"}},
		// Other servers — not paxeer-net, ignored.
		{"fs_write", "matrix://tool/mcp/fs/write_file@2024.11.1",
			map[string]string{"path": "/tmp/x", "content": "hi"}},
		// Approve with no amount supplied — treated as zero, pass.
		{"approve_no_amount", "matrix://tool/mcp/paxeer-net/approve@0.1.0",
			map[string]string{"spender": "0xff"}},
	}
	for _, tc := range allowCases {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			ev := p.Evaluate(tc.toolURI, tc.args)
			if ev.Decision != SpendAllow {
				t.Fatalf("got %s (rule=%s reason=%s), want allow", ev.Decision, ev.Rule, ev.Reason)
			}
		})
	}
}

// TestPaxeerMalformedValueDenies covers the SpendDeny verdict for
// malformed wei strings. We REFUSE to silently treat garbage as zero —
// a planner that emits non-numeric value args is broken.
func TestPaxeerMalformedValueDenies(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	denyCases := []struct {
		name    string
		toolURI string
		args    map[string]string
	}{
		{"transfer_garbage", "matrix://tool/mcp/paxeer-net/transfer@0.1.0",
			map[string]string{"to": "0xabc", "amount_wei": "not-a-number"}},
		{"transfer_negative", "matrix://tool/mcp/paxeer-net/transfer@0.1.0",
			map[string]string{"to": "0xabc", "amount_wei": "-1"}},
		{"contract_write_bad_hex", "matrix://tool/mcp/paxeer-net/contract_write@0.1.0",
			map[string]string{"to": "0xff", "data": "0x", "value_wei": "0xZZZ"}},
	}
	for _, tc := range denyCases {
		t.Run("deny/"+tc.name, func(t *testing.T) {
			ev := p.Evaluate(tc.toolURI, tc.args)
			if ev.Decision != SpendDeny {
				t.Fatalf("got %s (rule=%s), want deny", ev.Decision, ev.Rule)
			}
			if ev.Rule != PaxeerRuleMalformed {
				t.Fatalf("got rule %q, want %q", ev.Rule, PaxeerRuleMalformed)
			}
		})
	}
}

// TestPaxeerAggregateCap exercises the plan-level ceiling: a sequence
// of allowed per-call evaluations whose ValueWei sum exceeds
// AggregateCapWei must gate at the aggregate stage.
func TestPaxeerAggregateCap(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	mk := func(wei string) SpendEvaluation {
		v := new(big.Int)
		v.SetString(wei, 10)
		return SpendEvaluation{Decision: SpendAllow, Tool: "transfer", ValueWei: v}
	}

	// 3 transfers of 0.6 PAX each = 1.8 PAX, under 5-PAX aggregate.
	under := []SpendEvaluation{
		mk("600000000000000000"), mk("600000000000000000"), mk("600000000000000000"),
	}
	if got := p.EvaluateAggregate(under); got.Decision != SpendAllow {
		t.Fatalf("under-aggregate got %s, want allow (reason=%s)", got.Decision, got.Reason)
	}

	// 7 transfers of 0.8 PAX each = 5.6 PAX, over the 5-PAX aggregate.
	over := make([]SpendEvaluation, 7)
	for i := range over {
		over[i] = mk("800000000000000000")
	}
	if got := p.EvaluateAggregate(over); got.Decision != SpendGate {
		t.Fatalf("over-aggregate got %s, want gate (reason=%s)", got.Decision, got.Reason)
	} else if got.Rule != PaxeerRuleAggregateCap {
		t.Fatalf("got rule %q, want %q", got.Rule, PaxeerRuleAggregateCap)
	}
}

// TestPaxeerAppliesTo confirms the alias gate: only paxeer-net write
// tools are claimed by the policy; everything else is "not our concern".
func TestPaxeerAppliesTo(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	cases := []struct {
		uri  string
		want bool
	}{
		{"matrix://tool/mcp/paxeer-net/transfer@0.1.0", true},
		{"matrix://tool/mcp/paxeer-net/contract_write@0.1.0", true},
		{"matrix://tool/mcp/paxeer-net/chain_info@0.1.0", false}, // read
		{"matrix://tool/mcp/paxeer-net/stream_close@0.1.0", false},
		{"matrix://tool/mcp/fs/write_file@2024.11.1", false},
		{"matrix://tool/mcp/git/git_commit@2024.11.1", false},
		{"bare-ref", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			if got := p.AppliesTo(tc.uri); got != tc.want {
				t.Fatalf("AppliesTo(%q) = %v, want %v", tc.uri, got, tc.want)
			}
		})
	}
}

// TestPaxeerNilPolicySafe — a nil policy must short-circuit cleanly so
// the daemon can run without spend gating in dev.
func TestPaxeerNilPolicySafe(t *testing.T) {
	var p *PaxeerSpendPolicy
	if p.AppliesTo("matrix://tool/mcp/paxeer-net/transfer@0.1.0") {
		t.Fatal("nil policy claimed transfer")
	}
	ev := p.Evaluate("matrix://tool/mcp/paxeer-net/transfer@0.1.0",
		map[string]string{"amount_wei": "999999999999999999999"})
	if ev.Decision != SpendAllow {
		t.Fatalf("nil policy got %s, want allow", ev.Decision)
	}
	agg := p.EvaluateAggregate([]SpendEvaluation{
		{Decision: SpendAllow, ValueWei: big.NewInt(123)},
	})
	if agg.Decision != SpendAllow {
		t.Fatalf("nil policy aggregate got %s, want allow", agg.Decision)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
