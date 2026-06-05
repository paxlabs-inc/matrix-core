// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package materiality implements the D9 materiality classifier per
// research/02-protocol.md §18.1. Live during plan walk per
// matrix.kvx executor_locked_design Q11 (line 707).
//
// Mapping from §18.1 verbatim:
//
//	Rule                        | Function
//	----------------------------|----------------------------------
//	Budget delta                | ruleBudgetDelta
//	New sub-agent               | ruleNewSubAgent
//	Scope expansion             | ruleScopeExpansion
//	New tool category           | ruleNewToolNamespace
//	Hard constraint relaxation  | ruleHardConstraintRelaxed
//	Success criteria change     | ruleSuccessCriteriaChanged
//	Deadline shift > 25%        | ruleDeadlineShift
//	Anchor flip                 | ruleAnchorFlip
//
// Each rule is independent; Classify runs them all and aggregates the
// reasons. Material if ANY rule fires. Reasons carry the rule name
// (machine-routable) plus a human-readable detail string.
//
// Non-material recipes (§18.1 second list) are NOT checked explicitly
// — if none of the 8 material rules fires, the modification is by
// definition non-material. The spec's non-material list is descriptive
// not prescriptive.
//
// The executor calls Classify on every intent.correct envelope mid-walk.
// Material → halt + lifecycle executing→accepted (S23Q9). Non-material
// → continue with journaled audit entry.
//
// Spec citations are inlined per rule in the rule comments below.
package materiality

import (
	"fmt"
	"strings"
	"time"

	"matrix/executor/tool"
	"matrix/mcl/ir"
)

// Inputs are the comparison surfaces for the §18.1 ruleset.
//
// Original* fields are the last-accepted plan + intent (what the user
// signed). New* fields are the post-correction candidates.
// OriginalAnchor / NewAnchor carry the D10 anchor flag from the
// corresponding intent.accept envelopes (Intent itself does not store
// anchor state; the accept envelope does — envelope/body.go:107).
//
// Now is the wall-clock used for deadline-shift remaining-window
// calculation. Zero defers to time.Now().UTC().
type Inputs struct {
	OriginalIntent *ir.Intent
	OriginalPlan   *ir.PlanTree
	NewIntent      *ir.Intent
	NewPlan        *ir.PlanTree

	OriginalAnchor bool
	NewAnchor      bool

	Now time.Time
}

// Classification is the outcome of a single classifier run.
type Classification struct {
	// Material is the binary D9 decision. True iff at least one rule fired.
	Material bool

	// Reasons enumerate every §18.1 rule that fired, in evaluation order.
	// Multiple rules may fire on a single correction.
	Reasons []Reason
}

// Reason names one §18.1 rule that fired.
type Reason struct {
	// Rule is one of the rule name constants (RuleBudgetDelta etc).
	// Use this for programmatic routing / metrics.
	Rule string

	// Detail is human-readable — e.g.
	// "expected_cost_new=12.50 PAX exceeds 1.10× original 10.00 PAX".
	// Surfaced into the intent.correct journal entry for audit.
	Detail string
}

// Rule name constants (machine-routable, stable across versions).
const (
	RuleBudgetDelta            = "budget_delta"
	RuleNewSubAgent            = "new_sub_agent"
	RuleScopeExpansion         = "scope_expansion"
	RuleNewToolNamespace       = "new_tool_namespace"
	RuleHardConstraintRelaxed  = "hard_constraint_relaxed"
	RuleSuccessCriteriaChanged = "success_criteria_changed"
	RuleDeadlineShift          = "deadline_shift"
	RuleAnchorFlip             = "anchor_flip"
)

// AllRules is the canonical ordered list of §18.1 rules.
var AllRules = []string{
	RuleBudgetDelta,
	RuleNewSubAgent,
	RuleScopeExpansion,
	RuleNewToolNamespace,
	RuleHardConstraintRelaxed,
	RuleSuccessCriteriaChanged,
	RuleDeadlineShift,
	RuleAnchorFlip,
}

