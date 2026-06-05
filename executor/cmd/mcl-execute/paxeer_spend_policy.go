// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// paxeer_spend_policy.go — daemon-side plan-time spend gate for the
// paxeer-net MCP bridge (the public-user chain surface).
//
// Mirrors the structure of gideon_ops_policy.go: a pure, table-driven
// policy object with deterministic, testable decision functions. The
// policy is evaluated on the synthesized plan BEFORE any tool dispatch
// (see daemon_pipeline.go Phase 7.5), so a runaway plan is caught
// before a single byte of value moves on-chain.
//
// Three layers of defence in the public stack, this is the second:
//
//	(1) Bridge-side ceiling — tools/paxeer/lib/tools.mjs reads
//	    PAXEER_MAX_SPEND_WEI on every write call and refuses values that
//	    exceed it. Last line of defence inside the agent process.
//
//	(2) PaxeerSpendPolicy (this file) — gates the FULL plan plan-time,
//	    so the user/operator sees one approval prompt covering every
//	    risky write the LLM intends to make, not N separate ones at
//	    dispatch time. Gives the panel/UI a single place to surface
//	    "approve this entire plan".
//
//	(3) Embedded Wallet REST API — the network-side custody enforces
//	    its own spend caps + allow-lists per Supabase user. Final
//	    authority; bridge cannot bypass it.
//
// The verdict surface is intentionally identical to GideonOpsPolicy so
// the existing pipeline plumbing (httpGateHandler / signTerminalFail)
// is reused verbatim.

import (
	"fmt"
	"math/big"
	"strings"
)

// SpendDecision is the closed enum of PaxeerSpendPolicy verdicts.
type SpendDecision int

const (
	// SpendAllow runs the tool call autonomously.
	SpendAllow SpendDecision = iota
	// SpendGate forces a mandatory human approval gate (per-call
	// value > cap, or aggregate plan value > aggregate cap).
	SpendGate
	// SpendDeny blocks the tool call outright (malformed value arg,
	// unknown write tool that the policy refuses to leave ungated).
	SpendDeny
)

