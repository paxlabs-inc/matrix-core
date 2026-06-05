// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// minimalSkillMtx returns a parseable, validator-clean SKILL.mtx body
// with caller-supplied §TOOLS and §SUB_SKILLS sections. Keeps the
// rest of the required sections fixed so tests can vary just the
// allow-list axis.
func minimalSkillMtx(slug, version, toolsBody, subsBody string) string {
	return "" +
		"§SKILL\n" +
		"id=" + slug + "\n" +
		"version=" + version + "\n" +
		"display=\"Test Skill\"\n" +
		"description=\"unit-test fixture\"\n" +
		"mcl.verbs=build\n" +
		"\n§INPUTS\n" +
		"slot target: ArtifactRef\n  required\n" +
		"\n§CORTEX\n" +
		"reads=Fact\n" +
		"\n§TOOLS\n" + toolsBody + "\n" +
		"\n§SUB_SKILLS\n" + subsBody + "\n" +
		"\n§PROCEDURE\n" +
		"on verb=build\n" +
		"  prompt\n    system=\"x\"\n    user=\"y\"\n  end\nend\n" +
		"\n§OUTPUTS\n" +
		"slot result: ArtifactRef\n  required\n" +
		"\n§FAILURE_MODES\n" +
		"x\n  action=fail\n  reason=policy_violation\n"
}

func writeSkillFixture(t *testing.T, root, slug, version, toolsBody, subsBody string) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	mtx := []byte(minimalSkillMtx(slug, version, toolsBody, subsBody))
	if err := os.WriteFile(filepath.Join(dir, "SKILL.mtx"), mtx, 0o644); err != nil {
		t.Fatalf("write SKILL.mtx: %v", err)
	}
}

func TestSkillLoader_ToolsNone(t *testing.T) {
	root := t.TempDir()
	writeSkillFixture(t, root, "no-tools", "1.0.0", "none", "none")

	l := NewSkillLoader(root)
	sk, err := l.Load("matrix://skill/no-tools@1.0.0")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !sk.ToolsNone {
		t.Errorf("ToolsNone = false, want true (§TOOLS = none)")
	}
	if !sk.SubSkillsNone {
		t.Errorf("SubSkillsNone = false, want true (§SUB_SKILLS = none)")
	}
	if len(sk.DeclaredTools) != 0 {
		t.Errorf("DeclaredTools = %v, want empty", sk.DeclaredTools)
	}
	if len(sk.DeclaredSubSkills) != 0 {
		t.Errorf("DeclaredSubSkills = %v, want empty", sk.DeclaredSubSkills)
	}
}

func TestSkillLoader_ToolsAllowList(t *testing.T) {
	root := t.TempDir()
	tools := "matrix://tool/mcp/fs/read_text_file@2024.11.1\n" +
		"matrix://tool/mcp/git/git_status@2024.11.1"
	subs := "matrix://skill/writing-plans@1.0.0"
	writeSkillFixture(t, root, "with-tools", "2.0.0", tools, subs)

	l := NewSkillLoader(root)
	sk, err := l.Load("matrix://skill/with-tools@2.0.0")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sk.ToolsNone {
		t.Errorf("ToolsNone = true, want false")
	}
	want := []string{
		"matrix://tool/mcp/fs/read_text_file@2024.11.1",
		"matrix://tool/mcp/git/git_status@2024.11.1",
	}
	if len(sk.DeclaredTools) != len(want) {
		t.Fatalf("DeclaredTools = %v, want %v", sk.DeclaredTools, want)
	}
	for i, u := range want {
		if sk.DeclaredTools[i] != u {
			t.Errorf("DeclaredTools[%d] = %q, want %q", i, sk.DeclaredTools[i], u)
		}
	}
	if len(sk.DeclaredSubSkills) != 1 || sk.DeclaredSubSkills[0] != "matrix://skill/writing-plans@1.0.0" {
		t.Errorf("DeclaredSubSkills = %v, want [writing-plans@1.0.0]", sk.DeclaredSubSkills)
	}
}

func TestSkillLoader_BrainstormingFixtureMatchesProductionShape(t *testing.T) {
	// Sanity check against the real corpus skill that triggered the
	// synthesizer fix. brainstorming declares §TOOLS none + §SUB_SKILLS
	// none; without the new fields it would have fallen through to
	// "all manifest tools allowed" and the LLM would have tried to
	// call git/fs tools (the bug we're fixing).
	l := NewSkillLoader(DefaultSkillRepoRoot)
	sk, err := l.Load("matrix://skill/brainstorming@0.1.0")
	if err != nil {
		t.Skipf("brainstorming skill not present in repo: %v", err)
	}
	if !sk.ToolsNone {
		t.Errorf("brainstorming ToolsNone = false, want true")
	}
	if !sk.SubSkillsNone {
		t.Errorf("brainstorming SubSkillsNone = false, want true")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
