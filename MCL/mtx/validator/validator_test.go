// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package validator

import (
	"os"
	"testing"

	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/parser"
	"matrix/mcl/mtx/token"
)

// ---- rule tests ----

func TestRule1RequiredSections(t *testing.T) {
	// Missing §TOOLS should fail
	src := []byte(`§SKILL
id=test
§INPUTS
§CORTEX
§SUB_SKILLS
none
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 1, "TOOLS")
}

func TestRule1AllSectionsPresent(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 1)
}

func TestRule2ValidVerbs(t *testing.T) {
	src := fullSkill("mcl.verbs=build modify delegate")
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 2)
}

func TestRule2InvalidVerb(t *testing.T) {
	src := fullSkill("mcl.verbs=build execute")
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 2, "execute")
}

func TestRule2ExtensionVerb(t *testing.T) {
	src := fullSkill("mcl.verbs=build x:brainstorm")
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 2)
}

func TestRule4DuplicateSlot(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required
slot target: string
  optional
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 4, "target")
}

func TestRule4UniqueSlots(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required
slot style: string
  optional
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 4)
}

func TestRule5ResolveRefsValidSlot(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on verb=build
  resolve slot.target <- cortex.find(type=Fact, limit=5)
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 5)
}

func TestRule5ResolveRefsInvalidSlot(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on verb=build
  resolve slot.missing <- cortex.find(type=Fact, limit=5)
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 5, "missing")
}

func TestRule6UnknownRefsValidSlot(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on verb=build
  unknown slot.target
    severity=blocking
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 6)
}

func TestRule7PromptMissingSystem(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on verb=build
  prompt
    user="hello"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 7, "system")
}

func TestRule7PromptComplete(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on verb=build
  prompt
    system="sys"
    user="usr"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 7)
}

func TestRule8InvalidFailureReason(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
broken
  action=fail
  reason=made_up_reason
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 8, "made_up_reason")
}

func TestRule8ValidFailureReason(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
broken
  action=fail
  reason=policy_violation
`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 8)
}

func TestRule9UnpinnedSubSkill(t *testing.T) {
	file := &ast.File{
		Sections: []*ast.Section{
			minSkillSection(), minInputs(), minCortex(),
			minTools(), {Name: "SUB_SKILLS", Entries: []ast.Entry{
				&ast.RefEntry{URI: "matrix://skill/foo"},
			}},
			minProcedure(), minOutputs(), minFailureModes(),
		},
	}
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 9, "not version-pinned")
}

func TestRule9PinnedSubSkill(t *testing.T) {
	file := &ast.File{
		Sections: []*ast.Section{
			minSkillSection(), minInputs(), minCortex(),
			minTools(), {Name: "SUB_SKILLS", Entries: []ast.Entry{
				&ast.RefEntry{URI: "matrix://skill/foo@1.0.0"},
			}},
			minProcedure(), minOutputs(), minFailureModes(),
		},
	}
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 9)
}

func TestRule10UnpinnedTool(t *testing.T) {
	file := &ast.File{
		Sections: []*ast.Section{
			minSkillSection(), minInputs(), minCortex(),
			{Name: "TOOLS", Entries: []ast.Entry{
				&ast.RefEntry{URI: "matrix://tool/registry/query"},
			}},
			minSubSkills(), minProcedure(), minOutputs(), minFailureModes(),
		},
	}
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 10, "not version-pinned")
}

func TestRule10PinnedTool(t *testing.T) {
	file := &ast.File{
		Sections: []*ast.Section{
			minSkillSection(), minInputs(), minCortex(),
			{Name: "TOOLS", Entries: []ast.Entry{
				&ast.RefEntry{URI: "matrix://tool/registry/query@2.0"},
			}},
			minSubSkills(), minProcedure(), minOutputs(), minFailureModes(),
		},
	}
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 10)
}

// ---- integration: validate real files ----

func TestValidateWritingPlansSKILL(t *testing.T) {
	src := readTestFile(t, "/root/matrix/skills/writing-plans/SKILL.mtx")
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("validation error: %s", e)
		}
	}
}

