// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package liveness

import (
	"encoding/json"
	"errors"
	"testing"
)

// The healthy baseline is the reference every enforcement test measures
// against: measured context clean, direct request, no prior repair. Every bound
// sits at its ceiling and the policy explains nothing, because nothing moved.
func TestHealthyBaselineDerivesTheUnmodulatedCeilings(t *testing.T) {
	policy, err := Derive(Inputs{Shape: ShapeDirect})
	if err != nil {
		t.Fatal(err)
	}
	if policy.SameStrategyRetries != 3 || policy.Parallelism != 4 ||
		policy.VerificationDepth != 2 || policy.ExplorationBudget != 4 ||
		policy.SearchBreadth != 4 || policy.OptionalWorkBudget != 4 ||
		policy.ToolCallBudget != 22 || policy.AskForHelp ||
		policy.ProposeFollowUpGoal {
		t.Fatalf("baseline policy = %+v", policy)
	}
	if len(policy.Causes) != 0 {
		t.Fatalf("healthy baseline explained itself: %+v", policy.Causes)
	}
	if !policy.RequiredVerification ||
		policy.SafetyInvariant != safetyInvariant {
		t.Fatalf("safety floor = %+v", policy)
	}
	// An unset shape is the direct shape: a caller that supplies no shape
	// cannot accidentally buy the research exploration ceiling.
	zero, err := Derive(Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if encode(t, zero) != encode(t, policy) {
		t.Fatalf("zero shape = %+v want %+v", zero, policy)
	}
}

// Measured context moves the bounds causally and the safety floor never moves
// with them. This is the counterfactual that proves the policy is derived from
// evidence rather than configured.
func TestMeasuredContextChangesBoundsWithoutTouchingSafety(t *testing.T) {
	baseline, err := Derive(Inputs{Shape: ShapeDirect})
	if err != nil {
		t.Fatal(err)
	}
	stressed, err := Derive(Inputs{
		Measured: MeasuredContext{
			UnsupportedPremises: 2, ActionsWithoutGrowth: 2,
		},
		Shape: ShapeDirect,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stressed.SameStrategyRetries >= baseline.SameStrategyRetries ||
		stressed.VerificationDepth <= baseline.VerificationDepth ||
		stressed.ToolCallBudget <= baseline.ToolCallBudget {
		t.Fatalf("policy did not causally change: baseline=%+v stressed=%+v",
			baseline, stressed)
	}
	if stressed.SameStrategyRetries != 1 ||
		stressed.VerificationDepth != 4 || !stressed.AskForHelp {
		t.Fatalf("stressed policy = %+v", stressed)
	}
	if !stressed.RequiredVerification ||
		stressed.SafetyInvariant != baseline.SafetyInvariant ||
		stressed.Parallelism != baseline.Parallelism {
		t.Fatalf("safety invariant changed: %+v", stressed)
	}
	if !hasCause(stressed, "evidence_depth") ||
		!hasCause(stressed, "strategy_revision") {
		t.Fatalf("provenance missing: %+v", stressed.Causes)
	}
	for _, cause := range stressed.Causes {
		if cause.Explanation == "" {
			t.Fatalf("cause without an explanation: %+v", cause)
		}
	}
}

// The measured task shape is the one lane that raises exploration, and the
// research shape is the same classification the acceptance gate uses.
func TestResearchShapeRaisesTheExplorationCeiling(t *testing.T) {
	direct, err := Derive(Inputs{Shape: ShapeDirect})
	if err != nil {
		t.Fatal(err)
	}
	research, err := Derive(Inputs{Shape: ShapeResearch})
	if err != nil {
		t.Fatal(err)
	}
	if research.ExplorationBudget != 8 || research.SearchBreadth != 6 ||
		research.ToolCallBudget != 26 {
		t.Fatalf("research policy = %+v", research)
	}
	if direct.ExplorationBudget >= research.ExplorationBudget ||
		direct.SearchBreadth >= research.SearchBreadth {
		t.Fatalf("shape did not move exploration: direct=%+v research=%+v",
			direct, research)
	}
	if !hasCause(research, "research_shape") || hasCause(direct, "research_shape") {
		t.Fatalf("shape provenance: direct=%+v research=%+v",
			direct.Causes, research.Causes)
	}
	for _, request := range []string{
		"Research the current state of the gateway.",
		"Look up the deployment history and cite sources.",
		"Find out what the latest news says.",
	} {
		if ClassifyShape(request) != ShapeResearch {
			t.Fatalf("classifier missed a research request: %q", request)
		}
	}
	for _, request := range []string{
		"Restart the daemon.",
		"Summarize the conversation so far.",
	} {
		if ClassifyShape(request) != ShapeDirect {
			t.Fatalf("classifier over-triggered: %q", request)
		}
	}
}

// A turn that already climbed a repair ladder tightens its own retry bound.
func TestPriorRepairTightensTheRetryBound(t *testing.T) {
	policy, err := Derive(Inputs{Shape: ShapeDirect, PriorRepair: true})
	if err != nil {
		t.Fatal(err)
	}
	if policy.SameStrategyRetries != 1 ||
		!hasCause(policy, "repair_prevention") {
		t.Fatalf("prior-repair policy = %+v", policy)
	}
}

// Derivation fails closed on impossible measured context, and Validate fails
// closed on any policy constructed outside the binding bounds.
func TestDerivationAndValidationFailClosed(t *testing.T) {
	if _, err := Derive(Inputs{
		Measured: MeasuredContext{UnsupportedPremises: -1},
	}); err == nil {
		t.Fatal("a negative premise count derived a policy")
	}
	if _, err := Derive(Inputs{
		Measured: MeasuredContext{ActionsWithoutGrowth: -3},
	}); err == nil {
		t.Fatal("a negative growth count derived a policy")
	}
	if _, err := Derive(Inputs{Shape: Shape("euphoric")}); err == nil {
		t.Fatal("an unknown task shape derived a policy")
	}
	valid, err := Derive(Inputs{Shape: ShapeDirect})
	if err != nil {
		t.Fatal(err)
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a derived policy failed its own validation: %v", err)
	}
	for name, mutate := range map[string]func(*Policy){
		"search breadth":        func(p *Policy) { p.SearchBreadth = 7 },
		"same strategy":         func(p *Policy) { p.SameStrategyRetries = 4 },
		"verification depth":    func(p *Policy) { p.VerificationDepth = 1 },
		"delegation bias":       func(p *Policy) { p.DelegationBias = .9 },
		"parallelism":           func(p *Policy) { p.Parallelism = 0 },
		"directness":            func(p *Policy) { p.Directness = .95 },
		"optional work":         func(p *Policy) { p.OptionalWorkBudget = 5 },
		"exploration budget":    func(p *Policy) { p.ExplorationBudget = 9 },
		"tool call budget low":  func(p *Policy) { p.ToolCallBudget = 9 },
		"tool call budget high": func(p *Policy) { p.ToolCallBudget = 33 },
		"required verification": func(p *Policy) { p.RequiredVerification = false },
		"safety invariant":      func(p *Policy) { p.SafetyInvariant = "  " },
		"response detail":       func(p *Policy) { p.ResponseDetail = "loud" },
	} {
		broken := valid
		mutate(&broken)
		if err := broken.Validate(); err == nil {
			t.Fatalf("validation accepted an out-of-bounds %s: %+v",
				name, broken)
		}
	}
}

// Property: no reachable measured context can derive a policy that escapes the
// binding bounds. Derive validates before returning, so this also proves the
// clamps hold across the whole reachable input space.
func TestEveryReachableContextDerivesABoundedPolicy(t *testing.T) {
	shapes := []Shape{ShapeDirect, ShapeResearch}
	for premises := 0; premises <= 40; premises++ {
		for growth := 0; growth <= 40; growth++ {
			for _, shape := range shapes {
				for _, repaired := range []bool{false, true} {
					policy, err := Derive(Inputs{
						Measured: MeasuredContext{
							UnsupportedPremises:  premises,
							ActionsWithoutGrowth: growth,
						},
						Shape: shape, PriorRepair: repaired,
					})
					if err != nil {
						t.Fatalf(
							"premises=%d growth=%d shape=%s repaired=%t: %v",
							premises, growth, shape, repaired, err,
						)
					}
					if err := policy.Validate(); err != nil {
						t.Fatalf("unbounded policy %+v: %v", policy, err)
					}
					if !policy.RequiredVerification ||
						policy.SafetyInvariant != safetyInvariant {
						t.Fatalf("safety floor moved: %+v", policy)
					}
				}
			}
		}
	}
}

// The exploration classifier is shared with the acceptance gate, so it has to
// cover every surface the runtime treats as open-web work.
func TestExplorationToolClassification(t *testing.T) {
	for _, name := range []string{
		"web_search", "web_fetch", "WEB_SEARCH", "research_sources",
		"fetch__fetch_page", "browser__fetch",
	} {
		if !IsExplorationTool(name) {
			t.Fatalf("exploration tool not classified: %q", name)
		}
	}
	for _, name := range []string{
		"exec__shell", "wallet_info", "memory_recall", "",
	} {
		if IsExplorationTool(name) {
			t.Fatalf("non-exploration tool classified as exploration: %q", name)
		}
	}
}

// The breaker key is canonical: two calls that differ only in JSON key order or
// whitespace are one strategy, and it is deliberately not the idempotency key.
func TestCanonicalToolSignatureIsArgumentOrderStable(t *testing.T) {
	left := CanonicalToolSignature(
		"exec__shell",
		json.RawMessage(`{"command":"ls","cwd":"/tmp"}`),
	)
	right := CanonicalToolSignature(
		"exec__shell",
		json.RawMessage("{\n  \"cwd\": \"/tmp\",\n  \"command\": \"ls\"\n}"),
	)
	if left != right {
		t.Fatalf("canonical signature is not order stable: %s vs %s",
			left, right)
	}
	if CanonicalToolSignature(
		"exec__service_list", json.RawMessage(`{"command":"ls","cwd":"/tmp"}`),
	) == left {
		t.Fatal("different tools share a signature")
	}
	if CanonicalToolSignature(
		"exec__shell", json.RawMessage(`{"command":"ls","cwd":"/var"}`),
	) == left {
		t.Fatal("different arguments share a signature")
	}
}

// The breaker counts cumulative repetition of one canonical signature, so
// interleaved churn — the pattern the legacy similarity heuristic was blind to
// — trips it, and once open it stays open at every phase.
func TestBreakerTripsOnInterleavedRepetitionAndStaysOpen(t *testing.T) {
	config := DefaultBreakerConfig()
	if config.RepeatThreshold != DefaultRepeatThreshold {
		t.Fatalf("breaker config = %+v", config)
	}
	state := BreakerState{}
	alpha := CanonicalToolSignature("exec__shell", json.RawMessage(`{"a":1}`))
	beta := CanonicalToolSignature("exec__shell", json.RawMessage(`{"b":2}`))
	for round := 0; round < DefaultRepeatThreshold; round++ {
		config.Observe(&state, alpha)
		config.Observe(&state, beta)
		if state.Open {
			t.Fatalf("breaker opened at round %d inside its threshold", round)
		}
		for _, phase := range []string{"turn", "provider", "tool"} {
			if err := state.Enforce(phase); err != nil {
				t.Fatalf("closed breaker refused the %s phase: %v", phase, err)
			}
		}
	}
	config.Observe(&state, alpha)
	if !state.Open || state.Signature != alpha || state.Reason == "" {
		t.Fatalf("breaker did not trip on cumulative repetition: %+v", state)
	}
	for _, phase := range []string{"turn", "provider", "tool"} {
		err := state.Enforce(phase)
		var circuit *CircuitError
		if !errors.As(err, &circuit) || circuit.Phase != phase ||
			circuit.Signature != alpha {
			t.Fatalf("phase %s enforcement = %#v", phase, err)
		}
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("phase %s error is not a circuit error: %v", phase, err)
		}
		if circuit.Error() == "" {
			t.Fatal("circuit error has no message")
		}
	}
	// Monotonic within a turn: fresh signatures never reclose it.
	config.Observe(&state, CanonicalToolSignature(
		"exec__service_list", json.RawMessage(`{}`),
	))
	if !state.Open || state.Signature != alpha {
		t.Fatalf("breaker reclosed: %+v", state)
	}
	// A zero config falls back to the default threshold rather than tripping
	// on the first dispatch.
	fresh := BreakerState{}
	var unset BreakerConfig
	unset.Observe(&fresh, alpha)
	if fresh.Open {
		t.Fatal("an unset breaker threshold tripped immediately")
	}
	// The ledger survives the checkpoint round trip it rides on.
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored BreakerState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.Open || restored.Signature != alpha ||
		restored.Signatures[alpha] != state.Signatures[alpha] {
		t.Fatalf("breaker state did not survive serialization: %+v", restored)
	}
}

func encode(t *testing.T, policy Policy) string {
	t.Helper()
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func hasCause(policy Policy, code string) bool {
	for _, cause := range policy.Causes {
		if cause.Code == code {
			return true
		}
	}
	return false
}
