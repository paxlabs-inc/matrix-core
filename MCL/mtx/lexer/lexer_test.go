// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package lexer

import (
	"os"
	"strings"
	"testing"

	"matrix/mcl/mtx/token"
)

// ---- unit tests for individual constructs ----

func TestSectionHeader(t *testing.T) {
	l := New([]byte("§SKILL\n"))
	tok := l.NextToken()
	expectToken(t, tok, token.SECTION, "§SKILL")
	tok = l.NextToken()
	expectToken(t, tok, token.NEWLINE, "\n")
	tok = l.NextToken()
	expectType(t, tok, token.EOF)
}

func TestKVPair(t *testing.T) {
	l := New([]byte("id=writing-plans\n"))
	toks := l.AllTokens()

	expected := []token.Type{token.IDENT, token.EQUALS, token.IDENT, token.NEWLINE, token.EOF}
	if len(toks) != len(expected) {
		t.Fatalf("got %d tokens, want %d: %v", len(toks), len(expected), toks)
	}
	for i, want := range expected {
		if toks[i].Type != want {
			t.Errorf("token[%d]: got %v, want %v", i, toks[i].Type, want)
		}
	}
	// "id" is a plain IDENT (not a keyword)
	if toks[0].Literal != "id" {
		t.Errorf("token[0].Literal: got %q, want %q", toks[0].Literal, "id")
	}
	if toks[2].Literal != "writing-plans" {
		t.Errorf("token[2].Literal: got %q, want %q", toks[2].Literal, "writing-plans")
	}
}

func TestStringLiteral(t *testing.T) {
	l := New([]byte(`system="Hello \"world\"\n"` + "\n"))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_SYSTEM, "system")
	expectToken(t, toks[1], token.EQUALS, "=")
	expectToken(t, toks[2], token.STRING, "Hello \"world\"\n")
	expectToken(t, toks[3], token.NEWLINE, "\n")
}

func TestStringInterpolation(t *testing.T) {
	// The lexer passes interpolation chars through as raw string content.
	// Parser handles interpolation extraction.
	l := New([]byte(`user="Goal: {prose} verb: {verb}"` + "\n"))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_USER, "user")
	expectToken(t, toks[1], token.EQUALS, "=")
	expectToken(t, toks[2], token.STRING, "Goal: {prose} verb: {verb}")
}

func TestNumberLiterals(t *testing.T) {
	l := New([]byte("42\n"))
	tok := l.NextToken()
	expectToken(t, tok, token.INT, "42")

	l = New([]byte("0.75\n"))
	tok = l.NextToken()
	expectToken(t, tok, token.FLOAT, "0.75")
}

func TestBooleans(t *testing.T) {
	l := New([]byte("required=true\n"))
	toks := l.AllTokens()
	expectToken(t, toks[0], token.KW_REQUIRED, "required")
	expectToken(t, toks[1], token.EQUALS, "=")
	expectToken(t, toks[2], token.BOOL_TRUE, "true")
}

func TestURILiteral(t *testing.T) {
	l := New([]byte("matrix://skill/writing-plans@1.0.0\n"))
	tok := l.NextToken()
	expectToken(t, tok, token.URI, "matrix://skill/writing-plans@1.0.0")
}

func TestURIInKVPair(t *testing.T) {
	l := New([]byte("ref=matrix://tool/registry/query@2.0\n"))
	toks := l.AllTokens()
	expectToken(t, toks[0], token.IDENT, "ref")
	expectToken(t, toks[1], token.EQUALS, "=")
	expectToken(t, toks[2], token.URI, "matrix://tool/registry/query@2.0")
}

func TestIndent(t *testing.T) {
	l := New([]byte("  required\n"))
	toks := l.AllTokens()
	expectToken(t, toks[0], token.INDENT, "  ")
	expectToken(t, toks[1], token.KW_REQUIRED, "required")
	expectToken(t, toks[2], token.NEWLINE, "\n")
}

func TestComment(t *testing.T) {
	// Comments are skipped; the lexer returns the NEWLINE after.
	l := New([]byte("# this is a comment\nid=test\n"))
	toks := l.AllTokens()

	// First real token should be NEWLINE from the comment line
	expectType(t, toks[0], token.NEWLINE)
	expectToken(t, toks[1], token.IDENT, "id")
}