// String renders the decision for transcript events + error messages.
func (d SpendDecision) String() string {
	switch d {
	case SpendAllow:
		return "allow"
	case SpendGate:
		return "gate"
	case SpendDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// Stable rule identifiers stamped on transcript events + gate prompts.
const (
	PaxeerRulePerCallCap   = "per_call_spend_cap"
	PaxeerRuleAggregateCap = "aggregate_plan_spend_cap"
	PaxeerRuleMalformed    = "malformed_value_arg"
)

// SpendEvaluation is the full result of evaluating one tool call. The
// bookkeeping fields (Tool, Args, ValueWei, RuleArg) are populated
// regardless of verdict so the audit log + gate prompt can show
// exactly what triggered the policy.
type SpendEvaluation struct {
	Decision SpendDecision
	Rule     string   // "" | PaxeerRulePerCallCap | PaxeerRuleAggregateCap | PaxeerRuleMalformed
	Reason   string   // human-readable explanation for the audit log / gate prompt
	Tool     string   // resolved tool name (e.g. "transfer")
	RuleArg  string   // arg key whose value carried the gated amount (e.g. "amount_wei")
	ValueWei *big.Int // parsed wei amount (nil for non-monetary writes)
}

// PaxeerSpendPolicy encodes the per-call + aggregate spend gates. Built
// once (DefaultPaxeerSpendPolicy) and held on daemonState; immutable at
// runtime so it is safe to share across goroutines.
type PaxeerSpendPolicy struct {
	// PerCallCapWei is the per-call hard ceiling. Any single tool call
	// whose value-bearing arg exceeds this gates. nil or zero disables
	// the per-call gate (still safe — the bridge-side ceiling and the
	// custody API enforce their own caps).
	PerCallCapWei *big.Int

	// AggregateCapWei is the per-plan ceiling across every gated write.
	// Catches "death by a thousand cuts" plans where each call is below
	// the per-call cap but the sum is large. nil or zero disables it.
	AggregateCapWei *big.Int

	// ValueArgs maps a paxeer-net write tool name to the ordered list of
	// arg keys whose value should be parsed as wei. The first present +
	// non-empty arg wins; an absent value is treated as zero (allowed).
	// Tools without an entry here are non-monetary writes (e.g.
	// stream_close, cancel_job, undelegate) and pass through.
	ValueArgs map[string][]string

	// PaxeerAlias is the MCP server alias the policy applies to. Tools
	// from other aliases (fs/git/fetch) are ignored, so the policy is
	// safe to enable unconditionally on the public-user daemon.
	PaxeerAlias string
}

// DefaultPaxeerSpendPolicy returns the production gate set.
//
// Caps:
//
//	PerCallCapWei   = 1 PAX  (1e18 wei) — single-call upper bound
//	AggregateCapWei = 5 PAX  (5e18 wei) — sum across the whole plan
//
// These are conservative defaults that gate any plan above pocket-
// change; operators tune them per-deployment via env vars consumed by
// the daemon's flag layer (see daemon_cmd.go: -paxeer-cap-wei /
// -paxeer-aggregate-cap-wei).
//
// Tool → value-arg mapping:
//
//	transfer        → amount_wei | amount | value
//	approve         → amount_wei | amount  (ERC-20 allowance set)
//	stream_open     → cap_wei | cap | total | total_wei
//	schedule_job    → deposit_wei | deposit | value_wei | value
//	delegate        → amount_wei | amount
//	redelegate      → amount_wei | amount
//	contract_write  → value_wei | value
func DefaultPaxeerSpendPolicy() *PaxeerSpendPolicy {
	onePax := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1e18
	fivePax := new(big.Int).Mul(onePax, big.NewInt(5))
	return &PaxeerSpendPolicy{
		PerCallCapWei:   onePax,
		AggregateCapWei: fivePax,
		PaxeerAlias:     "paxeer-net",
		ValueArgs: map[string][]string{
			"transfer":       {"amount_wei", "amount", "value"},
			"approve":        {"amount_wei", "amount"},
			"stream_open":    {"cap_wei", "cap", "total_wei", "total"},
			"schedule_job":   {"deposit_wei", "deposit", "value_wei", "value"},
			"delegate":       {"amount_wei", "amount"},
			"redelegate":     {"amount_wei", "amount"},
			"contract_write": {"value_wei", "value"},
		},
	}
}

// AppliesTo reports whether the policy gates calls against the given
// tool URI. Returns true only for paxeer-net writes whose tool name
// has an entry in ValueArgs. Cheap; safe to call on every tool node.
func (p *PaxeerSpendPolicy) AppliesTo(toolRef string) bool {
	if p == nil {
		return false
	}
	alias, name := splitMCPToolRef(toolRef)
	if alias != p.PaxeerAlias {
		return false
	}
	_, ok := p.ValueArgs[name]
	return ok
}

// Evaluate gates a single tool call against the per-call cap. Returns
// SpendAllow with ValueWei=nil for non-monetary writes (so callers can
// skip aggregate accumulation cleanly). Pure: no I/O, no state mutation.
func (p *PaxeerSpendPolicy) Evaluate(toolRef string, args map[string]string) SpendEvaluation {
	alias, toolName := splitMCPToolRef(toolRef)
	ev := SpendEvaluation{Decision: SpendAllow, Tool: toolName}
	if p == nil {
		return ev
	}
	if alias != p.PaxeerAlias {
		// Not a paxeer-net call — every other server's writes are not
		// the spend policy's concern.
		return ev
	}
	keys, ok := p.ValueArgs[toolName]
	if !ok {
		// Either a read tool or a non-monetary write (stream_close,
		// cancel_job, undelegate, sign_message, …). Allow.
		return ev
	}

	// Resolve the value-bearing arg.
	var (
		argKey string
		argRaw string
	)
	for _, k := range keys {
		if v, present := args[k]; present {
			argKey, argRaw = k, strings.TrimSpace(v)
			break
		}
	}
	if argRaw == "" {
		// No value supplied — treat as zero. Some tools (approve with
		// 'max', schedule_job with default deposit) legitimately omit
		// it. Bridge-side and custody-side will catch a true overspend.
		return ev
	}

	wei, perr := parseWei(argRaw)
	if perr != nil {
		ev.Decision = SpendDeny
		ev.Rule = PaxeerRuleMalformed
		ev.RuleArg = argKey
		ev.Reason = fmt.Sprintf(
			"tool %q arg %q has malformed wei value %q: %v",
			toolName, argKey, argRaw, perr)
		return ev
	}
	ev.RuleArg = argKey
	ev.ValueWei = wei

	if p.PerCallCapWei != nil && p.PerCallCapWei.Sign() > 0 && wei.Cmp(p.PerCallCapWei) > 0 {
		ev.Decision = SpendGate
		ev.Rule = PaxeerRulePerCallCap
		ev.Reason = fmt.Sprintf(
			"tool %q would spend %s wei via %q, exceeding per-call cap %s wei; "+
				"requires explicit human approval",
			toolName, wei.String(), argKey, p.PerCallCapWei.String())
		return ev
	}

	return ev
}

// EvaluateAggregate folds a sequence of per-call evaluations into a
// plan-level verdict. Returns the first hard verdict (deny > gate) or
// an aggregate-cap gate when the running total of allowed values
// crosses AggregateCapWei. Per-call gates short-circuit the aggregate
// check because the per-call gate already triggers the same broker.
func (p *PaxeerSpendPolicy) EvaluateAggregate(evs []SpendEvaluation) SpendEvaluation {
	if p == nil || len(evs) == 0 {
		return SpendEvaluation{Decision: SpendAllow}
	}
	total := new(big.Int)
	for _, ev := range evs {
		switch ev.Decision {
		case SpendDeny, SpendGate:
			return ev
		}
		if ev.ValueWei != nil {
			total.Add(total, ev.ValueWei)
		}
	}
	if p.AggregateCapWei != nil && p.AggregateCapWei.Sign() > 0 && total.Cmp(p.AggregateCapWei) > 0 {
		return SpendEvaluation{
			Decision: SpendGate,
			Rule:     PaxeerRuleAggregateCap,
			Tool:     "<aggregate>",
			ValueWei: total,
			Reason: fmt.Sprintf(
				"plan would spend %s wei across paxeer-net writes, exceeding aggregate cap %s wei; "+
					"requires explicit human approval",
				total.String(), p.AggregateCapWei.String()),
		}
	}
	return SpendEvaluation{Decision: SpendAllow}
}

// parseWei accepts a decimal or 0x-hex wei string and returns the
// non-negative *big.Int. Empty / whitespace returns 0. Negative or
// malformed input returns an error.
func parseWei(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return new(big.Int), nil
	}
	// Reject negatives outright — wei is unsigned.
	if strings.HasPrefix(s, "-") {
		return nil, fmt.Errorf("negative wei value")
	}
	v := new(big.Int)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		if _, ok := v.SetString(s[2:], 16); !ok {
			return nil, fmt.Errorf("invalid hex wei value")
		}
		return v, nil
	}
	if _, ok := v.SetString(s, 10); !ok {
		return nil, fmt.Errorf("invalid decimal wei value")
	}
	return v, nil
}

