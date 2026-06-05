// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package parser

import (
	"os"
	"testing"

	"matrix/mcl/mtx/ast"
)

// ---- unit tests ----

func TestParseMinimalSection(t *testing.T) {
	src := []byte("§SKILL\nid=test\nversion=1.0.0\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	if len(file.Sections) != 1 {
		t.Fatalf("got %d sections, want 1", len(file.Sections))
	}

	sec := file.Sections[0]
	if sec.Name != "SKILL" {
		t.Errorf("section name: got %q, want %q", sec.Name, "SKILL")
	}
	if len(sec.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(sec.Entries))
	}

	kv := sec.Entries[0].(*ast.KVPair)
	if kv.Key[0] != "id" {
		t.Errorf("kv key: got %q, want %q", kv.Key[0], "id")
	}
	assertStringValue(t, kv.Value, "test")
}

func TestParseMultipleSections(t *testing.T) {
	src := []byte("§SKILL\nid=test\n\n§INPUTS\n\n§CORTEX\nreads=Fact Goal\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	if len(file.Sections) != 3 {
		t.Fatalf("got %d sections, want 3", len(file.Sections))
	}
	if file.Sections[0].Name != "SKILL" {
		t.Errorf("section[0] name: %q", file.Sections[0].Name)
	}
	if file.Sections[1].Name != "INPUTS" {
		t.Errorf("section[1] name: %q", file.Sections[1].Name)
	}
	if file.Sections[2].Name != "CORTEX" {
		t.Errorf("section[2] name: %q", file.Sections[2].Name)
	}
}

func TestParseSlotDecl(t *testing.T) {
	src := []byte("§INPUTS\nslot target: ArtifactRef\n  required\n  hint=\"The target\"\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	sec := file.Sections[0]
	if len(sec.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(sec.Entries))
	}

	sd := sec.Entries[0].(*ast.SlotDecl)
	if sd.Name != "target" {
		t.Errorf("slot name: got %q, want %q", sd.Name, "target")
	}
	if sd.TypeRef.Name != "ArtifactRef" {
		t.Errorf("slot type: got %q, want %q", sd.TypeRef.Name, "ArtifactRef")
	}
	if len(sd.Modifiers) != 2 {
		t.Fatalf("got %d modifiers, want 2", len(sd.Modifiers))
	}
	if sd.Modifiers[0].Kind != ast.ModRequired {
		t.Errorf("mod[0]: got %v, want ModRequired", sd.Modifiers[0].Kind)
	}
	if sd.Modifiers[1].Kind != ast.ModHint {
		t.Errorf("mod[1]: got %v, want ModHint", sd.Modifiers[1].Kind)
	}
}