func TestOnBlock(t *testing.T) {
	src := "on verb=build\nend\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_ON, "on")
	expectToken(t, toks[1], token.KW_VERB, "verb")
	expectToken(t, toks[2], token.EQUALS, "=")
	expectToken(t, toks[3], token.KW_BUILD, "build")
	expectToken(t, toks[4], token.NEWLINE, "\n")
	expectToken(t, toks[5], token.KW_END, "end")
	expectToken(t, toks[6], token.NEWLINE, "\n")
}

func TestConfidenceCondition(t *testing.T) {
	src := "on confidence<0.75\nend\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_ON, "on")
	expectToken(t, toks[1], token.KW_CONFIDENCE, "confidence")
	expectToken(t, toks[2], token.LT, "<")
	expectToken(t, toks[3], token.FLOAT, "0.75")
}

func TestComparators(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want token.Type
		lit  string
	}{
		{"<", token.LT, "<"},
		{"<=", token.LTEQ, "<="},
		{">", token.GT, ">"},
		{">=", token.GTEQ, ">="},
		{"==", token.EQEQ, "=="},
		{"<-", token.ARROW, "<-"},
	} {
		l := New([]byte(tc.in + "\n"))
		tok := l.NextToken()
		if tok.Type != tc.want || tok.Literal != tc.lit {
			t.Errorf("input %q: got %v %q, want %v %q", tc.in, tok.Type, tok.Literal, tc.want, tc.lit)
		}
	}
}

func TestResolveStatement(t *testing.T) {
	src := `resolve slot.target <- cortex.find(type=Fact, near=slot.target.prose, limit=5)` + "\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_RESOLVE, "resolve")
	expectToken(t, toks[1], token.KW_SLOT, "slot")
	expectToken(t, toks[2], token.DOT, ".")
	expectToken(t, toks[3], token.IDENT, "target")
	expectToken(t, toks[4], token.ARROW, "<-")
	expectToken(t, toks[5], token.KW_CORTEX_FIND, "cortex.find")
	expectToken(t, toks[6], token.LPAREN, "(")
	expectToken(t, toks[7], token.IDENT, "type")
	expectToken(t, toks[8], token.EQUALS, "=")
	expectToken(t, toks[9], token.IDENT, "Fact")
}

func TestCortexFunctions(t *testing.T) {
	for _, fn := range []struct {
		in   string
		want token.Type
	}{
		{"cortex.find()", token.KW_CORTEX_FIND},
		{"cortex.resolve()", token.KW_CORTEX_RESOLVE},
		{"cortex.context()", token.KW_CORTEX_CONTEXT},
	} {
		l := New([]byte(fn.in + "\n"))
		tok := l.NextToken()
		if tok.Type != fn.want {
			t.Errorf("input %q: got %v, want %v", fn.in, tok.Type, fn.want)
		}
	}
}

func TestCortexDotUnknown(t *testing.T) {
	// "cortex.bundle" is NOT a cortex function keyword — should emit IDENT "cortex" + DOT + IDENT "bundle"
	l := New([]byte("cortex.bundle\n"))
	toks := l.AllTokens()
	expectToken(t, toks[0], token.IDENT, "cortex")
	expectToken(t, toks[1], token.DOT, ".")
	expectToken(t, toks[2], token.IDENT, "bundle")
}

func TestSlotDeclaration(t *testing.T) {
	src := "slot target: ArtifactRef\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_SLOT, "slot")
	expectToken(t, toks[1], token.IDENT, "target")
	expectToken(t, toks[2], token.COLON, ":")
	expectToken(t, toks[3], token.IDENT, "ArtifactRef")
}

func TestEnumType(t *testing.T) {
	src := "slot style: enum<formal|casual|technical>\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_SLOT, "slot")
	expectToken(t, toks[1], token.IDENT, "style")
	expectToken(t, toks[2], token.COLON, ":")
	expectToken(t, toks[3], token.KW_ENUM, "enum")
	expectToken(t, toks[4], token.LT, "<")
	expectToken(t, toks[5], token.IDENT, "formal")
	expectToken(t, toks[6], token.PIPE, "|")
	expectToken(t, toks[7], token.IDENT, "casual")
	expectToken(t, toks[8], token.PIPE, "|")
	expectToken(t, toks[9], token.IDENT, "technical")
	expectToken(t, toks[10], token.GT, ">")
}

