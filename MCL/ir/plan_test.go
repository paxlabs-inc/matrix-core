// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package ir

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// makeValidPlan constructs a minimal but well-formed PlanTree covering
// every node kind. Reused across tests.
func makeValidPlan() *PlanTree {
	return &PlanTree{
		ID:        "01HZ0000000000000000000001",
		Version:   "mcl/0.1",
		IntentID:  "01HZ0000000000000000000002",
		CreatedAt: "2026-05-24T12:00:00Z",
		CreatedBy: "matrix://agent/did:pax:0xexec",
		SkillRef:  "matrix://skill/writing-plans@1.0.0",
		Root: PlanNode{
			ID:   "n1",
			Kind: NodeSequential,
			Children: []PlanNode{
				{
					ID:   "n2",
					Kind: NodeStep,
					Step: &StepPayload{
						PromptName:      "draft_plan",
						Inputs:          map[string]string{"target": "matrix://cortex/Fact/abc#1"},
						ExpectedOutputs: []string{"plan_draft"},
					},
				},
				{
					ID:   "n3",
					Kind: NodeParallel,
					Children: []PlanNode{
						{
							ID:   "n4",
							Kind: NodeToolCall,
							ToolCall: &ToolCallPayload{
								ToolRef:         "matrix://tool/mcp/fs/read_file@2024.11.1",
								Args:            map[string]string{"path": "/workspace/notes.md"},
								SideEffectClass: "read",
							},
						},
						{
							ID:   "n5",
							Kind: NodeToolCall,
							ToolCall: &ToolCallPayload{
								ToolRef:         "matrix://tool/mcp/fetch/get@0.6.0",
								Args:            map[string]string{"url": "https://example.com"},
								SideEffectClass: "network",
								TimeoutMs:       30000,
							},
						},
					},
				},
				{
					ID:   "n6",
					Kind: NodeSubDispatch,
					SubDispatch: &SubDispatchPayload{
						SkillRef: "matrix://skill/executing-plans@1.0.0",
					},
				},
				{
					ID:   "n7",
					Kind: NodeGate,
					Gate: &GatePayload{
						Question: "Proceed with deploy?",
						Options:  []string{"yes", "no"},
					},
				},
			},
		},
	}
}

func TestPlan_Hash_RoundTrip(t *testing.T) {
	plan := makeValidPlan()
	h1, err := HashPlan(plan)
	if err != nil {
		t.Fatalf("HashPlan: %v", err)
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-char sha256, got %d", len(h1))
	}

	// Hash field MUST be cleared during compute (self-referential)
	plan.Hash = h1
	h2, err := HashPlan(plan)
	if err != nil {
		t.Fatalf("HashPlan again: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash not stable across set: %s vs %s", h1, h2)
	}
	// Side-effect: HashPlan must restore Hash to its original value
	if plan.Hash != h1 {
		t.Fatalf("HashPlan did not restore Hash: got %q", plan.Hash)
	}
}

func TestPlan_CanonicalJSON_DeterministicOrder(t *testing.T) {
	plan := makeValidPlan()
	a, err := CanonicalJSONPlan(plan)
	if err != nil {
		t.Fatalf("CanonicalJSONPlan: %v", err)
	}
	b, err := CanonicalJSONPlan(plan)
	if err != nil {
		t.Fatalf("CanonicalJSONPlan repeat: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("canonical JSON not stable")
	}

	// Keys at every level must be sorted ascending. Spot-check root level.
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(a, &parsed); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	// We can't directly observe key order through map, but a re-canonicalise
	// of the parsed form must produce identical bytes.
	c, err := canonicalAny(parsed)
	if err != nil {
		t.Fatalf("re-canonicalise: %v", err)
	}
	if !bytes.Equal(a, c) {
		t.Fatalf("canonical JSON not idempotent under re-marshal")
	}
}

func TestPlan_CanonicalJSON_OmitsEmpty(t *testing.T) {
	plan := makeValidPlan()
	canon, err := CanonicalJSONPlan(plan)
	if err != nil {
		t.Fatalf("CanonicalJSONPlan: %v", err)
	}
	// Empty Hash field must NOT appear in canonical output
	if bytes.Contains(canon, []byte(`"hash":""`)) {
		t.Fatalf("empty hash field leaked into canonical: %s", canon)
	}
	// ModelDigest empty: must NOT appear
	if bytes.Contains(canon, []byte(`"model_digest"`)) {
		t.Fatalf("empty model_digest leaked into canonical: %s", canon)
	}
}

func TestPlan_HashChangesOnEdit(t *testing.T) {
	plan := makeValidPlan()
	h1, err := HashPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Root.Children[0].Step.PromptName = "different_prompt"
	h2, err := HashPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatalf("hash did not change after edit")
	}
}

func TestValidatePlan_Valid(t *testing.T) {
	if err := ValidatePlan(makeValidPlan()); err != nil {
		t.Fatalf("ValidatePlan: %v", err)
	}
}