// Thresholds from §18.1 — quoted verbatim from the spec.
const (
	// BudgetPercentThreshold: §18.1 "expected_cost_new > expected_cost_original × 1.10".
	BudgetPercentThreshold = 1.10

	// BudgetAbsoluteThresholdPAX: §18.1 "OR expected_cost_new − expected_cost_original > 5 PAX".
	BudgetAbsoluteThresholdPAX = 5.0

	// DeadlineShiftThreshold: §18.1 "New deadline moves the remaining-time
	// window by more than 25%".
	DeadlineShiftThreshold = 0.25
)

// Classify runs every §18.1 rule against the inputs and returns the
// aggregate Classification. Material iff at least one rule fires.
func Classify(in Inputs) Classification {
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	var reasons []Reason
	for _, fn := range []func(Inputs) *Reason{
		ruleBudgetDelta,
		ruleNewSubAgent,
		ruleScopeExpansion,
		ruleNewToolNamespace,
		ruleHardConstraintRelaxed,
		ruleSuccessCriteriaChanged,
		ruleDeadlineShift,
		ruleAnchorFlip,
	} {
		if r := fn(in); r != nil {
			reasons = append(reasons, *r)
		}
	}
	return Classification{
		Material: len(reasons) > 0,
		Reasons:  reasons,
	}
}

// ---- rule 1: Budget delta ----
//
// §18.1: "expected_cost_new > expected_cost_original × 1.10 OR
//
//	expected_cost_new − expected_cost_original > 5 PAX.
//	(Whichever is larger.)"
//
// Source of expected_cost: ir.Budget.MaxCost (intent or plan). Plan budget
// narrows intent budget (MCL/ir/plan.go:63-65); we read from plan when
// present, else from intent.
func ruleBudgetDelta(in Inputs) *Reason {
	orig, origAsset := budgetAmount(in.OriginalIntent, in.OriginalPlan)
	cand, candAsset := budgetAmount(in.NewIntent, in.NewPlan)

	// If either side has no budget, no rule fires. Adding a new budget
	// where none existed is not "delta" in §18.1 terms; it falls under
	// the hard-constraint-relaxed rule if applicable.
	if !orig.valid || !cand.valid {
		return nil
	}
	if origAsset != candAsset {
		// Asset switch (e.g. PAX → USD) is treated as a material
		// change — different monetary regime, can't compare amounts.
		return &Reason{
			Rule:   RuleBudgetDelta,
			Detail: fmt.Sprintf("budget asset changed: %s → %s", origAsset, candAsset),
		}
	}

	delta := cand.amount - orig.amount
	pctTrigger := false
	absTrigger := false
	if orig.amount > 0 && cand.amount > orig.amount*BudgetPercentThreshold {
		pctTrigger = true
	}
	if delta > BudgetAbsoluteThresholdPAX {
		absTrigger = true
	}
	if !pctTrigger && !absTrigger {
		return nil
	}
	return &Reason{
		Rule: RuleBudgetDelta,
		Detail: fmt.Sprintf("expected_cost_new=%.4f %s vs original=%.4f %s (delta=%.4f, pct_threshold=%v, abs_threshold=%v)",
			cand.amount, candAsset, orig.amount, origAsset, delta, pctTrigger, absTrigger),
	}
}

// budgetAmount returns the MaxCost from the plan (preferred) or intent.
type maybeBudget struct {
	amount float64
	valid  bool
}

func budgetAmount(intent *ir.Intent, plan *ir.PlanTree) (maybeBudget, string) {
	if plan != nil && plan.Budget != nil && plan.Budget.MaxCost != nil {
		return maybeBudget{amount: plan.Budget.MaxCost.Amount, valid: true}, plan.Budget.MaxCost.Asset
	}
	if intent != nil && intent.Budget != nil && intent.Budget.MaxCost != nil {
		return maybeBudget{amount: intent.Budget.MaxCost.Amount, valid: true}, intent.Budget.MaxCost.Asset
	}
	return maybeBudget{}, ""
}

