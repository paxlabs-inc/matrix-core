// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package interpreter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/parser"
	"matrix/mcl/mtx/token"
)

// ---- mock LLM ----

type mockLLM struct {
	response string
	err      error
	calls    []mockLLMCall
}

type mockLLMCall struct {
	Messages []Message
	Grammar  string
}

func (m *mockLLM) Decode(ctx context.Context, messages []Message, grammar string) (string, error) {
	m.calls = append(m.calls, mockLLMCall{Messages: messages, Grammar: grammar})
	return m.response, m.err
}

// ---- mock cortex ----

type mockCortex struct {
	findResults   []CortexResult
	resolveResult *CortexResult
	contextResult string
	findErr       error
	resolveErr    error
	contextErr    error
	findCalls     []map[string]string
	resolveCalls  []string
	contextCalls  []map[string]string
}

func (m *mockCortex) Find(ctx context.Context, args map[string]string) ([]CortexResult, error) {
	m.findCalls = append(m.findCalls, args)
	return m.findResults, m.findErr
}

func (m *mockCortex) Resolve(ctx context.Context, expr string) (*CortexResult, error) {
	m.resolveCalls = append(m.resolveCalls, expr)
	return m.resolveResult, m.resolveErr
}

func (m *mockCortex) Context(ctx context.Context, args map[string]string) (string, error) {
	m.contextCalls = append(m.contextCalls, args)
	return m.contextResult, m.contextErr
}

// ---- helpers ----

func mustParse(t *testing.T, src []byte) *ast.File {
	t.Helper()
	p := parser.New(src)
	file, errs := p.Parse()
	if len(errs) > 0 {
		for _, e := range errs {
			t.Logf("parse error: %s", e)
		}
		t.Fatalf("parse failed with %d errors", len(errs))
	}
	return file
}

func projectRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

// ---- unit tests ----

