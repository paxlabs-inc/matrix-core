// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"fmt"
	"strings"
	"testing"

	"matrix/executor/runtime"
	"matrix/executor/tool"
	"matrix/mcl/ir"
)

// fakeManifest builds a minimal AgentManifest with two tools across
// two servers — enough surface area to exercise the allow-list filter
// without spinning up real MCP servers.
func fakeManifest() *tool.AgentManifest {
	return &tool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "did:pax:0xtest",
		Servers: []tool.ServerEntry{
			{
				Alias:         "fs",
				Transport:     "stdio",
				Command:       "/bin/true",
				Version:       "2024.11.1",
				PackageDigest: "sha256:" + strings.Repeat("a", 64),
				Tools: []tool.ToolEntry{
					{Name: "read_text_file", SideEffectClass: "read", Description: "read text"},
					{Name: "directory_tree", SideEffectClass: "read", Description: "tree view"},
				},
			},
			{
				Alias:         "git",
				Transport:     "stdio",
				Command:       "/bin/true",
				Version:       "2024.11.1",
				PackageDigest: "sha256:" + strings.Repeat("b", 64),
				Tools: []tool.ToolEntry{
					{Name: "git_log", SideEffectClass: "read", Description: "log"},
					{Name: "git_status", SideEffectClass: "read", Description: "status"},
				},
			},
		},
	}
}

func TestBuildSystemPrompt_ToolsNone(t *testing.T) {
	skill := &runtime.LoadedSkill{
		URI:       "matrix://skill/brainstorming@0.1.0",
		ToolsNone: true,
	}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "/data/workspace")

	if !strings.Contains(prompt, "§TOOLS = none") {
		t.Errorf("prompt missing §TOOLS=none banner:\n%s", prompt)
	}
	if !strings.Contains(prompt, "DO NOT emit any tool_call nodes") {
		t.Errorf("prompt missing strong negative for tool_call:\n%s", prompt)
	}
	// No tool URIs should be enumerated when §TOOLS=none.
	if strings.Contains(prompt, "matrix://tool/mcp/fs/read_text_file") {
		t.Errorf("prompt leaks fs tool when §TOOLS=none")
	}
	if strings.Contains(prompt, "matrix://tool/mcp/git/git_log") {
		t.Errorf("prompt leaks git tool when §TOOLS=none")
	}
	if !strings.Contains(prompt, "/data/workspace") {
		t.Errorf("prompt missing workspace root section")
	}
}

func TestBuildSystemPrompt_AllowList(t *testing.T) {
	skill := &runtime.LoadedSkill{
		URI: "matrix://skill/scoped@1.0.0",
		DeclaredTools: []string{
			"matrix://tool/mcp/fs/read_text_file@2024.11.1",
		},
	}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "/data/workspace")

	if !strings.Contains(prompt, "matrix://tool/mcp/fs/read_text_file@2024.11.1") {
		t.Errorf("prompt missing allowed tool:\n%s", prompt)
	}
	if strings.Contains(prompt, "matrix://tool/mcp/fs/directory_tree") {
		t.Errorf("prompt leaks non-allowlisted directory_tree")
	}
	if strings.Contains(prompt, "matrix://tool/mcp/git/git_log") {
		t.Errorf("prompt leaks non-allowlisted git_log")
	}
}

func TestBuildSystemPrompt_NoSkillSection_FallsBackToFullManifest(t *testing.T) {
	// CLI walk_cmd path: skill manifest absent / nil allow-list, no
	// `none` flag — historic behavior shows every manifest tool.
	skill := &runtime.LoadedSkill{
		URI: "matrix://skill/legacy@1.0.0",
	}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "")

	for _, want := range []string{
		"matrix://tool/mcp/fs/read_text_file@2024.11.1",
		"matrix://tool/mcp/fs/directory_tree@2024.11.1",
		"matrix://tool/mcp/git/git_log@2024.11.1",
		"matrix://tool/mcp/git/git_status@2024.11.1",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q in fallback enumeration", want)
		}
	}
	// Workspace section omitted when WorkspaceRoot is empty.
	if strings.Contains(prompt, "== Workspace ==") {
		t.Errorf("prompt has workspace section despite empty root")
	}
}

func TestVerifySkillAllowList_RejectsToolCallWhenNone(t *testing.T) {
	skill := &runtime.LoadedSkill{ToolsNone: true}
	plan := &ir.PlanTree{
		Root: ir.PlanNode{
			ID:   "n0",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{
					ID:   "n1",
					Kind: ir.NodeToolCall,
					ToolCall: &ir.ToolCallPayload{
						ToolRef: "matrix://tool/mcp/fs/read_text_file@2024.11.1",
					},
				},
			},
		},
	}
	err := verifySkillAllowList(plan, skill)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "§TOOLS none") {
		t.Errorf("error doesn't mention §TOOLS none: %v", err)
	}
}