// ---- rule 2: New sub-agent ----
//
// §18.1: "Any intent.dispatch to an AgentRef not present in the
//
//	originally-accepted plan."
//
// We approximate intent.dispatch presence via NodeSubDispatch.AgentRef
// in the PlanTree (the plan-time declaration of what dispatches will
// fire). A run-time dispatch to an AgentRef NOT pre-declared in the
// originally-accepted plan would be caught here when the corrected
// plan is the input.
func ruleNewSubAgent(in Inputs) *Reason {
	original := collectAgentRefs(in.OriginalPlan)
	candidate := collectAgentRefs(in.NewPlan)
	var added []string
	for ref := range candidate {
		if !original[ref] {
			added = append(added, ref)
		}
	}
	if len(added) == 0 {
		return nil
	}
	return &Reason{
		Rule:   RuleNewSubAgent,
		Detail: fmt.Sprintf("new AgentRef(s) not in original plan: %s", strings.Join(added, ", ")),
	}
}

func collectAgentRefs(plan *ir.PlanTree) map[string]bool {
	out := map[string]bool{}
	if plan == nil {
		return out
	}
	walkPlanNodes(&plan.Root, func(n *ir.PlanNode) {
		if n.Kind == ir.NodeSubDispatch && n.SubDispatch != nil && n.SubDispatch.AgentRef != "" {
			out[n.SubDispatch.AgentRef] = true
		}
	})
	return out
}

// walkPlanNodes is a simple recursive visitor over the plan tree.
// Not exported because callers should compose with the kind-specific
// extractors below rather than re-implement traversal.
func walkPlanNodes(n *ir.PlanNode, fn func(*ir.PlanNode)) {
	if n == nil {
		return
	}
	fn(n)
	for i := range n.Children {
		walkPlanNodes(&n.Children[i], fn)
	}
}

// ---- rule 3: Scope expansion ----
//
// §18.1: "Any CortexScope.include or query widening passed to an existing
//
//	sub-agent vs. the originally-accepted scope."
//
// We detect ScopeURI added/changed for an existing AgentRef. Full
// CortexScope set-difference would require resolving the Scope record
// from cortex (CortexScope lives in cortex/scope/ per Phase 10).
// v1 lock: signature-level change is the trigger; deeper Scope.include
// diffing is deferred to v1.1 when the materiality classifier can take
// the resolved Scope records as inputs.
func ruleScopeExpansion(in Inputs) *Reason {
	if in.OriginalPlan == nil || in.NewPlan == nil {
		return nil
	}
	original := collectAgentScope(in.OriginalPlan)
	candidate := collectAgentScope(in.NewPlan)
	var expanded []string
	for agent, newScope := range candidate {
		origScope, existed := original[agent]
		if !existed {
			continue // covered by ruleNewSubAgent
		}
		if origScope != newScope {
			expanded = append(expanded, fmt.Sprintf("agent=%s scope_uri=%q→%q", agent, origScope, newScope))
		}
	}
	if len(expanded) == 0 {
		return nil
	}
	return &Reason{
		Rule:   RuleScopeExpansion,
		Detail: "scope changed on existing sub-agent(s): " + strings.Join(expanded, "; "),
	}
}

func collectAgentScope(plan *ir.PlanTree) map[string]string {
	out := map[string]string{}
	walkPlanNodes(&plan.Root, func(n *ir.PlanNode) {
		if n.Kind == ir.NodeSubDispatch && n.SubDispatch != nil && n.SubDispatch.AgentRef != "" {
			out[n.SubDispatch.AgentRef] = n.SubDispatch.ScopeURI
		}
	})
	return out
}