func TestListType(t *testing.T) {
	src := "slot constraints: Constraint[]\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_SLOT, "slot")
	expectToken(t, toks[1], token.IDENT, "constraints")
	expectToken(t, toks[2], token.COLON, ":")
	expectToken(t, toks[3], token.IDENT, "Constraint")
	expectToken(t, toks[4], token.LBRACKET, "[")
	expectToken(t, toks[5], token.RBRACKET, "]")
}

func TestPromptBlock(t *testing.T) {
	src := "prompt\n  system=\"Hello\"\n  user=\"World\"\nend\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_PROMPT, "prompt")
	expectToken(t, toks[1], token.NEWLINE, "\n")
	expectToken(t, toks[2], token.INDENT, "  ")
	expectToken(t, toks[3], token.KW_SYSTEM, "system")
	expectToken(t, toks[4], token.EQUALS, "=")
	expectToken(t, toks[5], token.STRING, "Hello")
	expectToken(t, toks[6], token.NEWLINE, "\n")
	expectToken(t, toks[7], token.INDENT, "  ")
	expectToken(t, toks[8], token.KW_USER, "user")
	expectToken(t, toks[9], token.EQUALS, "=")
	expectToken(t, toks[10], token.STRING, "World")
	expectToken(t, toks[11], token.NEWLINE, "\n")
	expectToken(t, toks[12], token.KW_END, "end")
}

func TestSpaceList(t *testing.T) {
	src := "mcl.verbs=build modify delegate\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	// mcl . verbs = build modify delegate \n EOF
	expectToken(t, toks[0], token.IDENT, "mcl")
	expectToken(t, toks[1], token.DOT, ".")
	expectToken(t, toks[2], token.IDENT, "verbs")
	expectToken(t, toks[3], token.EQUALS, "=")
	expectToken(t, toks[4], token.KW_BUILD, "build")
	expectToken(t, toks[5], token.KW_MODIFY, "modify")
	expectToken(t, toks[6], token.KW_DELEGATE, "delegate")
}

func TestFailureEntry(t *testing.T) {
	src := "budget_exceeded\n  suggest=raise_budget\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.IDENT, "budget_exceeded")
	expectToken(t, toks[1], token.NEWLINE, "\n")
	expectToken(t, toks[2], token.INDENT, "  ")
	expectToken(t, toks[3], token.KW_SUGGEST, "suggest")
	expectToken(t, toks[4], token.EQUALS, "=")
	expectToken(t, toks[5], token.KW_RAISE_BUDGET, "raise_budget")
}

func TestNoneKeyword(t *testing.T) {
	l := New([]byte("none\n"))
	tok := l.NextToken()
	expectToken(t, tok, token.KW_NONE, "none")
}

func TestCRLFNormalisation(t *testing.T) {
	l := New([]byte("id=test\r\nversion=1\r\n"))
	toks := l.AllTokens()

	// Should tokenise identically to LF version
	var types []token.Type
	for _, tok := range toks {
		types = append(types, tok.Type)
	}
	expected := []token.Type{
		token.IDENT, token.EQUALS, token.IDENT, token.NEWLINE,
		token.IDENT, token.EQUALS, token.INT, token.NEWLINE,
		token.EOF,
	}
	if len(types) != len(expected) {
		t.Fatalf("CRLF: got %d tokens, want %d: %v", len(types), len(expected), toks)
	}
	for i, want := range expected {
		if types[i] != want {
			t.Errorf("CRLF token[%d]: got %v, want %v", i, types[i], want)
		}
	}
}

func TestPositionTracking(t *testing.T) {
	l := New([]byte("§SKILL\nid=test\n"))
	tok := l.NextToken()
	if tok.Pos.Line != 1 || tok.Pos.Col != 1 {
		t.Errorf("§SKILL position: got %d:%d, want 1:1", tok.Pos.Line, tok.Pos.Col)
	}
	l.NextToken() // NEWLINE

	tok = l.NextToken() // id
	if tok.Pos.Line != 2 || tok.Pos.Col != 1 {
		t.Errorf("id position: got %d:%d, want 2:1", tok.Pos.Line, tok.Pos.Col)
	}
}

func TestUnknownBlock(t *testing.T) {
	src := "unknown slot.target\n  severity=blocking\nend\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_UNKNOWN, "unknown")
	expectToken(t, toks[1], token.KW_SLOT, "slot")
	expectToken(t, toks[2], token.DOT, ".")
	expectToken(t, toks[3], token.IDENT, "target")
	expectToken(t, toks[4], token.NEWLINE, "\n")
	expectToken(t, toks[5], token.INDENT, "  ")
	expectToken(t, toks[6], token.IDENT, "severity")
	expectToken(t, toks[7], token.EQUALS, "=")
	expectToken(t, toks[8], token.KW_BLOCKING, "blocking")
}