func TestVerifySkillAllowList_RejectsUnlistedTool(t *testing.T) {
	skill := &runtime.LoadedSkill{
		DeclaredTools: []string{
			"matrix://tool/mcp/fs/read_text_file@2024.11.1",
		},
	}
	plan := &ir.PlanTree{
		Root: ir.PlanNode{
			ID:   "n0",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{
					ID:   "n1",
					Kind: ir.NodeToolCall,
					ToolCall: &ir.ToolCallPayload{
						ToolRef: "matrix://tool/mcp/git/git_log@2024.11.1",
					},
				},
			},
		},
	}
	err := verifySkillAllowList(plan, skill)
	if err == nil {
		t.Fatalf("expected error for non-allowlisted tool")
	}
	if !strings.Contains(err.Error(), "not in skill §TOOLS allow-list") {
		t.Errorf("error doesn't mention allow-list violation: %v", err)
	}
}

func TestVerifySkillAllowList_AcceptsAllowedTool(t *testing.T) {
	skill := &runtime.LoadedSkill{
		DeclaredTools: []string{
			"matrix://tool/mcp/fs/read_text_file@2024.11.1",
		},
	}
	plan := &ir.PlanTree{
		Root: ir.PlanNode{
			ID:   "n0",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{
					ID:   "n1",
					Kind: ir.NodeToolCall,
					ToolCall: &ir.ToolCallPayload{
						ToolRef: "matrix://tool/mcp/fs/read_text_file@2024.11.1",
					},
				},
			},
		},
	}
	if err := verifySkillAllowList(plan, skill); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestVerifySkillAllowList_UnconstrainedSkill_AnyToolOK(t *testing.T) {
	// No DeclaredTools, no ToolsNone → "no enforcement" mode (the
	// CLI walk_cmd posture for ad-hoc plans).
	skill := &runtime.LoadedSkill{}
	plan := &ir.PlanTree{
		Root: ir.PlanNode{
			ID:   "n0",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{
					ID:   "n1",
					Kind: ir.NodeToolCall,
					ToolCall: &ir.ToolCallPayload{
						ToolRef: "matrix://tool/mcp/anything/goes@1.0.0",
					},
				},
			},
		},
	}
	if err := verifySkillAllowList(plan, skill); err != nil {
		t.Errorf("unconstrained skill should accept any tool: %v", err)
	}
}