// ---- rule 4: New tool namespace ----
//
// §18.1: "A ToolRef whose namespace (tools/<ns>/*) was not in the original
//
//	plan. Same-namespace swaps are non-material."
//
// We parse every ToolRef via tool.ParseToolURI (which already enforces
// the Q17 split into Provider/Server). The "namespace" is:
//
//   - For MCP tools: (Provider, Server). Two MCP tools differing only
//     in Name are same-namespace; differing in Server are different.
//   - For native tools: Provider alone (one of the 8 S6 namespaces).
//
// Unparseable tool refs surface as a material reason since they would
// fail validation anyway.
func ruleNewToolNamespace(in Inputs) *Reason {
	original := collectToolNamespaces(in.OriginalPlan)
	candidate := collectToolNamespaces(in.NewPlan)
	var added []string
	for ns := range candidate {
		if !original[ns] {
			added = append(added, ns)
		}
	}
	if len(added) == 0 {
		return nil
	}
	return &Reason{
		Rule:   RuleNewToolNamespace,
		Detail: "new tool namespace(s) not in original plan: " + strings.Join(added, ", "),
	}
}

func collectToolNamespaces(plan *ir.PlanTree) map[string]bool {
	out := map[string]bool{}
	if plan == nil {
		return out
	}
	walkPlanNodes(&plan.Root, func(n *ir.PlanNode) {
		if n.Kind != ir.NodeToolCall || n.ToolCall == nil {
			return
		}
		u, err := tool.ParseToolURI(n.ToolCall.ToolRef)
		if err != nil {
			// Unparseable URI: stamp the raw ref as its own
			// namespace so it triggers a material reason if it
			// wasn't present originally.
			out["unparseable:"+n.ToolCall.ToolRef] = true
			return
		}
		if u.IsMCP() {
			out["mcp/"+u.Server] = true
		} else {
			out[u.Provider] = true
		}
	})
	return out
}