// parsePaxeerCapFlag interprets a cap-wei flag value:
//
//	""    → (nil, false)  — leave default in place
//	"-1"  → (nil, true)   — explicit disable for this layer
//	"0"   → (zero, true)  — explicit disable (Sign()==0 short-circuits)
//	"123" → (big.Int(123), true)
//
// Bad input is treated as "leave default" (false) so a typo can't
// silently weaken the policy.
func parsePaxeerCapFlag(raw string) (*big.Int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, false
	}
	if s == "-1" {
		return nil, true
	}
	v := new(big.Int)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		if _, ok := v.SetString(s[2:], 16); !ok {
			return nil, false
		}
		return v, true
	}
	if _, ok := v.SetString(s, 10); !ok {
		return nil, false
	}
	if v.Sign() < 0 {
		// Negative other than the explicit "-1" sentinel is rejected.
		return nil, false
	}
	return v, true
}

// paxeerCapForLog renders the configured cap (or "disabled" / "default")
// for the daemon.config boot event. If perCall is true the per-call
// cap is rendered; otherwise the aggregate cap.
func paxeerCapForLog(p *PaxeerSpendPolicy, perCall bool) string {
	if p == nil {
		return "policy_disabled"
	}
	v := p.PerCallCapWei
	if !perCall {
		v = p.AggregateCapWei
	}
	if v == nil {
		return "disabled"
	}
	return v.String()
}

// splitMCPToolRef extracts (alias, name) from a tool URI, e.g.
//
//	matrix://tool/mcp/paxeer-net/transfer@0.1.0  →  ("paxeer-net", "transfer")
//	matrix://tool/mcp/fs/read_text_file@2024.11  →  ("fs",         "read_text_file")
//
// Bare or malformed refs return ("", raw); callers treat unknown alias
// as "not our concern" and pass through.
func splitMCPToolRef(toolRef string) (alias, name string) {
	const prefix = "matrix://tool/mcp/"
	if !strings.HasPrefix(toolRef, prefix) {
		return "", toolRef
	}
	tail := toolRef[len(prefix):]
	// strip @version
	if at := strings.LastIndex(tail, "@"); at >= 0 {
		tail = tail[:at]
	}
	slash := strings.Index(tail, "/")
	if slash <= 0 || slash == len(tail)-1 {
		return "", toolRef
	}
	return tail[:slash], tail[slash+1:]
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