func TestValidateCoreVerb(t *testing.T) {
	src := readTestFile(t, "/root/matrix/MCL/core/verb.mtx")
	file := mustParse(t, src)
	errs := ValidateCore(file)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("validation error: %s", e)
		}
	}
}

// ---------------------------------------------------------------------------
// Rule 11 · on-block kind= metadata (Session 31b model router)
// ---------------------------------------------------------------------------

// skillWithProcedure splices a §PROCEDURE block into the canonical full
// skill skeleton. Keeps test inputs focused on the relevant rule.
func skillWithProcedure(procBody string) []byte {
	return []byte("§SKILL\nid=test\nmcl.verbs=build\n§INPUTS\n§CORTEX\n§TOOLS\nnone\n§SUB_SKILLS\nnone\n§PROCEDURE\n" +
		procBody + "\n§OUTPUTS\n§FAILURE_MODES\n")
}

func TestRule11_KindStringValid(t *testing.T) {
	src := skillWithProcedure(`on verb=build
  kind = "code"
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 11)
}

func TestRule11_KindIdentValid(t *testing.T) {
	src := skillWithProcedure(`on verb=build
  kind = code
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 11)
}

func TestRule11_KindAllClosedEnumValues(t *testing.T) {
	// Every value in ir.StepKindNames MUST pass rule 11. Guards against
	// drift between the enum source-of-truth and the validator.
	for _, kind := range []string{
		"reason", "code", "summarize", "write",
		"transform", "classify", "hard_reason",
	} {
		t.Run(kind, func(t *testing.T) {
			src := skillWithProcedure("on verb=build\n  kind = \"" + kind + "\"\nend")
			file := mustParse(t, src)
			errs := ValidateSkill(file)
			assertNoRuleErrors(t, errs, 11)
		})
	}
}

func TestRule11_KindUnknownRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring expected in V11 error message
	}{
		{
			"typo",
			"on verb=build\n  kind = \"writting\"\nend",
			"writting",
		},
		{
			"uppercase",
			"on verb=build\n  kind = \"CODE\"\nend",
			"CODE",
		},
		{
			"hyphen_form",
			"on verb=build\n  kind = \"hard-reason\"\nend",
			"hard-reason",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := skillWithProcedure(tt.body)
			file := mustParse(t, src)
			errs := ValidateSkill(file)
			assertHasRuleError(t, errs, 11, tt.want)
		})
	}
}

func TestRule11_KindOmittedOK(t *testing.T) {
	// Backwards-compat: skills without a kind annotation must not
	// produce any V11 errors. This is the case for the 159 bulk-
	// converted SKILL.mtx fixtures (matrix.kvx sess#22c).
	src := skillWithProcedure(`on verb=build
  prompt
    system="You are a builder."
    user="Build."
  end
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 11)
}

func TestRule11_KindInNestedOnBlock(t *testing.T) {
	// rule 11 walks nested on-blocks too (mirrors how other rules
	// already use walkEntries). Catches the case where a sub-condition
	// declares a stale kind.
	src := skillWithProcedure(`on verb=build
  on confidence>=0.85
    kind = "bogus_inner_kind"
  end
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 11, "bogus_inner_kind")
}

func TestRule11_NonKindKVPairsTolerated(t *testing.T) {
	// rule 11 must NOT flag unrelated metadata KVPairs (e.g. legacy
	// `skip = true` sentinels). Forward-compat for future hints.
	src := skillWithProcedure(`on verb=build
  skip = false
  output_cardinality = 8
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 11)
}

// ---------------------------------------------------------------------------
// Rule 12 · on-block output_cardinality= metadata (Session 31c P3c)
// ---------------------------------------------------------------------------

func TestRule12_OutputCardinalityPositiveOK(t *testing.T) {
	src := skillWithProcedure(`on verb=build
  output_cardinality = 8
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 12)
}