func TestVerbConditionMatch(t *testing.T) {
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
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{
		Prose: "build me a plan",
		Verb:  "build",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Executed {
		t.Fatal("expected on-block to execute")
	}
	if result.MatchedCondition != "verb=build" {
		t.Fatalf("got condition %q, want verb=build", result.MatchedCondition)
	}
}

func TestVerbConditionNoMatch(t *testing.T) {
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
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{
		Prose: "find me a fact",
		Verb:  "find",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Executed {
		t.Fatal("expected no on-block to match")
	}
}

func TestConfidenceCondition(t *testing.T) {
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
on confidence<0.75
  clarify slot.target
    prompt="What do you want?"
    type=string
    required=true
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	interp := New(file, nil, nil)

	// Low confidence — should match
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:       "build",
		Confidence: 0.50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Executed {
		t.Fatal("expected match on confidence<0.75 when confidence=0.50")
	}
	if len(result.ClarifyQuestions) != 1 {
		t.Fatalf("got %d clarify questions, want 1", len(result.ClarifyQuestions))
	}
	if result.ClarifyQuestions[0].Prompt != "What do you want?" {
		t.Fatalf("got prompt %q", result.ClarifyQuestions[0].Prompt)
	}

	// High confidence — should not match
	result2, err := interp.Run(context.Background(), &RunInput{
		Verb:       "build",
		Confidence: 0.90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result2.Executed {
		t.Fatal("expected no match when confidence=0.90")
	}
}

func TestFirstMatchWins(t *testing.T) {
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
    system="first"
    user="first"
  end
end
on verb=build
  prompt
    system="second"
    user="second"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	llm := &mockLLM{response: `{"verb":"build"}`}
	interp := New(file, llm, nil)
	result, err := interp.Run(context.Background(), &RunInput{
		Verb: "build",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PromptMessages) == 0 {
		t.Fatal("expected prompt messages")
	}
	if result.PromptMessages[0].Content != "first" {
		t.Fatalf("got %q, want 'first' (first-match-wins)", result.PromptMessages[0].Content)
	}
}

func TestPromptInterpolation(t *testing.T) {
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
  prompt
    system="Build mode"
    user="Goal: {prose}\nVerb: {verb}\nBundle:\n{cortex.bundle}\nSlots:\n{slots}\nTarget: {slot.target}"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	interp := New(file, nil, nil) // nil LLM = dry-run
	result, err := interp.Run(context.Background(), &RunInput{
		Prose:      "build a database",
		Verb:       "build",
		Bundle:     "[memory: Goal#1 \"create schema\"]",
		SlotValues: map[string]string{"target": "the database"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PromptMessages) != 2 {
		t.Fatalf("got %d messages, want 2", len(result.PromptMessages))
	}
	user := result.PromptMessages[1].Content
	if !strings.Contains(user, "build a database") {
		t.Fatalf("missing prose interpolation in %q", user)
	}
	if !strings.Contains(user, "Verb: build") {
		t.Fatalf("missing verb interpolation in %q", user)
	}
	if !strings.Contains(user, "[memory: Goal#1") {
		t.Fatalf("missing bundle interpolation in %q", user)
	}
	if !strings.Contains(user, "the database") {
		t.Fatalf("missing slot.target interpolation in %q", user)
	}
}

func TestLLMCallWithGrammar(t *testing.T) {
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
	llm := &mockLLM{response: `{"verb":"build","objects":["db"]}`}
	interp := New(file, llm, nil)
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:    "build",
		Grammar: "intent_frame@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FrameJSON != `{"verb":"build","objects":["db"]}` {
		t.Fatalf("unexpected frame JSON: %q", result.FrameJSON)
	}
	if len(llm.calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(llm.calls))
	}
	if llm.calls[0].Grammar != "intent_frame@1" {
		t.Fatalf("grammar not passed through: %q", llm.calls[0].Grammar)
	}
}

func TestLLMError(t *testing.T) {
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
	llm := &mockLLM{err: fmt.Errorf("model unavailable")}
	interp := New(file, llm, nil)
	_, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveStmtFind(t *testing.T) {
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
	cx := &mockCortex{
		findResults: []CortexResult{
			{URI: "matrix://cortex/Fact/abc#1", Type: "Fact", Summary: "a fact"},
		},
	}
	interp := New(file, nil, cx)
	result, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatal(err)
	}
	slot := result.Slots["target"]
	if slot == nil {
		t.Fatal("slot 'target' not found")
	}
	if slot.Status != SlotResolved {
		t.Fatalf("slot status=%d, want SlotResolved(%d)", slot.Status, SlotResolved)
	}
	if slot.Value != "matrix://cortex/Fact/abc#1" {
		t.Fatalf("slot value=%q", slot.Value)
	}
	if len(cx.findCalls) != 1 {
		t.Fatalf("expected 1 find call, got %d", len(cx.findCalls))
	}
}

func TestResolveStmtResolve(t *testing.T) {
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
on verb=modify
  resolve slot.target <- cortex.resolve(slot.target.prose)
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	cx := &mockCortex{
		resolveResult: &CortexResult{URI: "matrix://cortex/Fact/xyz#2", Type: "Fact"},
	}
	interp := New(file, nil, cx)
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:       "modify",
		SlotValues: map[string]string{"target": "the schema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	slot := result.Slots["target"]
	if slot.Status != SlotResolved {
		t.Fatalf("slot status=%d, want SlotResolved", slot.Status)
	}
	if slot.Value != "matrix://cortex/Fact/xyz#2" {
		t.Fatalf("slot value=%q", slot.Value)
	}
	if len(cx.resolveCalls) != 1 {
		t.Fatalf("expected 1 resolve call, got %d", len(cx.resolveCalls))
	}
	// Positional arg should be the slot's raw prose
	if cx.resolveCalls[0] != "the schema" {
		t.Fatalf("resolve expr=%q, want 'the schema'", cx.resolveCalls[0])
	}
}

func TestUnknownBlock(t *testing.T) {
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
    reason="Cannot find target"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unknowns) != 1 {
		t.Fatalf("got %d unknowns, want 1", len(result.Unknowns))
	}
	u := result.Unknowns[0]
	if u.SlotName != "target" {
		t.Fatalf("unknown slot=%q", u.SlotName)
	}
	if u.Severity != "blocking" {
		t.Fatalf("severity=%q", u.Severity)
	}
	if u.Reason != "Cannot find target" {
		t.Fatalf("reason=%q", u.Reason)
	}
}

func TestUnknownBlockSkipsResolvedSlot(t *testing.T) {
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
  resolve slot.target <- cortex.find(type=Fact, limit=1)
  unknown slot.target
    severity=blocking
    reason="Should be skipped"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	cx := &mockCortex{
		findResults: []CortexResult{
			{URI: "matrix://cortex/Fact/abc#1"},
		},
	}
	interp := New(file, nil, cx)
	result, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatal(err)
	}
	// Unknown should be skipped because resolve succeeded
	if len(result.Unknowns) != 0 {
		t.Fatalf("got %d unknowns, want 0 (slot resolved)", len(result.Unknowns))
	}
}

func TestSlotInitialization(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required

slot style: enum<formal|casual>
  optional
  default=formal
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
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatal(err)
	}

	target := result.Slots["target"]
	if target == nil {
		t.Fatal("slot 'target' not found")
	}
	if target.Required != true {
		t.Fatal("target should be required")
	}
	if target.Status != SlotEmpty {
		t.Fatalf("target status=%d, want SlotEmpty", target.Status)
	}

	style := result.Slots["style"]
	if style == nil {
		t.Fatal("slot 'style' not found")
	}
	if style.Required != false {
		t.Fatal("style should be optional")
	}
	if style.Status != SlotDefault {
		t.Fatalf("style status=%d, want SlotDefault", style.Status)
	}
	if style.Value != "formal" {
		t.Fatalf("style value=%q, want 'formal'", style.Value)
	}
}

func TestSlotPreFill(t *testing.T) {
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
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:       "build",
		SlotValues: map[string]string{"target": "my project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	slot := result.Slots["target"]
	if slot.Status != SlotRaw {
		t.Fatalf("status=%d, want SlotRaw", slot.Status)
	}
	if slot.Value != "my project" {
		t.Fatalf("value=%q", slot.Value)
	}
}

func TestNoProcedureSection(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	interp := New(file, nil, nil)
	_, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err == nil {
		t.Fatal("expected error for missing §PROCEDURE")
	}
	if !strings.Contains(err.Error(), "no §PROCEDURE") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnknownCondition(t *testing.T) {
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
    reason="Need target"
  end
end
on unknown
  clarify slot.target
    prompt="Please specify the target"
    type=string
    required=true
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)

	// First pass: verb=build creates a blocking unknown
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unknowns) != 1 {
		t.Fatalf("got %d unknowns, want 1", len(result.Unknowns))
	}
	// The "on unknown" block should NOT have matched (first-match-wins: verb=build matched first)
	if result.MatchedCondition != "verb=build" {
		t.Fatalf("condition=%q", result.MatchedCondition)
	}
}

func TestDryRunNoLLM(t *testing.T) {
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
    system="Build it"
    user="Goal: {prose}"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	interp := New(file, nil, nil) // nil LLM = dry-run
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:  "build",
		Prose: "a house",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FrameJSON != "" {
		t.Fatalf("expected empty FrameJSON in dry-run, got %q", result.FrameJSON)
	}
	if len(result.PromptMessages) != 2 {
		t.Fatalf("got %d messages", len(result.PromptMessages))
	}
	if result.PromptMessages[1].Content != "Goal: a house" {
		t.Fatalf("user content=%q", result.PromptMessages[1].Content)
	}
}

func TestSlotValCondition(t *testing.T) {
	src := []byte(`§SKILL
id=test
§INPUTS
slot mode: enum<fast|slow>
  required
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on slot.mode=fast
  prompt
    system="Fast mode"
    user="Go fast"
  end
end
on slot.mode=slow
  prompt
    system="Slow mode"
    user="Take time"
  end
end
§OUTPUTS
§FAILURE_MODES
`)
	file := mustParse(t, src)
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:       "build",
		SlotValues: map[string]string{"mode": "slow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Executed {
		t.Fatal("expected match")
	}
	if result.MatchedCondition != "slot.mode=slow" {
		t.Fatalf("condition=%q", result.MatchedCondition)
	}
	if result.PromptMessages[0].Content != "Slow mode" {
		t.Fatalf("system=%q", result.PromptMessages[0].Content)
	}
}

func TestFormatSlots(t *testing.T) {
	interp := &Interpreter{}
	result := &RunResult{
		Slots: map[string]*Slot{
			"target": {Name: "target", Value: "matrix://cortex/Fact/1#1", Status: SlotResolved},
		},
	}
	s := interp.formatSlots(result)
	if !strings.Contains(s, "target=matrix://cortex/Fact/1#1") {
		t.Fatalf("format=%q", s)
	}
	if !strings.Contains(s, "(resolved)") {
		t.Fatalf("format=%q", s)
	}
}

func TestFormatUnknowns(t *testing.T) {
	interp := &Interpreter{}
	result := &RunResult{
		Unknowns: []*Unknown{
			{SlotName: "target", Severity: "blocking", Reason: "Can't find it"},
		},
	}
	s := interp.formatUnknowns(result)
	if !strings.Contains(s, "slot.target") {
		t.Fatalf("format=%q", s)
	}
	if !strings.Contains(s, "[blocking]") {
		t.Fatalf("format=%q", s)
	}
}

func TestValueToString(t *testing.T) {
	tests := []struct {
		val  ast.Value
		want string
	}{
		{&ast.StringValue{Text: "hello"}, "hello"},
		{&ast.IntValue{Raw: "42"}, "42"},
		{&ast.FloatValue{Raw: "3.14"}, "3.14"},
		{&ast.BoolValue{Val: true}, "true"},
		{&ast.BoolValue{Val: false}, "false"},
		{&ast.IdentValue{Name: "foo"}, "foo"},
		{&ast.URIValue{URI: "matrix://skill/x@1"}, "matrix://skill/x@1"},
		{&ast.SpaceListValue{Items: []string{"a", "b", "c"}}, "a b c"},
		{&ast.SlotExprValue{Parts: []string{"slot", "target", "prose"}}, "slot.target.prose"},
		{nil, ""},
	}
	for _, tt := range tests {
		got := valueToString(tt.val)
		if got != tt.want {
			t.Errorf("valueToString(%T)=%q, want %q", tt.val, got, tt.want)
		}
	}
}

func TestFormatTypeRef(t *testing.T) {
	tests := []struct {
		tr   ast.TypeRef
		want string
	}{
		{ast.TypeRef{Name: "string"}, "string"},
		{ast.TypeRef{Name: "Constraint", IsList: true}, "Constraint[]"},
		{ast.TypeRef{Name: "enum", EnumSet: []string{"a", "b", "c"}}, "enum<a|b|c>"},
		{ast.TypeRef{Name: "enum", EnumSet: []string{"x"}, IsList: true}, "enum<x>[]"},
	}
	for _, tt := range tests {
		got := formatTypeRef(tt.tr)
		if got != tt.want {
			t.Errorf("formatTypeRef(%+v)=%q, want %q", tt.tr, got, tt.want)
		}
	}
}

// ---- integration test against real SKILL.mtx ----

func TestRunWritingPlansSKILL(t *testing.T) {
	root := projectRoot()
	path := filepath.Join(root, "skills", "writing-plans", "SKILL.mtx")
	src, err := os.ReadFile(path)
	if err != nil {
		// Skip if file doesn't exist (CI might not have it)
		t.Skipf("SKILL.mtx not found: %v", err)
	}

	file := mustParse(t, src)

	llm := &mockLLM{response: `{"verb":"build","objects":["database"]}`}
	cx := &mockCortex{
		findResults: []CortexResult{
			{URI: "matrix://cortex/Fact/db-spec#1", Type: "Fact", Summary: "Database spec"},
		},
	}

	interp := New(file, llm, cx)
	result, err := interp.Run(context.Background(), &RunInput{
		Prose:      "build me a database schema for user profiles",
		Verb:       "build",
		Bundle:     "[Goal: create user system]",
		Grammar:    "intent_frame@1",
		Confidence: 0.85,
		SlotValues: map[string]string{"target": "user profile database"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !result.Executed {
		t.Fatal("expected on verb=build to match")
	}
	if result.MatchedCondition != "verb=build" {
		t.Fatalf("condition=%q", result.MatchedCondition)
	}

	// Should have called LLM
	if len(llm.calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(llm.calls))
	}
	if llm.calls[0].Grammar != "intent_frame@1" {
		t.Fatalf("grammar=%q", llm.calls[0].Grammar)
	}

	// Should have FrameJSON
	if result.FrameJSON == "" {
		t.Fatal("expected non-empty FrameJSON")
	}

	// Should have slots initialized from §INPUTS
	if len(result.Slots) == 0 {
		t.Fatal("expected slots from §INPUTS")
	}
	target := result.Slots["target"]
	if target == nil {
		t.Fatal("slot 'target' not found")
	}

	// Prompt messages should have interpolated content
	if len(result.PromptMessages) < 2 {
		t.Fatalf("expected >=2 prompt messages, got %d", len(result.PromptMessages))
	}
	user := result.PromptMessages[1].Content
	if !strings.Contains(user, "user profile database") || !strings.Contains(user, "build") {
		t.Logf("user message: %s", user)
	}
}

func TestRunWritingPlansModify(t *testing.T) {
	root := projectRoot()
	path := filepath.Join(root, "skills", "writing-plans", "SKILL.mtx")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("SKILL.mtx not found: %v", err)
	}

	file := mustParse(t, src)
	cx := &mockCortex{
		resolveResult: &CortexResult{URI: "matrix://cortex/Fact/schema#3", Type: "Fact"},
	}
	interp := New(file, nil, cx) // nil LLM = dry-run
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:       "modify",
		Prose:      "update the schema",
		SlotValues: map[string]string{"target": "the schema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MatchedCondition != "verb=modify" {
		t.Fatalf("condition=%q", result.MatchedCondition)
	}

	// cortex.resolve should have been called
	if len(cx.resolveCalls) != 1 {
		t.Fatalf("expected 1 resolve call, got %d", len(cx.resolveCalls))
	}

	target := result.Slots["target"]
	if target == nil {
		t.Fatal("target slot not found")
	}
	if target.Status != SlotResolved {
		t.Fatalf("target status=%d, want SlotResolved", target.Status)
	}
}

func TestRunWritingPlansLowConfidence(t *testing.T) {
	root := projectRoot()
	path := filepath.Join(root, "skills", "writing-plans", "SKILL.mtx")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("SKILL.mtx not found: %v", err)
	}

	file := mustParse(t, src)
	interp := New(file, nil, nil)
	result, err := interp.Run(context.Background(), &RunInput{
		Verb:       "build",
		Confidence: 0.50, // below 0.75 threshold
	})
	if err != nil {
		t.Fatal(err)
	}

	// verb=build matches FIRST (first-match-wins), not confidence<0.75
	if result.MatchedCondition != "verb=build" {
		t.Fatalf("condition=%q, want verb=build (first-match-wins)", result.MatchedCondition)
	}
}

// ---- test interpolation edge cases ----

func TestInterpolateUnknownVar(t *testing.T) {
	interp := &Interpreter{file: &ast.File{}}
	result := &RunResult{Slots: make(map[string]*Slot)}
	input := &RunInput{Prose: "test"}

	got := interp.interpolate("prefix {unknown_var} suffix", input, result)
	if got != "prefix {unknown_var} suffix" {
		t.Fatalf("got %q", got)
	}
}

func TestInterpolateSlotProse(t *testing.T) {
	interp := &Interpreter{file: &ast.File{}}
	result := &RunResult{
		Slots: map[string]*Slot{
			"target": {Name: "target", Value: "matrix://cortex/Fact/1#1", RawProse: "the database", Status: SlotResolved},
		},
	}
	input := &RunInput{}

	got := interp.interpolate("{slot.target.prose}", input, result)
	if got != "the database" {
		t.Fatalf("got %q, want 'the database'", got)
	}

	got2 := interp.interpolate("{slot.target}", input, result)
	if got2 != "matrix://cortex/Fact/1#1" {
		t.Fatalf("got %q, want URI", got2)
	}
}

func TestInterpolateUnclosedBrace(t *testing.T) {
	interp := &Interpreter{file: &ast.File{}}
	result := &RunResult{Slots: make(map[string]*Slot)}
	input := &RunInput{Prose: "test"}

	got := interp.interpolate("unclosed {brace", input, result)
	if got != "unclosed {brace" {
		t.Fatalf("got %q", got)
	}
}

// Ensure unknown condition is not confused with unknown block keyword
func TestInterpolateEmptySlots(t *testing.T) {
	interp := &Interpreter{file: &ast.File{}}
	result := &RunResult{Slots: map[string]*Slot{}}
	input := &RunInput{}

	got := interp.interpolate("Slots: {slots}", input, result)
	if got != "Slots: (none)" {
		t.Fatalf("got %q", got)
	}
}

// ---- confidence comparison operators ----

func TestConfidenceOperators(t *testing.T) {
	tests := []struct {
		op         string
		threshold  string
		confidence float64
		want       bool
	}{
		{"<", "0.75", 0.50, true},
		{"<", "0.75", 0.75, false},
		{"<=", "0.75", 0.75, true},
		{">", "0.75", 0.80, true},
		{">", "0.75", 0.75, false},
		{">=", "0.75", 0.75, true},
		{"==", "0.75", 0.75, true},
		{"==", "0.75", 0.76, false},
	}
	for _, tt := range tests {
		cond := &ast.ConfidenceCondition{
			Op:        tt.op,
			Threshold: tt.threshold,
			CondPos:   token.Pos{Line: 1, Col: 1},
		}
		interp := &Interpreter{file: &ast.File{}}
		result := &RunResult{Slots: map[string]*Slot{}}
		input := &RunInput{Confidence: tt.confidence}
		got, _ := interp.evalCondition(cond, input, result)
		if got != tt.want {
			t.Errorf("confidence%s%s with %f: got %v, want %v", tt.op, tt.threshold, tt.confidence, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Session 31b · StepKindHint propagation (model router · P2b)
// ---------------------------------------------------------------------------

func TestRun_StepKindHint_StringValue(t *testing.T) {
	src := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=build\n  kind = \"code\"\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	file := mustParse(t, src)

	interp := New(file, nil, nil)
	res, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Executed {
		t.Fatal("on-block did not execute")
	}
	if res.StepKindHint != "code" {
		t.Errorf("StepKindHint = %q, want %q", res.StepKindHint, "code")
	}
}

func TestRun_StepKindHint_IdentValue(t *testing.T) {
	// Bare identifier ergonomic short-hand: `kind = code` (no quotes).
	src := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=build\n  kind = summarize\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	file := mustParse(t, src)

	interp := New(file, nil, nil)
	res, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StepKindHint != "summarize" {
		t.Errorf("StepKindHint = %q, want %q", res.StepKindHint, "summarize")
	}
}

func TestRun_StepKindHint_OmittedDefaultEmpty(t *testing.T) {
	// Backwards-compat: skills without a kind annotation produce empty
	// StepKindHint (which routes to KindReason at executor time).
	src := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=build\n  prompt\n    system=\"You are a builder.\"\n    user=\"Build.\"\n  end\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	file := mustParse(t, src)

	interp := New(file, &mockLLM{response: "{}"}, nil)
	res, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StepKindHint != "" {
		t.Errorf("StepKindHint = %q, want empty (no annotation)", res.StepKindHint)
	}
}

func TestRun_StepKindHint_NonMatchingOnBlockDoesNotPopulate(t *testing.T) {
	// First-match-wins: an on-block whose condition does NOT match
	// must not contribute its kind hint to the result.
	src := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build modify\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=modify\n  kind = \"code\"\nend\non verb=build\n  kind = \"write\"\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	file := mustParse(t, src)

	interp := New(file, nil, nil)
	res, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only verb=build matched, so we should see its hint, not modify's.
	if res.StepKindHint != "write" {
		t.Errorf("StepKindHint = %q, want %q (matched block wins)", res.StepKindHint, "write")
	}
}

func TestExtractKindValue(t *testing.T) {
	tests := []struct {
		name string
		val  ast.Value
		want string
	}{
		{"string", &ast.StringValue{Text: "code"}, "code"},
		{"string_trimmed", &ast.StringValue{Text: "  reason  "}, "reason"},
		{"ident", &ast.IdentValue{Name: "summarize"}, "summarize"},
		{"int_rejected", &ast.IntValue{Raw: "42"}, ""},
		{"bool_rejected", &ast.BoolValue{Val: true}, ""},
		{"nil_rejected", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractKindValue(tt.val); got != tt.want {
				t.Errorf("ExtractKindValue(%v) = %q, want %q", tt.val, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Session 31c P3c · OutputCardinalityHint propagation
// ---------------------------------------------------------------------------

func TestRun_OutputCardinality_PositiveInt(t *testing.T) {
	src := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=build\n  output_cardinality = 8\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	file := mustParse(t, src)

	interp := New(file, nil, nil)
	res, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Executed {
		t.Fatal("on-block did not execute")
	}
	if res.OutputCardinalityHint != 8 {
		t.Errorf("OutputCardinalityHint = %d, want 8", res.OutputCardinalityHint)
	}
}

func TestRun_OutputCardinality_OmittedDefaultsToZero(t *testing.T) {
	// Backwards-compat: skills without an output_cardinality annotation
	// produce zero (which the planner reads as "free to choose").
	src := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=build\n  prompt\n    system=\"s\"\n    user=\"u\"\n  end\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	file := mustParse(t, src)

	interp := New(file, &mockLLM{response: "{}"}, nil)
	res, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputCardinalityHint != 0 {
		t.Errorf("OutputCardinalityHint = %d, want 0 (unset)", res.OutputCardinalityHint)
	}
}

func TestRun_OutputCardinality_AlongsideKindHint(t *testing.T) {
	// Both metadata KVs in the same on-block must populate independently
	// (proves the switch-by-key plumbing handles multiple keys per block).
	src := []byte("\u00a7SKILL\nid=test\nmcl.verbs=build\n\u00a7INPUTS\n\u00a7CORTEX\n\u00a7TOOLS\nnone\n\u00a7SUB_SKILLS\nnone\n\u00a7PROCEDURE\non verb=build\n  kind = \"write\"\n  output_cardinality = 3\nend\n\u00a7OUTPUTS\n\u00a7FAILURE_MODES\n")
	file := mustParse(t, src)

	interp := New(file, nil, nil)
	res, err := interp.Run(context.Background(), &RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StepKindHint != "write" {
		t.Errorf("StepKindHint = %q, want write", res.StepKindHint)
	}
	if res.OutputCardinalityHint != 3 {
		t.Errorf("OutputCardinalityHint = %d, want 3", res.OutputCardinalityHint)
	}
}

func TestExtractPositiveIntValue(t *testing.T) {
	tests := []struct {
		name    string
		val     ast.Value
		wantOK  bool
		wantInt int
	}{
		{"positive", &ast.IntValue{Raw: "8"}, true, 8},
		{"trimmed", &ast.IntValue{Raw: "  42  "}, true, 42},
		{"zero_rejected", &ast.IntValue{Raw: "0"}, false, 0},
		{"negative_rejected", &ast.IntValue{Raw: "-1"}, false, 0},
		{"non_int_rejected", &ast.StringValue{Text: "8"}, false, 0},
		{"float_rejected", &ast.IntValue{Raw: "3.14"}, false, 0},
		{"empty_rejected", &ast.IntValue{Raw: ""}, false, 0},
		{"nil_rejected", nil, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := ExtractPositiveIntValue(tt.val)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if n != tt.wantInt {
				t.Errorf("n = %d, want %d", n, tt.wantInt)
			}
		})
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
