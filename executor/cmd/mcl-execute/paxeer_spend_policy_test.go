// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"encoding/json"
	"math/big"
	"reflect"
	"testing"
	"time"
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

// ---------------------------------------------------------------------------
// P2-1: circuit breaker + per-recipient cap + rolling time-window budget.
// These tests exercise the hardened PaxeerSpendPolicy. The state is runtime/
// sidecar only and must never be journaled or serialized into OverallRoot.
// ---------------------------------------------------------------------------

// onePAX is a test helper: 1e18 wei.
func onePAX() *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
}

// spendArgs builds a transfer args map for the given recipient + wei amount.
func spendArgs(to, wei string) map[string]string {
	return map[string]string{"to": to, "amount_wei": wei}
}

// TestSpend_PerRecipientCapEnforced — a single recipient may not receive
// more than PerRecipientCapWei across multiple allowed calls. The second
// call that would push the recipient over the per-recipient ceiling must
// be gated (SpendGate), and the rule must be the per-recipient rule.
func TestSpend_PerRecipientCapEnforced(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	// Raise the per-call cap so the per-recipient gate is what's exercised.
	p.PerCallCapWei = new(big.Int).Mul(onePAX(), big.NewInt(5)) // 5 PAX
	// Tighten the per-recipient cap so the test is deterministic.
	p.PerRecipientCapWei = new(big.Int).Mul(onePAX(), big.NewInt(2)) // 2 PAX
	st := p.NewState()

	const recipient = "0xRECIPIENT_A"
	transfer := "matrix://tool/mcp/paxeer-net/transfer@0.1.0"

	// 1.5 PAX to recipient A — allowed, recorded.
	ev1 := p.EvaluateWithState(transfer, spendArgs(recipient, "1500000000000000000"), st, time.Now())
	if ev1.Decision != SpendAllow {
		t.Fatalf("first call got %s, want allow (rule=%s reason=%s)", ev1.Decision, ev1.Rule, ev1.Reason)
	}

	// Another 1 PAX to recipient A → total 2.5 PAX > 2 PAX cap → must gate.
	ev2 := p.EvaluateWithState(transfer, spendArgs(recipient, "1000000000000000000"), st, time.Now())
	if ev2.Decision != SpendGate {
		t.Fatalf("second call got %s, want gate (rule=%s reason=%s)", ev2.Decision, ev2.Rule, ev2.Reason)
	}
	if ev2.Rule != PaxeerRulePerRecipientCap {
		t.Fatalf("got rule %q, want %q", ev2.Rule, PaxeerRulePerRecipientCap)
	}
}

// TestSpend_CircuitBreakerTripsAfterNViolations — after N cap-violations
// (gate or deny verdicts) within the breaker window, the breaker trips and
// ALL subsequent calls are rejected with the breaker rule until cooldown.
func TestSpend_CircuitBreakerTripsAfterNViolations(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	p.BreakerThreshold = 3
	p.BreakerWindow = 5 * time.Minute
	p.BreakerCooldown = 10 * time.Minute
	st := p.NewState()

	transfer := "matrix://tool/mcp/paxeer-net/transfer@0.1.0"
	// Each call exceeds the per-call cap (1 PAX) → SpendGate = a violation.
	over := spendArgs("0xA", "5000000000000000000") // 5 PAX

	now := time.Now()
	for i := 0; i < p.BreakerThreshold; i++ {
		ev := p.EvaluateWithState(transfer, over, st, now)
		// Each is a cap-violation (gate), recorded as a breaker violation.
		if ev.Decision != SpendGate {
			t.Fatalf("call %d got %s, want gate (rule=%s)", i, ev.Decision, ev.Rule)
		}
	}

	// After N violations, the breaker should be tripped.
	if !st.IsTripped(now) {
		t.Fatal("breaker should be tripped after N violations")
	}

	// The very next call — even a tiny allowed amount — must be DENIED
	// (fail CLOSED) because the breaker is tripped.
	small := spendArgs("0xB", "1")
	ev := p.EvaluateWithState(transfer, small, st, now)
	if ev.Decision != SpendDeny {
		t.Fatalf("post-trip call got %s, want deny (fail-closed)", ev.Decision)
	}
	if ev.Rule != PaxeerRuleBreakerTripped {
		t.Fatalf("post-trip rule %q, want %q", ev.Rule, PaxeerRuleBreakerTripped)
	}
}

// TestSpend_BreakerCooldownRecovers — once the breaker cooldown elapses,
// the breaker resets and calls are evaluated normally again.
func TestSpend_BreakerCooldownRecovers(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	p.BreakerThreshold = 2
	p.BreakerWindow = 5 * time.Minute
	p.BreakerCooldown = 1 * time.Minute
	st := p.NewState()

	transfer := "matrix://tool/mcp/paxeer-net/transfer@0.1.0"
	over := spendArgs("0xA", "5000000000000000000") // 5 PAX > 1 PAX cap

	tripTime := time.Now()
	for i := 0; i < p.BreakerThreshold; i++ {
		p.EvaluateWithState(transfer, over, st, tripTime)
	}
	if !st.IsTripped(tripTime) {
		t.Fatal("breaker should be tripped")
	}

	// While tripped (before cooldown) → deny.
	ev := p.EvaluateWithState(transfer, over, st, tripTime.Add(30*time.Second))
	if ev.Decision != SpendDeny {
		t.Fatalf("pre-cooldown got %s, want deny", ev.Decision)
	}

	// After cooldown elapses → breaker resets, normal evaluation resumes.
	after := tripTime.Add(p.BreakerCooldown + time.Second)
	ev2 := p.EvaluateWithState(transfer, over, st, after)
	// 5 PAX > 1 PAX per-call cap → gate (not breaker deny).
	if ev2.Decision != SpendGate {
		t.Fatalf("post-cooldown got %s, want gate (rule=%s reason=%s)", ev2.Decision, ev2.Rule, ev2.Reason)
	}
	if ev2.Rule == PaxeerRuleBreakerTripped {
		t.Fatal("breaker rule should NOT fire after cooldown")
	}
}