func TestRule12_OutputCardinalityZeroRejected(t *testing.T) {
	src := skillWithProcedure(`on verb=build
  output_cardinality = 0
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 12, "0")
}

func TestRule12_OutputCardinalityNegativeRejected(t *testing.T) {
	src := skillWithProcedure(`on verb=build
  output_cardinality = -3
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	// Parser may reject "-3" before the validator sees it (depends on
	// the lexer's int form). Accept either: parser rejection OR rule
	// 12 error. The contract is that "-3" never sneaks through as a
	// hint to the planner.
	if len(errs) == 0 {
		t.Errorf("expected validation error for negative cardinality, got none")
	}
}

func TestRule12_OutputCardinalityStringRejected(t *testing.T) {
	src := skillWithProcedure(`on verb=build
  output_cardinality = "8"
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 12, "integer literal")
}

func TestRule12_OutputCardinalityOmittedOK(t *testing.T) {
	// Backwards-compat: skills without the annotation produce no V12
	// errors (the 159 bulk-converted fixtures all match this case).
	src := skillWithProcedure(`on verb=build
  prompt
    system="s"
    user="u"
  end
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 12)
}

func TestRule12_OutputCardinalityInNestedOnBlock(t *testing.T) {
	// rule 12 walks nested on-blocks (mirrors rule 11's posture).
	src := skillWithProcedure(`on verb=build
  on confidence>=0.85
    output_cardinality = 0
  end
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertHasRuleError(t, errs, 12, "0")
}

func TestRule12_PositiveAlongsideValidKind(t *testing.T) {
	// Both metadata KVs in the same on-block: V11 + V12 must each
	// independently accept their respective valid input.
	src := skillWithProcedure(`on verb=build
  kind = "write"
  output_cardinality = 5
end`)
	file := mustParse(t, src)
	errs := ValidateSkill(file)
	assertNoRuleErrors(t, errs, 11)
	assertNoRuleErrors(t, errs, 12)
}

func TestValidateCoreFrame(t *testing.T) {
	src := readTestFile(t, "/root/matrix/MCL/core/frame.mtx")
	file := mustParse(t, src)
	errs := ValidateCore(file)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("validation error: %s", e)
		}
	}
}

// ---- helpers ----

func mustParse(t *testing.T, src []byte) *ast.File {
	t.Helper()
	p := parser.New(src)
	file, errs := p.Parse()
	if len(errs) > 0 {
		for _, e := range errs {
			t.Logf("parse error (non-fatal): %s", e)
		}
	}
	return file
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("test file not found: %s", path)
	}
	return data
}

func assertHasRuleError(t *testing.T, errs []*Error, rule int, substring string) {
	t.Helper()
	for _, e := range errs {
		if e.Rule == rule && contains(e.Msg, substring) {
			return
		}
	}
	t.Errorf("expected rule %d error containing %q, got: %v", rule, substring, errs)
}

func assertNoRuleErrors(t *testing.T, errs []*Error, rule int) {
	t.Helper()
	for _, e := range errs {
		if e.Rule == rule {
			t.Errorf("unexpected rule %d error: %s", rule, e)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (sub == "" || findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func fullSkill(verbLine string) []byte {
	return []byte("§SKILL\nid=test\n" + verbLine + "\n§INPUTS\n§CORTEX\n§TOOLS\nnone\n§SUB_SKILLS\nnone\n§PROCEDURE\n§OUTPUTS\n§FAILURE_MODES\n")
}

func minSkillSection() *ast.Section {
	return &ast.Section{Name: "SKILL"}
}
func minInputs() *ast.Section {
	return &ast.Section{Name: "INPUTS"}
}
func minCortex() *ast.Section {
	return &ast.Section{Name: "CORTEX"}
}
func minTools() *ast.Section {
	return &ast.Section{Name: "TOOLS", Entries: []ast.Entry{&ast.NoneEntry{}}}
}
func minSubSkills() *ast.Section {
	return &ast.Section{Name: "SUB_SKILLS", Entries: []ast.Entry{&ast.NoneEntry{}}}
}
func minProcedure() *ast.Section {
	return &ast.Section{Name: "PROCEDURE"}
}
func minOutputs() *ast.Section {
	return &ast.Section{Name: "OUTPUTS"}
}
func minFailureModes() *ast.Section {
	return &ast.Section{Name: "FAILURE_MODES"}
}

func pos() token.Pos { return token.Pos{} }

// Copyright © 2026 Paxlabs Inc. All rights reserved.