func TestClarifyBlock(t *testing.T) {
	src := "clarify slot.target\n  prompt=\"Which one?\"\n  type=ArtifactRef\n  required=true\nend\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.KW_CLARIFY, "clarify")
	expectToken(t, toks[1], token.KW_SLOT, "slot")
	expectToken(t, toks[2], token.DOT, ".")
	expectToken(t, toks[3], token.IDENT, "target")
	expectToken(t, toks[4], token.NEWLINE, "\n")
	expectToken(t, toks[5], token.INDENT, "  ")
	expectToken(t, toks[6], token.KW_PROMPT, "prompt")
	expectToken(t, toks[7], token.EQUALS, "=")
	expectToken(t, toks[8], token.STRING, "Which one?")
}

func TestOptionsBracket(t *testing.T) {
	src := `options=[option1 option2 option3]` + "\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.IDENT, "options")
	expectToken(t, toks[1], token.EQUALS, "=")
	expectToken(t, toks[2], token.LBRACKET, "[")
	expectToken(t, toks[3], token.IDENT, "option1")
	expectToken(t, toks[4], token.IDENT, "option2")
	expectToken(t, toks[5], token.IDENT, "option3")
	expectToken(t, toks[6], token.RBRACKET, "]")
}

func TestDottedKVKey(t *testing.T) {
	src := "stage.1.id=normalise\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.IDENT, "stage")
	expectToken(t, toks[1], token.DOT, ".")
	expectToken(t, toks[2], token.INT, "1")
	expectToken(t, toks[3], token.DOT, ".")
	expectToken(t, toks[4], token.IDENT, "id")
	expectToken(t, toks[5], token.EQUALS, "=")
	expectToken(t, toks[6], token.IDENT, "normalise")
}

func TestHashSection(t *testing.T) {
	src := "§HASH\nv=1\nalgo=sha256_ast\ndigest=\n"
	l := New([]byte(src))
	toks := l.AllTokens()

	expectToken(t, toks[0], token.SECTION, "§HASH")
	expectToken(t, toks[1], token.NEWLINE, "\n")
	expectToken(t, toks[2], token.IDENT, "v")
	expectToken(t, toks[3], token.EQUALS, "=")
	expectToken(t, toks[4], token.INT, "1")
}

// ---- integration tests against real .mtx files ----

const (
	testCoreDir  = "/root/matrix/MCL/core"
	testSkillDir = "/root/matrix/skills/writing-plans"
)

func TestLexVerbMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/verb.mtx")
	l := New(src)
	toks := l.AllTokens()

	assertNoIllegal(t, toks, "verb.mtx")
	assertHasSection(t, toks, "§VERB")
	assertContainsKeyword(t, toks, token.KW_FIND)
	assertContainsKeyword(t, toks, token.KW_BUILD)
	assertContainsLiteral(t, toks, token.STRING, "") // at least one string
	assertEndsWithEOF(t, toks)
}

func TestLexFrameMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/frame.mtx")
	l := New(src)
	toks := l.AllTokens()

	assertNoIllegal(t, toks, "frame.mtx")
	assertHasSection(t, toks, "§FRAME")
	assertContainsKeyword(t, toks, token.KW_SLOT)
	assertContainsKeyword(t, toks, token.KW_ENUM)
	assertEndsWithEOF(t, toks)
}

func TestLexPipelineMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/pipeline.mtx")
	l := New(src)
	toks := l.AllTokens()

	assertNoIllegal(t, toks, "pipeline.mtx")
	assertHasSection(t, toks, "§PIPELINE")
	assertContainsKeyword(t, toks, token.KW_CORTEX_CONTEXT)
	assertEndsWithEOF(t, toks)
}

func TestLexConfidenceMtx(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/confidence.mtx")
	l := New(src)
	toks := l.AllTokens()

	assertNoIllegal(t, toks, "confidence.mtx")
	assertHasSection(t, toks, "§CONFIDENCE")
	assertEndsWithEOF(t, toks)
}