func TestVerifySkillAllowList_RejectsSubDispatchWhenNone(t *testing.T) {
	skill := &runtime.LoadedSkill{SubSkillsNone: true}
	plan := &ir.PlanTree{
		Root: ir.PlanNode{
			ID:   "n0",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{
					ID:   "n1",
					Kind: ir.NodeSubDispatch,
					SubDispatch: &ir.SubDispatchPayload{
						SkillRef: "matrix://skill/writing-plans@1.0.0",
					},
				},
			},
		},
	}
	err := verifySkillAllowList(plan, skill)
	if err == nil {
		t.Fatalf("expected error for sub_dispatch when §SUB_SKILLS=none")
	}
	if !strings.Contains(err.Error(), "§SUB_SKILLS none") {
		t.Errorf("error doesn't mention §SUB_SKILLS none: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Session 31b · Step-kind hint surfacing in planner system prompt (P2b)
// ---------------------------------------------------------------------------

// stepKindMtx builds a minimal SKILL.mtx with the given on-block kind
// annotations baked into the §PROCEDURE section. verbKinds maps verb
// names to kind values (e.g. {"build":"code"}).
func stepKindMtx(verbKinds map[string]string) []byte {
	var sb strings.Builder
	sb.WriteString("\u00a7SKILL\nid=test\nmcl.verbs=")
	verbs := make([]string, 0, len(verbKinds))
	for v := range verbKinds {
		verbs = append(verbs, v)
	}
	for i, v := range verbs {
		if i > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(v)
	}
	sb.WriteString("\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\n")
	for _, v := range verbs {
		fmt.Fprintf(&sb, "on verb=%s\n  kind = %q\nend\n", v, verbKinds[v])
	}
	sb.WriteString("\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	return []byte(sb.String())
}

func TestExtractStepKindHints_None(t *testing.T) {
	if got := extractStepKindHints(nil); got != nil {
		t.Errorf("nil bytes: got %v, want nil", got)
	}
	if got := extractStepKindHints([]byte{}); got != nil {
		t.Errorf("empty bytes: got %v, want nil", got)
	}
}

func TestExtractStepKindHints_SingleVerb(t *testing.T) {
	mtx := stepKindMtx(map[string]string{"build": "code"})
	hints := extractStepKindHints(mtx)
	if hints["build"] != "code" {
		t.Errorf("hints[build] = %q, want code (full hints: %v)", hints["build"], hints)
	}
}

func TestExtractStepKindHints_MultipleVerbs(t *testing.T) {
	mtx := stepKindMtx(map[string]string{
		"build":   "code",
		"analyze": "summarize",
		"deliver": "write",
	})
	hints := extractStepKindHints(mtx)
	want := map[string]string{"build": "code", "analyze": "summarize", "deliver": "write"}
	for v, k := range want {
		if hints[v] != k {
			t.Errorf("hints[%s] = %q, want %q (full hints: %v)", v, hints[v], k, hints)
		}
	}
}

func TestExtractStepKindHints_NoAnnotations(t *testing.T) {
	// SKILL.mtx with an on-block but no kind= KV: must return nil
	// (vs an empty non-nil map) so callers can short-circuit cheaply.
	mtx := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=build\n  prompt\n    system=\"s\"\n    user=\"u\"\n  end\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	if got := extractStepKindHints(mtx); got != nil {
		t.Errorf("no annotations: got %v, want nil", got)
	}
}

func TestBuildSystemPrompt_SurfacesStepKindHints(t *testing.T) {
	skill := &runtime.LoadedSkill{
		URI:      "matrix://skill/router-aware@1.0.0",
		MtxBytes: stepKindMtx(map[string]string{"build": "code", "analyze": "summarize"}),
	}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "")

	if !strings.Contains(prompt, "== Step routing hints") {
		t.Errorf("prompt missing step routing hints section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "verb=build -> step.kind=\"code\"") {
		t.Errorf("prompt missing build->code hint:\n%s", prompt)
	}
	if !strings.Contains(prompt, "verb=analyze -> step.kind=\"summarize\"") {
		t.Errorf("prompt missing analyze->summarize hint:\n%s", prompt)
	}
	// Verbs must appear in sorted order so the system prompt hashes
	// stably across runs (D11 + cache key stability).
	buildIdx := strings.Index(prompt, "verb=build")
	analyzeIdx := strings.Index(prompt, "verb=analyze")
	if analyzeIdx > buildIdx {
		t.Errorf("verbs not sorted (analyze should precede build): build@%d analyze@%d",
			buildIdx, analyzeIdx)
	}
	// And the schema description must advertise the step.kind field.
	if !strings.Contains(prompt, "\"kind\": \"<reason|code|summarize|write|transform|classify|hard_reason>\"") {
		t.Errorf("schema description missing step.kind enum:\n%s", prompt)
	}
}

func TestBuildSystemPrompt_OmitsHintsSectionWhenNone(t *testing.T) {
	// Skills without on-block kind annotations must not emit an empty
	// hint section (would inflate prompt + waste tokens for the 159
	// bulk-converted fixtures).
	skill := &runtime.LoadedSkill{URI: "matrix://skill/bulk@1.0.0"}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "")
	if strings.Contains(prompt, "== Step routing hints") {
		t.Errorf("prompt has hints section when skill has no .mtx bytes:\n%s", prompt)
	}
}

// ---------------------------------------------------------------------------
// Session 31c · Planner prompt parallelism nudge (P3b)
// ---------------------------------------------------------------------------

func TestBuildSystemPrompt_TeachesAggressiveParallel(t *testing.T) {
	// The planner is biased to emit Sequential by default which leaves
	// the walker's parallel goroutine fan-out (S23Q3) on the table for
	// independent work. Verify the system prompt explicitly nudges the
	// LLM toward parallel{} for fan-out cases.
	skill := &runtime.LoadedSkill{URI: "matrix://skill/x@1.0.0"}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "")

	// The constraint paragraph must contain BOTH the strong directive
	// ("PREFER 'parallel'") and the concrete examples that anchor it.
	for _, want := range []string{
		"PREFER \"parallel\"",
		"Brainstorming",
		"max(...)",
		"sum(...)",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("planner prompt missing parallel-nudge fragment %q", want)
		}
	}
}