func TestParseSlotEnumType(t *testing.T) {
	src := []byte("§INPUTS\nslot style: enum<formal|casual|technical>\n  optional\n  default=formal\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	sd := file.Sections[0].Entries[0].(*ast.SlotDecl)
	if sd.TypeRef.Name != "enum" {
		t.Errorf("type name: got %q, want %q", sd.TypeRef.Name, "enum")
	}
	if len(sd.TypeRef.EnumSet) != 3 {
		t.Fatalf("enum set: got %d, want 3", len(sd.TypeRef.EnumSet))
	}
	if sd.TypeRef.EnumSet[0] != "formal" || sd.TypeRef.EnumSet[1] != "casual" || sd.TypeRef.EnumSet[2] != "technical" {
		t.Errorf("enum set: %v", sd.TypeRef.EnumSet)
	}
}

func TestParseSlotListType(t *testing.T) {
	src := []byte("§INPUTS\nslot constraints: Constraint[]\n  optional\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	sd := file.Sections[0].Entries[0].(*ast.SlotDecl)
	if sd.TypeRef.Name != "Constraint" || !sd.TypeRef.IsList {
		t.Errorf("type: got %q list=%v, want Constraint list=true", sd.TypeRef.Name, sd.TypeRef.IsList)
	}
}

func TestParseOnBlock(t *testing.T) {
	src := []byte(`§PROCEDURE
on verb=build
  prompt
    system="sys"
    user="usr"
  end
  resolve slot.target <- cortex.find(type=Fact, limit=5)
end
`)
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	sec := file.Sections[0]
	if len(sec.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(sec.Entries))
	}

	ob := sec.Entries[0].(*ast.OnBlock)
	vc := ob.Condition.(*ast.VerbCondition)
	if vc.Verb != "build" {
		t.Errorf("verb: got %q, want %q", vc.Verb, "build")
	}

	if len(ob.Entries) < 2 {
		t.Fatalf("on-block entries: got %d, want >= 2", len(ob.Entries))
	}

	// First entry: prompt block
	pb := ob.Entries[0].(*ast.PromptBlock)
	if len(pb.Roles) != 2 {
		t.Fatalf("prompt roles: got %d, want 2", len(pb.Roles))
	}
	if pb.Roles[0].Role != "system" || pb.Roles[0].Text != "sys" {
		t.Errorf("role[0]: %q=%q", pb.Roles[0].Role, pb.Roles[0].Text)
	}

	// Second entry: resolve
	rs := ob.Entries[1].(*ast.ResolveStmt)
	if rs.SlotName != "target" {
		t.Errorf("resolve slot: got %q", rs.SlotName)
	}
	if rs.CortexFn != "cortex.find" {
		t.Errorf("cortex fn: got %q", rs.CortexFn)
	}
	if len(rs.Args) != 2 {
		t.Fatalf("args: got %d, want 2", len(rs.Args))
	}
}

func TestParseConfidenceCondition(t *testing.T) {
	src := []byte("§PROCEDURE\non confidence<0.75\nend\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	ob := file.Sections[0].Entries[0].(*ast.OnBlock)
	cc := ob.Condition.(*ast.ConfidenceCondition)
	if cc.Op != "<" || cc.Threshold != "0.75" {
		t.Errorf("confidence condition: got %q %q, want < 0.75", cc.Op, cc.Threshold)
	}
}

func TestParseUnknownBlock(t *testing.T) {
	src := []byte(`§PROCEDURE
on verb=build
  unknown slot.target
    severity=blocking
    reason="Cannot identify target"
  end
end
`)
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	ob := file.Sections[0].Entries[0].(*ast.OnBlock)
	ub := ob.Entries[0].(*ast.UnknownBlock)
	if ub.SlotName != "target" {
		t.Errorf("unknown slot: got %q", ub.SlotName)
	}
	if len(ub.Modifiers) != 2 {
		t.Fatalf("unknown modifiers: got %d, want 2", len(ub.Modifiers))
	}
	if ub.Modifiers[0].Key != "severity" {
		t.Errorf("mod[0] key: got %q", ub.Modifiers[0].Key)
	}
}

func TestParseClarifyBlock(t *testing.T) {
	src := []byte(`§PROCEDURE
on confidence<0.75
  clarify slot.target
    prompt="Which one?"
    type=ArtifactRef
    required=true
  end
end
`)
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	ob := file.Sections[0].Entries[0].(*ast.OnBlock)
	cb := ob.Entries[0].(*ast.ClarifyBlock)
	if cb.SlotName != "target" {
		t.Errorf("clarify slot: got %q", cb.SlotName)
	}
	if len(cb.Modifiers) != 3 {
		t.Fatalf("clarify modifiers: got %d, want 3", len(cb.Modifiers))
	}
}

func TestParseFailureModes(t *testing.T) {
	src := []byte(`§FAILURE_MODES
budget_exceeded
  suggest=raise_budget

policy_violation
  action=fail
  reason=policy_violation
`)
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	sec := file.Sections[0]
	if len(sec.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(sec.Entries))
	}

	fe := sec.Entries[0].(*ast.FailEntry)
	if fe.Name != "budget_exceeded" {
		t.Errorf("fail name: got %q", fe.Name)
	}
	if len(fe.Modifiers) != 1 {
		t.Fatalf("fail modifiers: got %d, want 1", len(fe.Modifiers))
	}
	if fe.Modifiers[0].Key != "suggest" || fe.Modifiers[0].Value != "raise_budget" {
		t.Errorf("fail mod: %q=%q", fe.Modifiers[0].Key, fe.Modifiers[0].Value)
	}

	fe2 := sec.Entries[1].(*ast.FailEntry)
	if fe2.Name != "policy_violation" {
		t.Errorf("fail name: got %q", fe2.Name)
	}
	if len(fe2.Modifiers) != 2 {
		t.Fatalf("fail modifiers: got %d, want 2", len(fe2.Modifiers))
	}
}

func TestParseToolsNone(t *testing.T) {
	src := []byte("§TOOLS\nnone\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	sec := file.Sections[0]
	if len(sec.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(sec.Entries))
	}
	if _, ok := sec.Entries[0].(*ast.NoneEntry); !ok {
		t.Errorf("expected NoneEntry, got %T", sec.Entries[0])
	}
}

func TestParseToolsURIs(t *testing.T) {
	src := []byte("§TOOLS\nmatrix://tool/registry/query@2.0\nmatrix://tool/payments/stream@1.0\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	sec := file.Sections[0]
	if len(sec.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(sec.Entries))
	}
	re := sec.Entries[0].(*ast.RefEntry)
	if re.URI != "matrix://tool/registry/query@2.0" {
		t.Errorf("URI: got %q", re.URI)
	}
}

func TestParseDottedKey(t *testing.T) {
	src := []byte("§PIPELINE\nstage.1.id=normalise\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	kv := file.Sections[0].Entries[0].(*ast.KVPair)
	if len(kv.Key) != 3 || kv.Key[0] != "stage" || kv.Key[1] != "1" || kv.Key[2] != "id" {
		t.Errorf("dotted key: got %v", kv.Key)
	}
}

func TestParseSpaceList(t *testing.T) {
	src := []byte("§CORTEX\nreads=Preference Goal Constraint Event\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	kv := file.Sections[0].Entries[0].(*ast.KVPair)
	sl, ok := kv.Value.(*ast.SpaceListValue)
	if !ok {
		t.Fatalf("expected SpaceListValue, got %T", kv.Value)
	}
	if len(sl.Items) != 4 {
		t.Fatalf("space list: got %d items, want 4", len(sl.Items))
	}
	if sl.Items[0] != "Preference" || sl.Items[3] != "Event" {
		t.Errorf("space list items: %v", sl.Items)
	}
}

func TestParseNestedOnBlocks(t *testing.T) {
	src := []byte(`§PROCEDURE
on verb=build
  on confidence<0.75
  end
end
`)
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	ob := file.Sections[0].Entries[0].(*ast.OnBlock)
	if len(ob.Entries) != 1 {
		t.Fatalf("outer on entries: got %d, want 1", len(ob.Entries))
	}
	inner := ob.Entries[0].(*ast.OnBlock)
	if _, ok := inner.Condition.(*ast.ConfidenceCondition); !ok {
		t.Errorf("expected ConfidenceCondition, got %T", inner.Condition)
	}
}

func TestParseResolveWithSlotExpr(t *testing.T) {
	src := []byte("§PROCEDURE\non verb=find\n  resolve slot.target <- cortex.find(type=Fact, near=slot.target.prose, limit=5)\nend\n")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	ob := file.Sections[0].Entries[0].(*ast.OnBlock)
	rs := ob.Entries[0].(*ast.ResolveStmt)
	if len(rs.Args) != 3 {
		t.Fatalf("resolve args: got %d, want 3", len(rs.Args))
	}
	// Second arg should be a slot expr
	nearArg := rs.Args[1]
	if nearArg.Name != "near" {
		t.Errorf("arg name: got %q, want %q", nearArg.Name, "near")
	}
	se, ok := nearArg.Value.(*ast.SlotExprValue)
	if !ok {
		t.Fatalf("expected SlotExprValue, got %T", nearArg.Value)
	}
	if len(se.Parts) != 3 || se.Parts[0] != "slot" || se.Parts[1] != "target" || se.Parts[2] != "prose" {
		t.Errorf("slot expr parts: %v", se.Parts)
	}
}

// ---- integration tests against real .mtx files ----

const (
	testCoreDir  = "/root/matrix/MCL/core"
	testSkillDir = "/root/matrix/skills/writing-plans"
)

func TestParseVerbMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/verb.mtx")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	assertSectionExists(t, file, "VERB")
}

func TestParseFrameMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/frame.mtx")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	assertSectionExists(t, file, "FRAME")
	// Should have slot declarations
	sec := findSection(file, "FRAME")
	if sec == nil {
		t.Fatal("missing FRAME section")
	}
	slotCount := 0
	for _, e := range sec.Entries {
		if _, ok := e.(*ast.SlotDecl); ok {
			slotCount++
		}
	}
	if slotCount != 5 {
		t.Errorf("FRAME slots: got %d, want 5 (verb, objects, constraints, success_criteria, preferences)", slotCount)
	}
}

func TestParsePipelineMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/pipeline.mtx")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	assertSectionExists(t, file, "PIPELINE")
	sec := findSection(file, "PIPELINE")
	if sec == nil {
		t.Fatal("missing PIPELINE section")
	}
	// Should have many kv pairs for stage.N.X
	kvCount := 0
	for _, e := range sec.Entries {
		if _, ok := e.(*ast.KVPair); ok {
			kvCount++
		}
	}
	if kvCount < 20 {
		t.Errorf("PIPELINE kv pairs: got %d, want >= 20", kvCount)
	}
}

func TestParseConfidenceMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/confidence.mtx")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	assertSectionExists(t, file, "CONFIDENCE")
}

func TestParseWritingPlansSKILLMtx(t *testing.T) {
	src := readTestFile(t, testSkillDir+"/SKILL.mtx")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	// Must have all 9 required sections
	for _, name := range []string{"SKILL", "INPUTS", "CORTEX", "TOOLS", "SUB_SKILLS", "PROCEDURE", "OUTPUTS", "FAILURE_MODES", "HASH"} {
		assertSectionExists(t, file, name)
	}

	// PROCEDURE should have on-blocks
	proc := findSection(file, "PROCEDURE")
	if proc == nil {
		t.Fatal("missing PROCEDURE section")
	}
	onCount := 0
	for _, e := range proc.Entries {
		if _, ok := e.(*ast.OnBlock); ok {
			onCount++
		}
	}
	if onCount < 3 {
		t.Errorf("PROCEDURE on-blocks: got %d, want >= 3 (verb=build, verb=modify, confidence<0.75)", onCount)
	}

	// INPUTS should have slot declarations
	inputs := findSection(file, "INPUTS")
	if inputs == nil {
		t.Fatal("missing INPUTS section")
	}
	slotCount := 0
	for _, e := range inputs.Entries {
		if _, ok := e.(*ast.SlotDecl); ok {
			slotCount++
		}
	}
	if slotCount < 2 {
		t.Errorf("INPUTS slots: got %d, want >= 2", slotCount)
	}

	// TOOLS should have NoneEntry
	tools := findSection(file, "TOOLS")
	if tools == nil {
		t.Fatal("missing TOOLS section")
	}
	hasNone := false
	for _, e := range tools.Entries {
		if _, ok := e.(*ast.NoneEntry); ok {
			hasNone = true
		}
	}
	if !hasNone {
		t.Error("TOOLS should have 'none' entry")
	}

	// SUB_SKILLS should have RefEntry
	subs := findSection(file, "SUB_SKILLS")
	if subs == nil {
		t.Fatal("missing SUB_SKILLS section")
	}
	hasRef := false
	for _, e := range subs.Entries {
		if _, ok := e.(*ast.RefEntry); ok {
			hasRef = true
		}
	}
	if !hasRef {
		t.Error("SUB_SKILLS should have URI reference entry")
	}

	// FAILURE_MODES should have FailEntry
	fm := findSection(file, "FAILURE_MODES")
	if fm == nil {
		t.Fatal("missing FAILURE_MODES section")
	}
	failCount := 0
	for _, e := range fm.Entries {
		if _, ok := e.(*ast.FailEntry); ok {
			failCount++
		}
	}
	if failCount < 2 {
		t.Errorf("FAILURE_MODES fail entries: got %d, want >= 2", failCount)
	}
}

