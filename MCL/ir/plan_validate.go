// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package ir

import (
	"errors"
	"fmt"
	"strings"
)

// Plan validation errors.
var (
	ErrPlanMissingID         = errors.New("ir: PlanTree.ID required")
	ErrPlanMissingIntentID   = errors.New("ir: PlanTree.IntentID required")
	ErrPlanMissingSkillRef   = errors.New("ir: PlanTree.SkillRef required")
	ErrPlanUnknownNodeKind   = errors.New("ir: unknown PlanNode.Kind")
	ErrPlanNodeMissingID     = errors.New("ir: PlanNode.ID required")
	ErrPlanNodeDuplicateID   = errors.New("ir: duplicate PlanNode.ID")
	ErrPlanTerminalHasChild  = errors.New("ir: terminal PlanNode has children")
	ErrPlanBranchEmpty       = errors.New("ir: branch PlanNode has no children")
	ErrPlanPayloadMismatch   = errors.New("ir: PlanNode payload does not match Kind")
	ErrPlanToolRefBareHead   = errors.New("ir: PlanNode.ToolCall.ToolRef must be version-pinned")
	ErrPlanSubSkillBareHead  = errors.New("ir: PlanNode.SubDispatch.SkillRef must be version-pinned")
	ErrPlanSideEffectUnknown = errors.New("ir: PlanNode.ToolCall.SideEffectClass not in closed enum")
	ErrPlanStepNoPrompt      = errors.New("ir: PlanNode.Step needs inputs or expected_outputs")
	ErrPlanStepKindUnknown   = errors.New("ir: PlanNode.Step.Kind not in ValidStepKinds")
	ErrPlanGateNoQuestion    = errors.New("ir: PlanNode.Gate.Question required")
)

// ValidatePlan checks structural invariants of a PlanTree per
// research/05-skills-and-tools.md §3.2 and the Session 21 locked design.
// Returns nil if the plan is well-formed.
//
// Invariants enforced:
//  1. PlanTree.ID, IntentID, SkillRef populated
//  2. Every PlanNode has a unique non-empty ID within the tree
//  3. Every PlanNode.Kind is in ValidNodeKinds
//  4. Branch kinds (Sequential, Parallel) have ≥1 child and no terminal payload
//  5. Terminal kinds (Step, ToolCall, SubDispatch, Gate) have no children
//     and exactly the matching typed payload populated
//  6. ToolCall.ToolRef + SubDispatch.SkillRef are version-pinned (S4 hard rule)
//  7. ToolCall.SideEffectClass (if set) is in ValidSideEffectClasses
//  8. Gate.Question is non-empty
func ValidatePlan(plan *PlanTree) error {
	if plan == nil {
		return errors.New("ir: nil PlanTree")
	}
	if plan.ID == "" {
		return ErrPlanMissingID
	}
	if plan.IntentID == "" {
		return ErrPlanMissingIntentID
	}
	if plan.SkillRef == "" {
		return ErrPlanMissingSkillRef
	}
	if !isVersionPinned(plan.SkillRef) {
		return fmt.Errorf("%w: %q", ErrPlanSubSkillBareHead, plan.SkillRef)
	}

	seen := make(map[string]bool)
	return validatePlanNode(&plan.Root, seen)
}

func validatePlanNode(n *PlanNode, seen map[string]bool) error {
	if n.ID == "" {
		return ErrPlanNodeMissingID
	}
	if seen[n.ID] {
		return fmt.Errorf("%w: %q", ErrPlanNodeDuplicateID, n.ID)
	}
	seen[n.ID] = true

	if !ValidNodeKinds[n.Kind] {
		return fmt.Errorf("%w: %q (node %s)", ErrPlanUnknownNodeKind, n.Kind, n.ID)
	}

	terminal := isTerminalKind(n.Kind)
	if terminal && len(n.Children) > 0 {
		return fmt.Errorf("%w: node %s kind=%s has %d children", ErrPlanTerminalHasChild, n.ID, n.Kind, len(n.Children))
	}
	if !terminal && len(n.Children) == 0 {
		return fmt.Errorf("%w: node %s kind=%s", ErrPlanBranchEmpty, n.ID, n.Kind)
	}

	// Payload-Kind correspondence
	if err := validateNodePayload(n); err != nil {
		return err
	}

	for i := range n.Children {
		if err := validatePlanNode(&n.Children[i], seen); err != nil {
			return err
		}
	}
	return nil
}