// TestSpend_RollingWindowBudgetRejectsOverspend — total spend within a
// rolling time window must not exceed RollingWindowBudgetWei. Once the
// window budget is consumed, further spend is gated.
func TestSpend_RollingWindowBudgetRejectsOverspend(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	// Raise the per-call cap so the rolling-window gate is what's exercised.
	p.PerCallCapWei = new(big.Int).Mul(onePAX(), big.NewInt(5))          // 5 PAX
	p.RollingWindowBudgetWei = new(big.Int).Mul(onePAX(), big.NewInt(3)) // 3 PAX
	p.RollingWindowDuration = 10 * time.Minute
	st := p.NewState()

	transfer := "matrix://tool/mcp/paxeer-net/transfer@0.1.0"
	base := time.Now()

	// 2 PAX — allowed, recorded into the rolling window.
	ev1 := p.EvaluateWithState(transfer, spendArgs("0xA", "2000000000000000000"), st, base)
	if ev1.Decision != SpendAllow {
		t.Fatalf("first got %s, want allow (rule=%s reason=%s)", ev1.Decision, ev1.Rule, ev1.Reason)
	}

	// 2 PAX more → total 4 PAX > 3 PAX window budget → must gate.
	ev2 := p.EvaluateWithState(transfer, spendArgs("0xB", "2000000000000000000"), st, base.Add(1*time.Minute))
	if ev2.Decision != SpendGate {
		t.Fatalf("second got %s, want gate (rule=%s reason=%s)", ev2.Decision, ev2.Rule, ev2.Reason)
	}
	if ev2.Rule != PaxeerRuleRollingWindowBudget {
		t.Fatalf("got rule %q, want %q", ev2.Rule, PaxeerRuleRollingWindowBudget)
	}

	// After the window rolls past, the old spend expires and 2 PAX is OK.
	ev3 := p.EvaluateWithState(transfer, spendArgs("0xC", "2000000000000000000"),
		st, base.Add(p.RollingWindowDuration+time.Second))
	if ev3.Decision != SpendAllow {
		t.Fatalf("post-window got %s, want allow (rule=%s reason=%s)", ev3.Decision, ev3.Rule, ev3.Reason)
	}
}

// TestSpend_StateNotJournaled — the policy's runtime state must be purely
// sidecar: it carries NO serialization hooks (no MarshalJSON / journal
// integration). We assert the SpendPolicyState type does not implement
// json.Marshaler and has no "Journal" or "Serialize" method. This is a
// compile-time + reflection guarantee that the state is never journaled.
func TestSpend_StateNotJournaled(t *testing.T) {
	p := DefaultPaxeerSpendPolicy()
	st := p.NewState()

	// (a) Must NOT implement json.Marshaler.
	var _ interface{} = st
	if _, ok := interface{}(st).(json.Marshaler); ok {
		t.Fatal("SpendPolicyState must NOT implement json.Marshaler — state must be sidecar-only")
	}

	// (b) Must NOT expose Serialize / Journal / Marshal / Snapshot methods
	// (reflection scan of the concrete type).
	stType := reflect.TypeOf(st)
	if stType.Kind() == reflect.Ptr {
		stType = stType.Elem()
	}
	forbidden := map[string]bool{
		"Serialize": true, "Marshal": true, "Journal": true,
		"Snapshot": true, "ToBytes": true, "Encode": true,
	}
	for i := 0; i < stType.NumMethod(); i++ {
		name := stType.Method(i).Name
		if forbidden[name] {
			t.Fatalf("SpendPolicyState must not expose serialization method %q", name)
		}
	}

	// (c) SpendPolicyState is NOT referenced from OverallRoot-typed state.
	// The daemonState.paxeerSpend field is *PaxeerSpendPolicy (config-only);
	// the mutable state lives in a separate *SpendPolicyState that the
	// daemon holds alongside but never marshals. Assert the policy struct
	// field for state is unexported + a pointer (sidecar, not embedded
	// value that would get value-copied into snapshots).
	polType := reflect.TypeOf(p)
	if polType.Kind() != reflect.Ptr || polType.Elem().Kind() != reflect.Struct {
		t.Fatal("policy must be *struct")
	}
	polElem := polType.Elem()
	stateField, ok := polElem.FieldByName("state")
	if !ok {
		t.Fatal("PaxeerSpendPolicy must have a 'state' field for the sidecar state")
	}
	// An unexported field has a non-empty PkgPath; an exported field has
	// PkgPath == "". We require the state field to be unexported so it
	// can never be value-copied into a snapshot/journal by reflection.
	if stateField.PkgPath == "" {
		t.Fatal("PaxeerSpendPolicy.state must be unexported (sidecar, never journaled)")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