// TestBuildSystemPrompt_ForbidsReasonOnlyPlans pins the sess#37 follow-up
// safety net: the planner system prompt must contain an explicit
// constraint forbidding plans that consist entirely of reason-kind
// step nodes when tools are available or the prose references concrete
// artifacts. This is the defense-in-depth layer for thin skills that
// declare §TOOLS=none — the planner falls back to the manifest tools
// rather than hallucinating about a path it never read.
func TestBuildSystemPrompt_ForbidsReasonOnlyPlans(t *testing.T) {
	skill := &runtime.LoadedSkill{URI: "matrix://skill/x@1.0.0"}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "")

	// Every load-bearing fragment of the new clause must appear so a
	// future prompt refactor that drops the policy fails CI loudly.
	for _, want := range []string{
		"REASON-ONLY PLANS ARE POLICY-FORBIDDEN",
		"filesystem path",
		"persistence verb",
		"contract violation",
		"D18", // ties the policy back to the compiler/executor split
		"§TOOLS=none",
		"forge-bridge.shell_exec",
		"/memory",
		"MATRIX_DAEMON",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("planner prompt missing reason-only-forbidden fragment %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Session 31c P3c · output_cardinality hint surfacing
// ---------------------------------------------------------------------------

// stepCardMtx builds a minimal SKILL.mtx with both on-block kind
// annotations and output_cardinality annotations interleaved.
func stepCardMtx(verbCards map[string]int, verbKinds map[string]string) []byte {
	var sb strings.Builder
	sb.WriteString("\u00a7SKILL\nid=test\nmcl.verbs=")
	verbs := make([]string, 0)
	seen := map[string]struct{}{}
	for v := range verbCards {
		if _, ok := seen[v]; !ok {
			verbs = append(verbs, v)
			seen[v] = struct{}{}
		}
	}
	for v := range verbKinds {
		if _, ok := seen[v]; !ok {
			verbs = append(verbs, v)
			seen[v] = struct{}{}
		}
	}
	for i, v := range verbs {
		if i > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(v)
	}
	sb.WriteString("\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\n")
	for _, v := range verbs {
		fmt.Fprintf(&sb, "on verb=%s\n", v)
		if k, ok := verbKinds[v]; ok {
			fmt.Fprintf(&sb, "  kind = %q\n", k)
		}
		if n, ok := verbCards[v]; ok {
			fmt.Fprintf(&sb, "  output_cardinality = %d\n", n)
		}
		sb.WriteString("end\n")
	}
	sb.WriteString("\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	return []byte(sb.String())
}

func TestExtractOutputCardinalityHints_Single(t *testing.T) {
	mtx := stepCardMtx(map[string]int{"build": 8}, nil)
	cards := extractOutputCardinalityHints(mtx)
	if cards["build"] != 8 {
		t.Errorf("cards[build] = %d, want 8 (full: %v)", cards["build"], cards)
	}
}

func TestExtractOutputCardinalityHints_Multiple(t *testing.T) {
	mtx := stepCardMtx(map[string]int{"build": 8, "review": 3, "deliver": 5}, nil)
	cards := extractOutputCardinalityHints(mtx)
	for v, want := range map[string]int{"build": 8, "review": 3, "deliver": 5} {
		if cards[v] != want {
			t.Errorf("cards[%s] = %d, want %d", v, cards[v], want)
		}
	}
}

func TestExtractOutputCardinalityHints_None(t *testing.T) {
	mtx := stepCardMtx(nil, map[string]string{"build": "code"})
	if got := extractOutputCardinalityHints(mtx); got != nil {
		t.Errorf("output_cardinality absent: got %v, want nil", got)
	}
}

func TestBuildSystemPrompt_SurfacesCardinalityHints(t *testing.T) {
	skill := &runtime.LoadedSkill{
		URI:      "matrix://skill/brainstorming@0.1.0",
		MtxBytes: stepCardMtx(map[string]int{"build": 8}, map[string]string{"build": "write"}),
	}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "")

	if !strings.Contains(prompt, "Output-cardinality hints") {
		t.Errorf("prompt missing output-cardinality section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "verb=build -> output_cardinality=8") {
		t.Errorf("prompt missing build->8 cardinality hint:\n%s", prompt)
	}
	// Both hint flavours surface together under the same header.
	if !strings.Contains(prompt, "verb=build -> step.kind=\"write\"") {
		t.Errorf("prompt missing kind hint when both hints present:\n%s", prompt)
	}
	if !strings.Contains(prompt, "do NOT emit N separate\nsequential NodeSteps") {
		t.Errorf("prompt missing the N-sequential-NodeSteps anti-pattern:\n%s", prompt)
	}
}

func TestBuildSystemPrompt_OmitsCardinalitySectionWhenNone(t *testing.T) {
	skill := &runtime.LoadedSkill{URI: "matrix://skill/no-card@1.0.0"}
	prompt := buildSystemPrompt(skill, fakeManifest(), nil, "")
	if strings.Contains(prompt, "Output-cardinality hints") {
		t.Errorf("prompt has cardinality section when skill has no .mtx bytes")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