// ---- rule 5: Hard constraint relaxation ----
//
// §18.1: "Any Constraint { hard: true } removed, or its bound loosened
//
//	(budget up, deadline later, quality min down, jurisdiction set
//	widened)."
//
// Comparison strategy:
//   - Build a map of original hard constraints keyed by (Type, identifier).
//   - For each candidate constraint, find the matching original and check
//     whether the bound moved in the loosening direction.
//   - For each original hard constraint missing from candidate, fire.
func ruleHardConstraintRelaxed(in Inputs) *Reason {
	if in.OriginalIntent == nil {
		return nil
	}
	origHard := indexHardConstraints(in.OriginalIntent.Frame.Constraints)
	if len(origHard) == 0 {
		return nil
	}
	candHard := indexHardConstraints(constraintsOf(in.NewIntent))

	var msgs []string
	for key, oc := range origHard {
		cc, present := candHard[key]
		if !present {
			msgs = append(msgs, fmt.Sprintf("removed hard constraint %s", key))
			continue
		}
		if loose, why := looser(oc, cc); loose {
			msgs = append(msgs, fmt.Sprintf("loosened hard constraint %s: %s", key, why))
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return &Reason{
		Rule:   RuleHardConstraintRelaxed,
		Detail: strings.Join(msgs, "; "),
	}
}

func constraintsOf(intent *ir.Intent) []ir.Constraint {
	if intent == nil {
		return nil
	}
	return intent.Frame.Constraints
}

// indexHardConstraints keys constraints by a stable identifier so the
// comparison can find matched pairs across the two intents.
func indexHardConstraints(list []ir.Constraint) map[string]ir.Constraint {
	out := map[string]ir.Constraint{}
	for _, c := range list {
		if !c.Hard {
			continue
		}
		key := c.Type
		// Rule and policy constraints disambiguate by their target ref.
		switch c.Type {
		case "rule":
			key = "rule:" + c.Rule
		case "policy":
			key = "policy:" + c.Policy
		case "quality":
			key = "quality:" + c.Metric
		case "x:custom":
			key = "x:" + c.Schema
		}
		out[key] = c
	}
	return out
}

// looser reports whether candidate is a looser version of the original.
// Returns (loose=false) on equal or tighter (non-material direction).
//
// "Loosening" semantics per §18.1:
//
//	budget   — Max higher
//	deadline — By later
//	quality  — Min lower
//	jurisdiction — Allow widened OR Deny narrowed
//	rule/policy/x:custom — any change (we can't reason about internal
//	                       loosening without resolving the ref)
func looser(orig, cand ir.Constraint) (bool, string) {
	switch orig.Type {
	case "budget":
		if orig.Max == nil || cand.Max == nil {
			// Removal of Max counts as loosening (no cap)
			if orig.Max != nil && cand.Max == nil {
				return true, "budget Max removed (no cap)"
			}
			return false, ""
		}
		if orig.Max.Asset != cand.Max.Asset {
			return true, fmt.Sprintf("budget asset changed: %s→%s", orig.Max.Asset, cand.Max.Asset)
		}
		if cand.Max.Amount > orig.Max.Amount {
			return true, fmt.Sprintf("budget Max %.4f→%.4f %s", orig.Max.Amount, cand.Max.Amount, orig.Max.Asset)
		}
	case "deadline":
		ot, oerr := parseTime(orig.By)
		ct, cerr := parseTime(cand.By)
		if oerr != nil || cerr != nil {
			// Unparseable deadlines treated as loosening (safer).
			return true, fmt.Sprintf("deadline parse error orig=%v cand=%v", oerr, cerr)
		}
		if ct.After(ot) {
			return true, fmt.Sprintf("deadline By %s→%s", orig.By, cand.By)
		}
	case "quality":
		if cand.Min < orig.Min {
			return true, fmt.Sprintf("quality.Min %g→%g", orig.Min, cand.Min)
		}
	case "jurisdiction":
		added := stringDifference(cand.Allow, orig.Allow)
		removed := stringDifference(orig.Deny, cand.Deny)
		if len(added) > 0 {
			return true, fmt.Sprintf("jurisdiction Allow widened by %v", added)
		}
		if len(removed) > 0 {
			return true, fmt.Sprintf("jurisdiction Deny narrowed by removing %v", removed)
		}
	case "rule", "policy", "x:custom":
		// We can't reason about internal loosening without resolving
		// the rule/policy ref. Treat any change in Data field as
		// loosening to err on the side of re-acceptance.
		if orig.Data != cand.Data || orig.Schema != cand.Schema ||
			orig.Rule != cand.Rule || orig.Policy != cand.Policy {
			return true, fmt.Sprintf("%s data/schema/ref changed", orig.Type)
		}
	}
	return false, ""
}

// stringDifference returns elements present in a but not in b.
func stringDifference(a, b []string) []string {
	bset := map[string]bool{}
	for _, s := range b {
		bset[s] = true
	}
	var diff []string
	for _, s := range a {
		if !bset[s] {
			diff = append(diff, s)
		}
	}
	return diff
}

// ---- rule 6: Success criteria change ----
//
// §18.1: "Any add, remove, or weakening of a Predicate in
//
//	frame.success_criteria."
//
// v1 detection: signature-level (Type + key identifying field). Any add
// or remove fires. Weakening of identically-typed predicates is hard to
// reason about without semantic understanding of the predicate's
// internal data; we treat parameter changes on the same (Type, key)
// pair as a material reason.
func ruleSuccessCriteriaChanged(in Inputs) *Reason {
	if in.OriginalIntent == nil && in.NewIntent == nil {
		return nil
	}
	origSet := indexPredicates(predicatesOf(in.OriginalIntent))
	candSet := indexPredicates(predicatesOf(in.NewIntent))

	var msgs []string
	for k, o := range origSet {
		c, present := candSet[k]
		if !present {
			msgs = append(msgs, fmt.Sprintf("removed predicate %s", k))
			continue
		}
		if !predicateEqual(o, c) {
			msgs = append(msgs, fmt.Sprintf("modified predicate %s", k))
		}
	}
	for k := range candSet {
		if _, present := origSet[k]; !present {
			msgs = append(msgs, fmt.Sprintf("added predicate %s", k))
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return &Reason{
		Rule:   RuleSuccessCriteriaChanged,
		Detail: strings.Join(msgs, "; "),
	}
}

func predicatesOf(intent *ir.Intent) []ir.Predicate {
	if intent == nil {
		return nil
	}
	return intent.Frame.SuccessCriteria
}

func indexPredicates(list []ir.Predicate) map[string]ir.Predicate {
	out := map[string]ir.Predicate{}
	for _, p := range list {
		out[predicateKey(p)] = p
	}
	return out
}

func predicateKey(p ir.Predicate) string {
	switch p.Type {
	case "delivered":
		return "delivered:" + p.Artifact
	case "signed_off":
		return "signed_off:" + p.By
	case "external":
		return "external:" + p.URL
	case "attestation":
		return "attestation:" + p.Source + "/" + p.Topic
	case "x:custom":
		return "x:" + p.Schema
	}
	return p.Type
}

func predicateEqual(a, b ir.Predicate) bool {
	return a.Type == b.Type &&
		a.Artifact == b.Artifact &&
		a.By == b.By &&
		a.URL == b.URL &&
		a.Check == b.Check &&
		a.Source == b.Source &&
		a.Topic == b.Topic &&
		a.Schema == b.Schema &&
		a.Data == b.Data
}

// ---- rule 7: Deadline shift > 25% ----
//
// §18.1: "New deadline moves the remaining-time window by more than 25%."
//
// remaining_orig = original_deadline − now
// remaining_new  = new_deadline − now
// shift_ratio    = |remaining_new − remaining_orig| / |remaining_orig|
// material iff shift_ratio > 0.25.
//
// Edge cases:
//   - Adding a deadline where none existed → material (new constraint)
//   - Removing a deadline → material (removed deadline = unbounded)
//   - Original deadline already in the past → cannot compute remaining
//     window; treat any change as material.
func ruleDeadlineShift(in Inputs) *Reason {
	origStr := deadlineOf(in.OriginalIntent)
	newStr := deadlineOf(in.NewIntent)
	if origStr == "" && newStr == "" {
		return nil
	}
	if origStr == "" && newStr != "" {
		return &Reason{Rule: RuleDeadlineShift, Detail: "deadline added: " + newStr}
	}
	if origStr != "" && newStr == "" {
		return &Reason{Rule: RuleDeadlineShift, Detail: "deadline removed (was " + origStr + ")"}
	}

	ot, oerr := parseTime(origStr)
	nt, nerr := parseTime(newStr)
	if oerr != nil || nerr != nil {
		return &Reason{Rule: RuleDeadlineShift,
			Detail: fmt.Sprintf("deadline parse error (orig=%v cand=%v)", oerr, nerr)}
	}

	remOrig := ot.Sub(in.Now)
	remNew := nt.Sub(in.Now)
	if remOrig <= 0 {
		// Original already expired — any change is treated as material.
		return &Reason{Rule: RuleDeadlineShift,
			Detail: fmt.Sprintf("original deadline %s already past now=%s; new=%s",
				origStr, in.Now.Format(time.RFC3339), newStr)}
	}

	deltaAbs := remNew - remOrig
	if deltaAbs < 0 {
		deltaAbs = -deltaAbs
	}
	ratio := float64(deltaAbs) / float64(remOrig)
	if ratio <= DeadlineShiftThreshold {
		return nil
	}
	return &Reason{
		Rule: RuleDeadlineShift,
		Detail: fmt.Sprintf("deadline shifted %.1f%% (orig remaining=%s, new remaining=%s)",
			ratio*100, remOrig.Truncate(time.Second), remNew.Truncate(time.Second)),
	}
}

func deadlineOf(intent *ir.Intent) string {
	if intent == nil {
		return ""
	}
	return intent.Deadline
}

// parseTime tries RFC3339 then RFC3339Nano then date-only. Returns the
// first successful parse.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable time %q", s)
}

// ---- rule 8: Anchor flip ----
//
// §18.1: "The plan changes from anchor-off to anchor-on (per D10), or
//
//	vice-versa."
func ruleAnchorFlip(in Inputs) *Reason {
	if in.OriginalAnchor == in.NewAnchor {
		return nil
	}
	dir := "off→on"
	if in.OriginalAnchor && !in.NewAnchor {
		dir = "on→off"
	}
	return &Reason{
		Rule:   RuleAnchorFlip,
		Detail: "anchor flag flipped " + dir,
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