func TestValidatePlan_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PlanTree)
		want error
	}{
		{"no_id", func(p *PlanTree) { p.ID = "" }, ErrPlanMissingID},
		{"no_intent_id", func(p *PlanTree) { p.IntentID = "" }, ErrPlanMissingIntentID},
		{"no_skill_ref", func(p *PlanTree) { p.SkillRef = "" }, ErrPlanMissingSkillRef},
		{"bare_skill", func(p *PlanTree) { p.SkillRef = "matrix://skill/writing-plans" }, ErrPlanSubSkillBareHead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := makeValidPlan()
			tc.mut(p)
			err := ValidatePlan(p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestValidatePlan_NodeKindInvariants(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PlanTree)
		want error
	}{
		{
			"unknown_kind",
			func(p *PlanTree) { p.Root.Children[0].Kind = "magic" },
			ErrPlanUnknownNodeKind,
		},
		{
			"terminal_has_children",
			func(p *PlanTree) {
				p.Root.Children[0].Children = []PlanNode{{ID: "x", Kind: NodeStep, Step: &StepPayload{PromptName: "p"}}}
			},
			ErrPlanTerminalHasChild,
		},
		{
			"branch_no_children",
			func(p *PlanTree) {
				p.Root.Children[1].Children = nil // n3 was Parallel
			},
			ErrPlanBranchEmpty,
		},
		{
			"duplicate_id",
			func(p *PlanTree) {
				p.Root.Children[0].ID = "n3" // collide with sibling
			},
			ErrPlanNodeDuplicateID,
		},
		{
			"step_kind_no_step_payload",
			func(p *PlanTree) {
				p.Root.Children[0].Step = nil
			},
			ErrPlanPayloadMismatch,
		},
		{
			"step_kind_with_extra_payload",
			func(p *PlanTree) {
				p.Root.Children[0].ToolCall = &ToolCallPayload{ToolRef: "matrix://tool/mcp/x/y@1.0.0"}
			},
			ErrPlanPayloadMismatch,
		},
		{
			"tool_call_bare_ref",
			func(p *PlanTree) {
				p.Root.Children[1].Children[0].ToolCall.ToolRef = "matrix://tool/mcp/fs/read_file"
			},
			ErrPlanToolRefBareHead,
		},
		{
			"tool_call_unknown_side_effect",
			func(p *PlanTree) {
				p.Root.Children[1].Children[0].ToolCall.SideEffectClass = "wizardry"
			},
			ErrPlanSideEffectUnknown,
		},
		{
			"sub_dispatch_bare_ref",
			func(p *PlanTree) {
				p.Root.Children[2].SubDispatch.SkillRef = "matrix://skill/executing-plans"
			},
			ErrPlanSubSkillBareHead,
		},
		{
			"gate_no_question",
			func(p *PlanTree) {
				p.Root.Children[3].Gate.Question = "   "
			},
			ErrPlanGateNoQuestion,
		},
		{
			"step_no_prompt_no_io",
			func(p *PlanTree) {
				p.Root.Children[0].Step = &StepPayload{}
			},
			ErrPlanStepNoPrompt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := makeValidPlan()
			tc.mut(p)
			err := ValidatePlan(p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestValidatePlan_BranchPayloadRejected(t *testing.T) {
	// Sequential / Parallel branches MUST NOT carry terminal payload
	p := makeValidPlan()
	p.Root.Step = &StepPayload{PromptName: "x"}
	err := ValidatePlan(p)
	if !errors.Is(err, ErrPlanPayloadMismatch) {
		t.Fatalf("want ErrPlanPayloadMismatch, got %v", err)
	}
}

func TestPlan_AllNodeKindsCovered(t *testing.T) {
	p := makeValidPlan()
	seen := map[string]bool{}
	var walk func(*PlanNode)
	walk = func(n *PlanNode) {
		seen[n.Kind] = true
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(&p.Root)
	for k := range ValidNodeKinds {
		if !seen[k] {
			t.Fatalf("makeValidPlan does not cover kind %q", k)
		}
	}
}

func TestPlan_IsVersionPinned(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"matrix://skill/foo", false},
		{"matrix://skill/foo@1.0.0", true},
		{"matrix://tool/mcp/fs/read_file@2024.11.1", true},
		{"matrix://skill/foo@sha256:abc123", true},
		{"matrix://skill/foo@1.0.0#fragment", true},
		{"matrix://skill/foo#fragment", false},
	}
	for _, tc := range cases {
		got := isVersionPinned(tc.in)
		if got != tc.want {
			t.Errorf("isVersionPinned(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestPlan_HashAcrossEncodeRoundtrip(t *testing.T) {
	// Encode → decode → re-encode → same hash. Verifies JSON tags are
	// symmetric and no field information is lost.
	p := makeValidPlan()
	h1, err := HashPlan(p)
	if err != nil {
		t.Fatal(err)
	}

	enc, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var p2 PlanTree
	if err := json.Unmarshal(enc, &p2); err != nil {
		t.Fatal(err)
	}
	h2, err := HashPlan(&p2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash unstable across round-trip: %s vs %s", h1, h2)
	}
}

func TestPlan_NilRejected(t *testing.T) {
	if err := ValidatePlan(nil); err == nil {
		t.Fatal("expected error on nil plan")
	}
	if !strings.Contains(ValidatePlan(nil).Error(), "nil PlanTree") {
		t.Fatalf("unexpected error message: %v", ValidatePlan(nil))
	}
}

// ---------------------------------------------------------------------------
// Session 31b · StepPayload.Kind (model router routing hint)
// ---------------------------------------------------------------------------

func TestStepKind_EmptyOmittedFromCanonical(t *testing.T) {
	// Backwards-compat: existing plans authored before P2 carry no Kind.
	// Canonical JSON MUST not introduce a "kind":"" key — that would break
	// the replay invariant for the 159 bulk-converted fixtures.
	plan := makeValidPlan()
	if plan.Root.Children[0].Step.Kind != "" {
		t.Fatalf("fixture expectation: makeValidPlan Step.Kind must be empty, got %q", plan.Root.Children[0].Step.Kind)
	}
	canon, err := CanonicalJSONPlan(plan)
	if err != nil {
		t.Fatalf("CanonicalJSONPlan: %v", err)
	}
	if bytes.Contains(canon, []byte(`"kind":""`)) {
		t.Fatalf("empty Step.Kind leaked into canonical: %s", canon)
	}
	// And specifically: a plan with an empty Step.Kind must hash identically
	// to the same plan with Step.Kind unset (literally the same in Go but
	// worth pinning in case the JSON tag drifts away from omitempty).
	plan2 := makeValidPlan()
	plan2.Root.Children[0].Step.Kind = ""
	h1, _ := HashPlan(plan)
	h2, _ := HashPlan(plan2)
	if h1 != h2 {
		t.Fatalf("hash drift between unset and empty Kind: %s vs %s", h1, h2)
	}
}

func TestStepKind_PresentRoundtrips(t *testing.T) {
	plan := makeValidPlan()
	plan.Root.Children[0].Step.Kind = "code"

	enc, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(enc, []byte(`"kind":"code"`)) {
		t.Fatalf("Kind missing from JSON: %s", enc)
	}

	var p2 PlanTree
	if err := json.Unmarshal(enc, &p2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := p2.Root.Children[0].Step.Kind; got != "code" {
		t.Fatalf("Kind did not round-trip: got %q want %q", got, "code")
	}

	// Hash MUST change when Kind changes — Kind is a content-addressed
	// field (routes to a different model, so it affects re-walk).
	h1, _ := HashPlan(plan)
	plan.Root.Children[0].Step.Kind = "summarize"
	h2, _ := HashPlan(plan)
	if h1 == h2 {
		t.Fatalf("hash did not change when Step.Kind changed")
	}
}

func TestValidatePlan_StepKindAccepted(t *testing.T) {
	for _, k := range StepKindNames {
		t.Run(k, func(t *testing.T) {
			p := makeValidPlan()
			p.Root.Children[0].Step.Kind = k
			if err := ValidatePlan(p); err != nil {
				t.Fatalf("kind=%q rejected: %v", k, err)
			}
		})
	}
	// Empty Kind also accepted (backwards-compat default).
	p := makeValidPlan()
	p.Root.Children[0].Step.Kind = ""
	if err := ValidatePlan(p); err != nil {
		t.Fatalf("empty Kind rejected: %v", err)
	}
}

func TestValidatePlan_StepKindRejected(t *testing.T) {
	cases := []string{
		"REASON",      // case mismatch
		"hard-reason", // hyphen vs underscore
		"think",       // not in closed set
		"  reason  ",  // unstripped whitespace
		"reason\n",    // trailing newline
	}
	for _, k := range cases {
		t.Run(k, func(t *testing.T) {
			p := makeValidPlan()
			p.Root.Children[0].Step.Kind = k
			err := ValidatePlan(p)
			if !errors.Is(err, ErrPlanStepKindUnknown) {
				t.Fatalf("kind=%q: want ErrPlanStepKindUnknown, got %v", k, err)
			}
		})
	}
}

func TestStepKindNames_ClosedEnum(t *testing.T) {
	if len(StepKindNames) != 7 {
		t.Fatalf("StepKindNames len = %d, want 7 (matches llm.AllStepKindNames closed enum)", len(StepKindNames))
	}
	if len(ValidStepKinds) != len(StepKindNames) {
		t.Fatalf("ValidStepKinds len %d != StepKindNames len %d (slice/map drift)",
			len(ValidStepKinds), len(StepKindNames))
	}
	// Every name in the slice must be in the map (caught by the builder
	// trivially, but pin the invariant against future hand-edits).
	for _, k := range StepKindNames {
		if !ValidStepKinds[k] {
			t.Errorf("StepKindNames lists %q but ValidStepKinds does not", k)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