func TestLexWritingPlansSKILLMtx(t *testing.T) {
	src := readTestFile(t, testSkillDir+"/SKILL.mtx")
	l := New(src)
	toks := l.AllTokens()

	assertNoIllegal(t, toks, "SKILL.mtx")
	assertHasSection(t, toks, "§SKILL")
	assertHasSection(t, toks, "§INPUTS")
	assertHasSection(t, toks, "§CORTEX")
	assertHasSection(t, toks, "§TOOLS")
	assertHasSection(t, toks, "§SUB_SKILLS")
	assertHasSection(t, toks, "§PROCEDURE")
	assertHasSection(t, toks, "§OUTPUTS")
	assertHasSection(t, toks, "§FAILURE_MODES")
	assertHasSection(t, toks, "§HASH")
	assertContainsKeyword(t, toks, token.KW_ON)
	assertContainsKeyword(t, toks, token.KW_END)
	assertContainsKeyword(t, toks, token.KW_PROMPT)
	assertContainsKeyword(t, toks, token.KW_RESOLVE)
	assertContainsKeyword(t, toks, token.KW_UNKNOWN)
	assertContainsKeyword(t, toks, token.KW_CLARIFY)
	assertContainsKeyword(t, toks, token.KW_CORTEX_FIND)
	assertContainsKeyword(t, toks, token.KW_CORTEX_RESOLVE)
	assertContainsKeyword(t, toks, token.KW_NONE)
	assertEndsWithEOF(t, toks)
}

func TestLexVerbMtxTokenCount(t *testing.T) {
	src := readTestFile(t, testCoreDir+"/verb.mtx")
	l := New(src)
	toks := l.AllTokens()

	// Sanity: a 43-line file should produce a reasonable number of tokens.
	// With comments stripped and blank lines emitting NEWLINE, expect > 50 tokens.
	if len(toks) < 50 {
		t.Errorf("verb.mtx: only %d tokens; expected > 50", len(toks))
	}
}

func TestLexWritingPlansSKILLMtxTokenCount(t *testing.T) {
	src := readTestFile(t, testSkillDir+"/SKILL.mtx")
	l := New(src)
	toks := l.AllTokens()

	// Full SKILL.mtx with procedure blocks should produce many tokens.
	if len(toks) < 100 {
		t.Errorf("SKILL.mtx: only %d tokens; expected > 100", len(toks))
	}
}

// ---- helpers ----

func readTestFile(t *testing.T, relPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(relPath)
	if err != nil {
		t.Skipf("test file not found: %s", relPath)
	}
	return data
}

func expectToken(t *testing.T, tok token.Token, wantType token.Type, wantLit string) {
	t.Helper()
	if tok.Type != wantType {
		t.Errorf("token type: got %v, want %v (literal=%q)", tok.Type, wantType, tok.Literal)
	}
	if tok.Literal != wantLit {
		t.Errorf("token literal: got %q, want %q (type=%v)", tok.Literal, wantLit, tok.Type)
	}
}

func expectType(t *testing.T, tok token.Token, wantType token.Type) {
	t.Helper()
	if tok.Type != wantType {
		t.Errorf("token type: got %v, want %v (literal=%q)", tok.Type, wantType, tok.Literal)
	}
}

func assertNoIllegal(t *testing.T, toks []token.Token, file string) {
	t.Helper()
	for i, tok := range toks {
		if tok.Type == token.ILLEGAL {
			t.Errorf("%s: ILLEGAL token at index %d, pos %s: %q", file, i, tok.Pos, tok.Literal)
		}
	}
}

func assertHasSection(t *testing.T, toks []token.Token, name string) {
	t.Helper()
	for _, tok := range toks {
		if tok.Type == token.SECTION && tok.Literal == name {
			return
		}
	}
	t.Errorf("missing section %q in token stream", name)
}

func assertContainsKeyword(t *testing.T, toks []token.Token, kw token.Type) {
	t.Helper()
	for _, tok := range toks {
		if tok.Type == kw {
			return
		}
	}
	t.Errorf("missing keyword %v in token stream", kw)
}

func assertContainsLiteral(t *testing.T, toks []token.Token, ty token.Type, _ string) {
	t.Helper()
	for _, tok := range toks {
		if tok.Type == ty {
			return
		}
	}
	t.Errorf("missing literal of type %v in token stream", ty)
}

func assertEndsWithEOF(t *testing.T, toks []token.Token) {
	t.Helper()
	if len(toks) == 0 || toks[len(toks)-1].Type != token.EOF {
		t.Error("token stream does not end with EOF")
	}
}

// Prevent unused import warning for strings.
var _ = strings.NewReader

// Copyright © 2026 Paxlabs Inc. All rights reserved.