func validateNodePayload(n *PlanNode) error {
	// Count populated terminal payloads
	populated := 0
	if n.Step != nil {
		populated++
	}
	if n.ToolCall != nil {
		populated++
	}
	if n.SubDispatch != nil {
		populated++
	}
	if n.Gate != nil {
		populated++
	}

	switch n.Kind {
	case NodeSequential, NodeParallel:
		if populated > 0 {
			return fmt.Errorf("%w: branch node %s has terminal payload", ErrPlanPayloadMismatch, n.ID)
		}
		return nil

	case NodeStep:
		if n.Step == nil {
			return fmt.Errorf("%w: kind=step but Step payload nil (node %s)", ErrPlanPayloadMismatch, n.ID)
		}
		if populated > 1 {
			return fmt.Errorf("%w: kind=step has %d payloads (node %s)", ErrPlanPayloadMismatch, populated, n.ID)
		}
		// A Step with no inputs and no expected outputs is meaningless
		if n.Step.PromptName == "" && len(n.Step.Inputs) == 0 && len(n.Step.ExpectedOutputs) == 0 {
			return fmt.Errorf("%w: node %s", ErrPlanStepNoPrompt, n.ID)
		}
		// Step.Kind (Session 31b model router) — empty is fine and routes
		// to "reason" at executor time; an explicit unknown value is a
		// planner bug and we reject it loudly so it surfaces in CI rather
		// than silently routing to the fallback model.
		if n.Step.Kind != "" && !ValidStepKinds[n.Step.Kind] {
			return fmt.Errorf("%w: %q (node %s)", ErrPlanStepKindUnknown, n.Step.Kind, n.ID)
		}
		return nil

	case NodeToolCall:
		if n.ToolCall == nil {
			return fmt.Errorf("%w: kind=tool_call but ToolCall payload nil (node %s)", ErrPlanPayloadMismatch, n.ID)
		}
		if populated > 1 {
			return fmt.Errorf("%w: kind=tool_call has %d payloads (node %s)", ErrPlanPayloadMismatch, populated, n.ID)
		}
		if !isVersionPinned(n.ToolCall.ToolRef) {
			return fmt.Errorf("%w: %q (node %s)", ErrPlanToolRefBareHead, n.ToolCall.ToolRef, n.ID)
		}
		if n.ToolCall.SideEffectClass != "" && !ValidSideEffectClasses[n.ToolCall.SideEffectClass] {
			return fmt.Errorf("%w: %q (node %s)", ErrPlanSideEffectUnknown, n.ToolCall.SideEffectClass, n.ID)
		}
		return nil

	case NodeSubDispatch:
		if n.SubDispatch == nil {
			return fmt.Errorf("%w: kind=sub_dispatch but SubDispatch payload nil (node %s)", ErrPlanPayloadMismatch, n.ID)
		}
		if populated > 1 {
			return fmt.Errorf("%w: kind=sub_dispatch has %d payloads (node %s)", ErrPlanPayloadMismatch, populated, n.ID)
		}
		if !isVersionPinned(n.SubDispatch.SkillRef) {
			return fmt.Errorf("%w: %q (node %s)", ErrPlanSubSkillBareHead, n.SubDispatch.SkillRef, n.ID)
		}
		return nil

	case NodeGate:
		if n.Gate == nil {
			return fmt.Errorf("%w: kind=gate but Gate payload nil (node %s)", ErrPlanPayloadMismatch, n.ID)
		}
		if populated > 1 {
			return fmt.Errorf("%w: kind=gate has %d payloads (node %s)", ErrPlanPayloadMismatch, populated, n.ID)
		}
		if strings.TrimSpace(n.Gate.Question) == "" {
			return fmt.Errorf("%w: node %s", ErrPlanGateNoQuestion, n.ID)
		}
		return nil
	}

	return fmt.Errorf("%w: %q (node %s)", ErrPlanUnknownNodeKind, n.Kind, n.ID)
}

// isTerminalKind reports whether a PlanNode of this kind has no children.
func isTerminalKind(kind string) bool {
	switch kind {
	case NodeStep, NodeToolCall, NodeSubDispatch, NodeGate:
		return true
	}
	return false
}

// isVersionPinned reports whether a matrix:// URI carries an explicit
// @version or @sha256:... suffix. Bare heads (e.g. matrix://skill/foo)
// are rejected per S4 hard rule (research/05-skills-and-tools.md §S4).
func isVersionPinned(uri string) bool {
	if uri == "" {
		return false
	}
	// Strip optional fragment
	if i := strings.IndexByte(uri, '#'); i >= 0 {
		uri = uri[:i]
	}
	// Must contain @ after the matrix:// prefix
	return strings.Contains(uri, "@")
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
