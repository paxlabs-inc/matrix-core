// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package liveness derives bounded, inspectable execution parameters from the
// measured context of a turn. It has no authority over tool classification,
// authorization, evidence requirements, or safety policy: the safety invariant
// is constant and required verification is never reducible.
//
// The derivation shape, the clamps, Validate, and the Cause provenance are
// carried over from the ion liveness decision policy. What changed is where the
// inputs come from. Ion modulates on affect axes supplied by its emotional,
// temporal, and relationship subsystems; Matrix has none of those and the
// resurrection spec does not adopt them. Every affect axis is therefore
// replaced by the Matrix signal req.6.1 names, or — where Matrix measures
// nothing — by a documented neutral constant chosen so the affect term
// vanishes from ion's formula rather than being invented:
//
//   - confidence     -> neutralConfidence. Ion's confidence lane and its
//     unsupported-premise lane both feed verification depth; Matrix measures
//     unsupported premises directly, so the constant is ion's own baseline and
//     the premise count is the only term that moves.
//   - frustration    -> neutralFrustration. Same reasoning: ion's frustration
//     lane and its actions-without-growth lane both feed the retry bound, and
//     actions-without-growth is the measured one.
//   - fatigue        -> neutralLoad. Matrix has no sustained-work signal at the
//     turn boundary; parallelism starts at its ceiling and is bounded by the
//     loop's mechanical enforcement rather than by a guessed load axis.
//   - curiosity      -> the task shape. A request that asks for research earns
//     the full exploration ceiling; anything else gets ion's neutral budget.
//   - satisfaction   -> neutralSatisfaction. Proposing follow-up goals is
//     automatrix's lane, not the runtime's.
//   - urgency        -> neutralUrgency. Matrix carries no deadline signal into
//     a turn, so optional work starts at its ceiling.
//
// Ion's temporal stage, relationship profile, and aesthetic simplicity lanes
// are dropped with the subsystems that produced them; their cause codes go with
// them. PriorRepair survives because the loop measures it: a resumed turn that
// already climbed a repair ladder tightens its own retry bound.
package liveness

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

type ResponseDetail string

const (
	DetailConcise  ResponseDetail = "concise"
	DetailBalanced ResponseDetail = "balanced"
	DetailDetailed ResponseDetail = "detailed"
)

// Shape is the measured shape of the task the turn was asked to do. It is
// derived from the user request by a deterministic classifier, never inferred
// by a model.
type Shape string

const (
	ShapeDirect   Shape = "direct"
	ShapeResearch Shape = "research"
)

const (
	neutralConfidence   = .65
	neutralFrustration  = 0.
	neutralLoad         = 0.
	neutralSatisfaction = 0.
	neutralUrgency      = 0.

	directExploration   = .5
	researchExploration = 1.
)

const safetyInvariant = "liveness cannot reduce classification, approval, authorization, evidence, or required verification"

// Cause is a user-safe explanation of one measured-state-to-decision edge. It
// exposes the consequence and the provenance category, never premise text,
// memory content, or a claim about feelings.
type Cause struct {
	Code        string `json:"code"`
	Explanation string `json:"explanation"`
}

// Policy is the typed causal boundary between measured context and the loop.
// SafetyInvariant is constant and Validate fails closed if a caller attempts to
// construct a policy outside the supported bounds.
type Policy struct {
	SearchBreadth        int            `json:"search_breadth"`
	SameStrategyRetries  int            `json:"same_strategy_retries"`
	VerificationDepth    int            `json:"verification_depth"`
	DelegationBias       float64        `json:"delegation_bias"`
	Parallelism          int            `json:"parallelism"`
	ResponseDetail       ResponseDetail `json:"response_detail"`
	Directness           float64        `json:"directness"`
	AskForHelp           bool           `json:"ask_for_help"`
	OptionalWorkBudget   int            `json:"optional_work_budget"`
	ExplorationBudget    int            `json:"exploration_budget"`
	ToolCallBudget       int            `json:"tool_call_budget"`
	ProposeFollowUpGoal  bool           `json:"propose_follow_up_goal"`
	RequiredVerification bool           `json:"required_verification"`
	SafetyInvariant      string         `json:"safety_invariant"`
	Causes               []Cause        `json:"causes"`
}

// MeasuredContext is the non-secret epistemic counter set available at the loop
// boundary. It never includes premise text or memory content.
type MeasuredContext struct {
	UnsupportedPremises  int `json:"unsupported_premises"`
	ActionsWithoutGrowth int `json:"actions_without_growth"`
}

type Inputs struct {
	Measured    MeasuredContext
	Shape       Shape
	PriorRepair bool
}