func TestParseWritingPlansPromptContent(t *testing.T) {
	src := readTestFile(t, testSkillDir+"/SKILL.mtx")
	file, errs := New(src).Parse()
	assertNoErrors(t, errs)

	proc := findSection(file, "PROCEDURE")
	if proc == nil {
		t.Fatal("missing PROCEDURE section")
	}

	// Find the first on-block (verb=build)
	var firstOn *ast.OnBlock
	for _, e := range proc.Entries {
		if ob, ok := e.(*ast.OnBlock); ok {
			firstOn = ob
			break
		}
	}
	if firstOn == nil {
		t.Fatal("no on-block in PROCEDURE")
	}

	// First entry should be a prompt block
	var pb *ast.PromptBlock
	for _, e := range firstOn.Entries {
		if p, ok := e.(*ast.PromptBlock); ok {
			pb = p
			break
		}
	}
	if pb == nil {
		t.Fatal("no prompt block in first on-block")
	}
	if len(pb.Roles) < 2 {
		t.Errorf("prompt roles: got %d, want >= 2", len(pb.Roles))
	}
	if pb.Roles[0].Role != "system" {
		t.Errorf("first role: got %q, want %q", pb.Roles[0].Role, "system")
	}
}

// ---- helpers ----

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("test file not found: %s", path)
	}
	return data
}

func assertNoErrors(t *testing.T, errs []*Error) {
	t.Helper()
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("parse error: %s", e)
		}
		t.Fatalf("parser reported %d errors", len(errs))
	}
}

func assertSectionExists(t *testing.T, file *ast.File, name string) {
	t.Helper()
	for _, sec := range file.Sections {
		if sec.Name == name {
			return
		}
	}
	t.Errorf("missing section %q", name)
}

func findSection(file *ast.File, name string) *ast.Section {
	for _, sec := range file.Sections {
		if sec.Name == name {
			return sec
		}
	}
	return nil
}

func assertStringValue(t *testing.T, v ast.Value, want string) {
	t.Helper()
	switch val := v.(type) {
	case *ast.StringValue:
		if val.Text != want {
			t.Errorf("string value: got %q, want %q", val.Text, want)
		}
	case *ast.IdentValue:
		if val.Name != want {
			t.Errorf("ident value: got %q, want %q", val.Name, want)
		}
	default:
		t.Errorf("expected string or ident value %q, got %T", want, v)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