// Derive applies continuous, bounded modulation over measured context. Required
// verification is always preserved.
func Derive(input Inputs) (Policy, error) {
	if input.Measured.UnsupportedPremises < 0 ||
		input.Measured.ActionsWithoutGrowth < 0 {
		return Policy{}, fmt.Errorf(
			"runtime liveness: evidence counts cannot be negative",
		)
	}
	exploration, err := explorationAxis(input.Shape)
	if err != nil {
		return Policy{}, err
	}

	result := Policy{
		SearchBreadth: 3, SameStrategyRetries: 3, VerificationDepth: 2,
		DelegationBias: .25, Parallelism: 3, ResponseDetail: DetailBalanced,
		Directness: .65, AskForHelp: false, OptionalWorkBudget: 3,
		ExplorationBudget: 4, ProposeFollowUpGoal: false,
		RequiredVerification: true,
		SafetyInvariant:      safetyInvariant,
	}

	// Unresolved evidence alters proof depth rather than safety.
	result.VerificationDepth = clampInt(
		2+int(math.Round((.65-neutralConfidence)*3))+
			input.Measured.UnsupportedPremises,
		2, 5,
	)
	result.Directness = clamp(.35+.55*neutralConfidence, .35, .9)
	if input.Measured.UnsupportedPremises > 0 {
		result.Causes = append(result.Causes, Cause{
			Code: "evidence_depth",
			Explanation: fmt.Sprintf(
				"Verification depth increased to %d because premise support is incomplete.",
				result.VerificationDepth,
			),
		})
	}

	// Evidence stagnation makes repetition cheaper than revision.
	result.SameStrategyRetries = clampInt(
		3-int(math.Round(neutralFrustration*2))-
			input.Measured.ActionsWithoutGrowth,
		0, 3,
	)
	result.AskForHelp = input.Measured.ActionsWithoutGrowth >= 2
	if result.SameStrategyRetries < 3 {
		result.Causes = append(result.Causes, Cause{
			Code: "strategy_revision",
			Explanation: fmt.Sprintf(
				"The same-strategy retry limit was reduced to %d after evidence stagnation increased.",
				result.SameStrategyRetries,
			),
		})
	}

	// Fan-out has a ceiling the loop enforces per batch.
	result.Parallelism = clampInt(4-int(math.Round(neutralLoad*3)), 1, 4)
	result.DelegationBias = clamp(.2+.65*neutralLoad, 0, .85)

	// The task shape has a hard idle-work budget even at its maximum.
	result.SearchBreadth = clampInt(2+int(math.Round(exploration*4)), 2, 6)
	result.ExplorationBudget = clampInt(int(math.Round(exploration*8)), 0, 8)
	result.ProposeFollowUpGoal = neutralSatisfaction >= .75
	if input.Shape == ShapeResearch {
		result.Causes = append(result.Causes, Cause{
			Code: "research_shape",
			Explanation: fmt.Sprintf(
				"Investigation breadth and exploration budget were raised to %d and %d because the request asks for researched evidence.",
				result.SearchBreadth, result.ExplorationBudget,
			),
		})
	}

	// Optional work collapses gradually, never required verification. Ion's
	// deadline_scope cause was dropped with the temporal lane that raised it.
	result.OptionalWorkBudget = clampInt(
		int(math.Round((1-neutralUrgency)*4)), 0, 4,
	)

	if input.PriorRepair {
		result.SameStrategyRetries = minInt(result.SameStrategyRetries, 1)
		result.Causes = append(result.Causes, Cause{
			Code:        "repair_prevention",
			Explanation: "The retry policy was tightened because this turn already ran a repair.",
		})
	}

	// The total action budget is enforced by the loop and remains sufficiently
	// large for the required evidence floor.
	result.ToolCallBudget = clampInt(
		4+result.VerificationDepth*3+result.OptionalWorkBudget*2+
			result.ExplorationBudget,
		10, 32,
	)
	sort.SliceStable(result.Causes, func(left, right int) bool {
		return result.Causes[left].Code < result.Causes[right].Code
	})
	if err := result.Validate(); err != nil {
		return Policy{}, err
	}
	return result, nil
}

func (policy Policy) Validate() error {
	if policy.SearchBreadth < 2 || policy.SearchBreadth > 6 ||
		policy.SameStrategyRetries < 0 || policy.SameStrategyRetries > 3 ||
		policy.VerificationDepth < 2 || policy.VerificationDepth > 5 ||
		policy.DelegationBias < 0 || policy.DelegationBias > .85 ||
		policy.Parallelism < 1 || policy.Parallelism > 4 ||
		policy.Directness < .35 || policy.Directness > .9 ||
		policy.OptionalWorkBudget < 0 || policy.OptionalWorkBudget > 4 ||
		policy.ExplorationBudget < 0 || policy.ExplorationBudget > 8 ||
		policy.ToolCallBudget < 10 || policy.ToolCallBudget > 32 ||
		!policy.RequiredVerification ||
		strings.TrimSpace(policy.SafetyInvariant) == "" {
		return fmt.Errorf("runtime liveness: policy is outside binding bounds")
	}
	switch policy.ResponseDetail {
	case DetailConcise, DetailBalanced, DetailDetailed:
	default:
		return fmt.Errorf("runtime liveness: invalid response detail")
	}
	return nil
}

// ClassifyShape is the one authoritative research-request classifier in the
// runtime. The acceptance gate's source requirement and the exploration budget
// read the same markers so the two can never drift apart.
func ClassifyShape(request string) Shape {
	normalized := " " + strings.ToLower(strings.TrimSpace(request)) + " "
	for _, marker := range []string{
		" research ", " look up ", " lookup ", " find out ",
		" investigate ", " search the web ", " web search ",
		" browse the web ", " tell me about ", " cite ",
		" citation ", " citations ", " cite your source ",
		" cite the source ", " cite sources ", " provide sources ",
		" include sources ", " with sources ", " reliable sources ",
		" authoritative sources ", " find sources ", " latest news ",
		" current news ",
	} {
		if strings.Contains(normalized, marker) {
			return ShapeResearch
		}
	}
	return ShapeDirect
}

// IsExplorationTool is the one authoritative exploration-tool classifier. The
// exploration budget and the acceptance gate's research-evidence read share it.
func IsExplorationTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "web_search" || name == "web_fetch" ||
		strings.Contains(name, "fetch") ||
		strings.HasPrefix(name, "research_")
}

func explorationAxis(shape Shape) (float64, error) {
	switch shape {
	case ShapeResearch:
		return researchExploration, nil
	case ShapeDirect, "":
		return directExploration, nil
	default:
		return 0, fmt.Errorf("runtime liveness: unknown task shape %q", shape)
	}
}

func clamp(value float64, low float64, high float64) float64 {
	return math.Max(low, math.Min(high, value))
}

func clampInt(value int, low int, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}
