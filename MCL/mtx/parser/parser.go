// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package parser builds a typed AST from the MatrixScript token stream.
//
// The parser is top-down recursive-descent. It consumes tokens from the
// lexer and builds ast.File → []ast.Section → []ast.Entry trees.
//
// Errors are collected (not fatal) so the parser can report multiple
// issues in one pass.
package parser

import (
	"fmt"

	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/lexer"
	"matrix/mcl/mtx/token"
)

// Error is a parse error at a specific source position.
type Error struct {
	Pos token.Pos
	Msg string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

// Parser produces an AST from a lexer token stream.
type Parser struct {
	lex    *lexer.Lexer
	tok    token.Token
	errors []*Error
}

// New creates a parser over the given source bytes.
func New(src []byte) *Parser {
	l := lexer.New(src)
	p := &Parser{lex: l}
	p.advance() // load first token
	return p
}

// Parse parses the entire file and returns the AST plus any errors.
func (p *Parser) Parse() (*ast.File, []*Error) {
	file := &ast.File{}

	p.skipNewlines()

	for p.tok.Type != token.EOF {
		if p.tok.Type == token.SECTION {
			sec := p.parseSection()
			if sec != nil {
				file.Sections = append(file.Sections, sec)
			}
		} else {
			p.errorf("expected section header (§NAME), got %v %q", p.tok.Type, p.tok.Literal)
			p.advance()
		}
		p.skipNewlines()
	}

	return file, p.errors
}

// Errors returns all parse errors.
func (p *Parser) Errors() []*Error {
	return p.errors
}

// ---- section ----

func (p *Parser) parseSection() *ast.Section {
	sec := &ast.Section{
		NamePos: p.tok.Pos,
	}

	// §NAME — strip the § prefix
	name := p.tok.Literal
	if len(name) > 0 && name[0] == 0xC2 { // UTF-8 first byte of §
		name = name[2:] // skip 2-byte § encoding
	}
	sec.Name = name

	p.advance() // consume SECTION token
	p.expectNewline()
	p.skipNewlines()

	for p.tok.Type != token.EOF && p.tok.Type != token.SECTION {
		entry := p.parseSectionEntry()
		if entry != nil {
			sec.Entries = append(sec.Entries, entry)
		}
		p.skipNewlines()
	}

	return sec
}

// ---- section entries ----

func (p *Parser) parseSectionEntry() ast.Entry {
	switch p.tok.Type {
	case token.SECTION, token.EOF:
		return nil

	case token.KW_SLOT:
		return p.parseSlotOrKVPair()

	case token.KW_ON:
		return p.parseOnBlock()

	case token.KW_PROMPT:
		return p.parsePromptBlock()

	case token.KW_RESOLVE:
		return p.parseResolveStmt()

	case token.KW_UNKNOWN:
		return p.parseUnknownBlock()

	case token.KW_CLARIFY:
		return p.parseClarifyBlock()

	case token.KW_NONE:
		return p.parseNoneEntry()

	case token.URI:
		return p.parseRefEntry()

	case token.INDENT:
		// Indented line at section level — probably a modifier that belongs
		// to a preceding entry. Skip it with a warning.
		p.errorf("unexpected indented line at section level")
		p.skipToNewline()
		return nil

	default:
		// Try KV pair or fail entry (both start with an IDENT-like token)
		return p.parseKVPairOrFailEntry()
	}
}

// ---- slot declaration ----

// parseSlotOrKVPair disambiguates "slot name: Type" from "slot.weight.X=Y".
func (p *Parser) parseSlotOrKVPair() ast.Entry {
	pos := p.tok.Pos
	p.advance() // consume "slot"

	// If followed by DOT, this is a dotted KV key: slot.weight.X=Y
	if p.tok.Type == token.DOT {
		return p.parseKVPairContinuation("slot", pos)
	}

	// Otherwise: slot declaration
	return p.parseSlotDeclBody(pos)
}

func (p *Parser) parseSlotDecl() *ast.SlotDecl {
	pos := p.tok.Pos
	p.advance() // consume "slot"
	return p.parseSlotDeclBody(pos)
}

func (p *Parser) parseSlotDeclBody(pos token.Pos) *ast.SlotDecl {
	sd := &ast.SlotDecl{SlotPos: pos}

	// Slot name can be a keyword (e.g. "slot verb: enum<...>")
	if p.tok.Type != token.IDENT && !p.tok.Type.IsKeyword() {
		p.errorf("expected slot name, got %v", p.tok.Type)
		p.skipToNewline()
		return sd
	}
	sd.Name = p.tok.Literal
	p.advance()

	p.expect(token.COLON)

	sd.TypeRef = p.parseTypeRef()

	p.expectNewline()

	// Parse indented modifiers
	for p.tok.Type == token.INDENT {
		mod := p.parseSlotModifier()
		if mod != nil {
			sd.Modifiers = append(sd.Modifiers, mod)
		}
	}

	return sd
}

func (p *Parser) parseSlotModifier() *ast.SlotModifier {
	pos := p.tok.Pos
	p.advance() // consume INDENT

	mod := &ast.SlotModifier{ModPos: pos}

	switch p.tok.Type {
	case token.KW_REQUIRED:
		mod.Kind = ast.ModRequired
		p.advance()
	case token.KW_OPTIONAL:
		mod.Kind = ast.ModOptional
		p.advance()
	default:
		// default=, hint=, max=
		key := p.tok.Literal
		p.advance()
		p.expect(token.EQUALS)
		val := p.parseValue()
		switch key {
		case "default":
			mod.Kind = ast.ModDefault
		case "hint":
			mod.Kind = ast.ModHint
		case "max":
			mod.Kind = ast.ModMax
		default:
			mod.Kind = ast.ModDefault // fallback
		}
		mod.Value = val
	}

	p.expectNewline()
	return mod
}

// ---- type reference ----

func (p *Parser) parseTypeRef() ast.TypeRef {
	tr := ast.TypeRef{TypePos: p.tok.Pos}

	if p.tok.Type == token.KW_ENUM {
		// enum<a|b|c>
		tr.Name = "enum"
		p.advance() // consume "enum"
		p.expect(token.LT)
		for {
			if p.tok.Type == token.GT || p.tok.Type == token.EOF || p.tok.Type == token.NEWLINE {
				break
			}
			if p.tok.Type == token.PIPE {
				p.advance()
				continue
			}
			tr.EnumSet = append(tr.EnumSet, p.tok.Literal)
			p.advance()
		}
		p.expect(token.GT)
	} else {
		tr.Name = p.tok.Literal
		p.advance()
	}

	// Check for list suffix []
	if p.tok.Type == token.LBRACKET {
		p.advance()
		p.expect(token.RBRACKET)
		tr.IsList = true
	}

	return tr
}

// ---- on block ----

func (p *Parser) parseOnBlock() *ast.OnBlock {
	ob := &ast.OnBlock{OnPos: p.tok.Pos}
	p.advance() // consume "on"

	ob.Condition = p.parseCondition()
	p.expectNewline()

	// Parse on-block body entries until "end"
	p.skipNewlines()
	for p.tok.Type != token.KW_END && p.tok.Type != token.EOF && p.tok.Type != token.SECTION {
		entry := p.parseOnEntry()
		if entry != nil {
			ob.Entries = append(ob.Entries, entry)
		}
		p.skipNewlines()
	}

	if p.tok.Type == token.KW_END {
		p.advance()
		p.expectNewline()
	} else {
		p.errorf("expected 'end' to close on-block, got %v", p.tok.Type)
	}

	return ob
}

func (p *Parser) parseCondition() ast.Condition {
	switch p.tok.Type {
	case token.KW_VERB:
		pos := p.tok.Pos
		p.advance() // consume "verb"
		p.expect(token.EQUALS)
		verb := p.tok.Literal
		p.advance()
		return &ast.VerbCondition{Verb: verb, VerbPos: pos}

	case token.KW_CONFIDENCE:
		pos := p.tok.Pos
		p.advance() // consume "confidence"
		op := p.tok.Literal
		if p.tok.Type != token.LT && p.tok.Type != token.LTEQ &&
			p.tok.Type != token.GT && p.tok.Type != token.GTEQ &&
			p.tok.Type != token.EQEQ {
			p.errorf("expected comparator after 'confidence', got %v", p.tok.Type)
			return &ast.ConfidenceCondition{Op: "?", Threshold: "0", CondPos: pos}
		}
		p.advance() // consume comparator
		threshold := p.tok.Literal
		p.advance() // consume float
		return &ast.ConfidenceCondition{Op: op, Threshold: threshold, CondPos: pos}

	case token.KW_SLOT:
		pos := p.tok.Pos
		p.advance() // consume "slot"
		p.expect(token.DOT)
		slotName := p.tok.Literal
		p.advance()
		p.expect(token.EQUALS)
		val := p.parseValue()
		return &ast.SlotValCondition{SlotName: slotName, Value: val, CondPos: pos}

	case token.KW_UNKNOWN:
		pos := p.tok.Pos
		p.advance()
		return &ast.UnknownCondition{CondPos: pos}

	default:
		p.errorf("expected condition after 'on', got %v %q", p.tok.Type, p.tok.Literal)
		return &ast.UnknownCondition{CondPos: p.tok.Pos}
	}
}

func (p *Parser) parseOnEntry() ast.Entry {
	// Handle INDENT — on-entries inside on-blocks may or may not be indented.
	// Per grammar N2: on-block body entries start at column 0. But prompts and
	// nested modifiers use indent. We skip INDENT tokens that appear before
	// a known on-entry keyword.
	if p.tok.Type == token.INDENT {
		p.advance()
	}

	switch p.tok.Type {
	case token.KW_PROMPT:
		return p.parsePromptBlock()
	case token.KW_RESOLVE:
		return p.parseResolveStmt()
	case token.KW_UNKNOWN:
		return p.parseUnknownBlock()
	case token.KW_CLARIFY:
		return p.parseClarifyBlock()
	case token.KW_ON:
		return p.parseOnBlock()
	case token.KW_END:
		return nil
	default:
		// Try kv_pair
		return p.parseKVPairOrFailEntry()
	}
}

// ---- prompt block ----

func (p *Parser) parsePromptBlock() *ast.PromptBlock {
	pb := &ast.PromptBlock{PromptPos: p.tok.Pos}
	p.advance() // consume "prompt"
	p.expectNewline()

	// Parse role entries until "end"
	p.skipNewlines()
	for p.tok.Type != token.KW_END && p.tok.Type != token.EOF && p.tok.Type != token.SECTION {
		if p.tok.Type == token.INDENT {
			p.advance() // consume indent
		}

		// After consuming indent, re-check for end
		if p.tok.Type == token.KW_END {
			break
		}

		pos := p.tok.Pos
		var role string
		switch p.tok.Type {
		case token.KW_SYSTEM:
			role = "system"
		case token.KW_USER:
			role = "user"
		case token.KW_ASSISTANT:
			role = "assistant"
		default:
			p.errorf("expected role name (system/user/assistant) in prompt block, got %v %q", p.tok.Type, p.tok.Literal)
			p.skipToNewline()
			p.skipNewlines()
			continue
		}
		p.advance()
		p.expect(token.EQUALS)

		if p.tok.Type != token.STRING {
			p.errorf("expected string literal for role value, got %v", p.tok.Type)
			p.skipToNewline()
			p.skipNewlines()
			continue
		}
		text := p.tok.Literal
		p.advance()

		pb.Roles = append(pb.Roles, &ast.PromptRole{
			Role:    role,
			Text:    text,
			RolePos: pos,
		})
		p.expectNewline()
		p.skipNewlines()
	}

	if p.tok.Type == token.KW_END {
		p.advance()
		p.expectNewline()
	} else {
		p.errorf("expected 'end' to close prompt block")
	}

	return pb
}

// ---- resolve statement ----

func (p *Parser) parseResolveStmt() *ast.ResolveStmt {
	rs := &ast.ResolveStmt{ResolvePos: p.tok.Pos}
	p.advance() // consume "resolve"

	// slot.name
	p.expect(token.KW_SLOT)
	p.expect(token.DOT)
	rs.SlotName = p.tok.Literal
	p.advance()

	// <-
	p.expect(token.ARROW)

	// cortex.fn(args...)
	switch p.tok.Type {
	case token.KW_CORTEX_FIND:
		rs.CortexFn = "cortex.find"
	case token.KW_CORTEX_RESOLVE:
		rs.CortexFn = "cortex.resolve"
	case token.KW_CORTEX_CONTEXT:
		rs.CortexFn = "cortex.context"
	default:
		p.errorf("expected cortex function, got %v %q", p.tok.Type, p.tok.Literal)
		p.skipToNewline()
		return rs
	}
	p.advance()

	p.expect(token.LPAREN)

	// Parse arguments — may be named (key=value) or positional (slot.x.y)
	for p.tok.Type != token.RPAREN && p.tok.Type != token.EOF && p.tok.Type != token.NEWLINE {
		if p.tok.Type == token.COMMA {
			p.advance()
			continue
		}

		arg := &ast.CortexArg{ArgPos: p.tok.Pos}

		// Positional arg: slot expression like slot.target.prose
		if p.tok.Type == token.KW_SLOT {
			arg.Name = ""
			arg.Value = p.parseSlotExpr()
			rs.Args = append(rs.Args, arg)
			continue
		}

		// Named arg: key=value
		arg.Name = p.tok.Literal
		p.advance()
		if p.tok.Type == token.EQUALS {
			p.advance()
			arg.Value = p.parseValue()
		} else {
			// Positional bare value
			arg.Value = &ast.IdentValue{Name: arg.Name, IdentPos: arg.ArgPos}
			arg.Name = ""
		}
		rs.Args = append(rs.Args, arg)
	}

	p.expect(token.RPAREN)
	p.expectNewline()

	return rs
}

// ---- unknown block ----

func (p *Parser) parseUnknownBlock() *ast.UnknownBlock {
	ub := &ast.UnknownBlock{UnknownPos: p.tok.Pos}
	p.advance() // consume "unknown"

	// slot.name
	p.expect(token.KW_SLOT)
	p.expect(token.DOT)
	ub.SlotName = p.tok.Literal
	p.advance()
	p.expectNewline()

	// Parse modifiers until "end"
	p.skipNewlines()
	for p.tok.Type != token.KW_END && p.tok.Type != token.EOF && p.tok.Type != token.SECTION {
		if p.tok.Type == token.INDENT {
			modPos := p.tok.Pos
			p.advance()
			if p.tok.Type == token.KW_END {
				break
			}
			key := p.tok.Literal
			p.advance()
			p.expect(token.EQUALS)
			val := p.parseValue()
			ub.Modifiers = append(ub.Modifiers, &ast.UnknownModifier{
				Key:    key,
				Value:  val,
				ModPos: modPos,
			})
			p.expectNewline()
			p.skipNewlines()
		} else {
			break
		}
	}

	if p.tok.Type == token.KW_END {
		p.advance()
		p.expectNewline()
	} else {
		p.errorf("expected 'end' to close unknown block")
	}

	return ub
}

// ---- clarify block ----

func (p *Parser) parseClarifyBlock() *ast.ClarifyBlock {
	cb := &ast.ClarifyBlock{ClarifyPos: p.tok.Pos}
	p.advance() // consume "clarify"

	// slot.name
	p.expect(token.KW_SLOT)
	p.expect(token.DOT)
	cb.SlotName = p.tok.Literal
	p.advance()
	p.expectNewline()

	// Parse modifiers until "end"
	p.skipNewlines()
	for p.tok.Type != token.KW_END && p.tok.Type != token.EOF && p.tok.Type != token.SECTION {
		if p.tok.Type == token.INDENT {
			modPos := p.tok.Pos
			p.advance()
			if p.tok.Type == token.KW_END {
				break
			}
			key := p.tok.Literal
			p.advance()
			p.expect(token.EQUALS)
			val := p.parseValue()
			cb.Modifiers = append(cb.Modifiers, &ast.ClarifyModifier{
				Key:    key,
				Value:  val,
				ModPos: modPos,
			})
			p.expectNewline()
			p.skipNewlines()
		} else {
			break
		}
	}

	if p.tok.Type == token.KW_END {
		p.advance()
		p.expectNewline()
	} else {
		p.errorf("expected 'end' to close clarify block")
	}

	return cb
}

// ---- failure entry ----

func (p *Parser) parseFailEntry(name string, pos token.Pos) *ast.FailEntry {
	fe := &ast.FailEntry{
		Name:    name,
		NamePos: pos,
	}
	p.expectNewline()

	// Parse indented modifiers
	for p.tok.Type == token.INDENT {
		modPos := p.tok.Pos
		p.advance()
		key := p.tok.Literal
		p.advance()
		p.expect(token.EQUALS)
		val := p.tok.Literal
		p.advance()
		fe.Modifiers = append(fe.Modifiers, &ast.FailModifier{
			Key:    key,
			Value:  val,
			ModPos: modPos,
		})
		p.expectNewline()
	}

	return fe
}

// ---- kv pair or fail entry ----

func (p *Parser) parseKVPairOrFailEntry() ast.Entry {
	pos := p.tok.Pos
	name := p.tok.Literal
	p.advance()

	// Dotted key or = after the first ident?
	if p.tok.Type == token.DOT || p.tok.Type == token.EQUALS {
		return p.parseKVPairContinuation(name, pos)
	}

	// If followed by NEWLINE, this is a fail entry name
	if p.tok.Type == token.NEWLINE || p.tok.Type == token.EOF {
		return p.parseFailEntry(name, pos)
	}

	// Could be a fail entry with inline content — treat as fail entry
	return p.parseFailEntry(name, pos)
}

func (p *Parser) parseKVPairContinuation(firstName string, pos token.Pos) ast.Entry {
	keys := []string{firstName}

	// Collect dotted key parts
	for p.tok.Type == token.DOT {
		p.advance() // consume .
		part := p.tok.Literal
		keys = append(keys, part)
		p.advance()
	}

	// Named prompt block: dotted key ending in "prompt" followed by NEWLINE
	// Used by core modules like verb.mtx: classifier.prompt ... end
	if p.tok.Type == token.NEWLINE && len(keys) > 0 && keys[len(keys)-1] == "prompt" {
		pb := p.parsePromptBlockBody(pos)
		return pb
	}

	kv := &ast.KVPair{
		Key:    keys,
		KeyPos: pos,
	}

	if p.tok.Type == token.EQUALS {
		p.advance()
		kv.Value = p.parseValueList()
	} else {
		p.errorf("expected '=' after key, got %v", p.tok.Type)
	}
	p.expectNewline()

	return kv
}

// ---- value parsing ----

func (p *Parser) parseValue() ast.Value {
	pos := p.tok.Pos

	switch p.tok.Type {
	case token.STRING:
		text := p.tok.Literal
		p.advance()
		return &ast.StringValue{Text: text, TextPos: pos}

	case token.INT:
		raw := p.tok.Literal
		p.advance()
		// Handle semver: 1.0.0 or dotted numeric paths
		if p.tok.Type == token.DOT {
			return p.parseDottedValue(raw, pos)
		}
		return &ast.IntValue{Raw: raw, IntPos: pos}

	case token.FLOAT:
		raw := p.tok.Literal
		p.advance()
		// Handle dotted continuation: 1.0.0 where lexer sees FLOAT "1.0" then DOT "."
		if p.tok.Type == token.DOT {
			return p.parseDottedValue(raw, pos)
		}
		return &ast.FloatValue{Raw: raw, FloatPos: pos}

	case token.BOOL_TRUE:
		p.advance()
		return &ast.BoolValue{Val: true, BoolPos: pos}

	case token.BOOL_FALSE:
		p.advance()
		return &ast.BoolValue{Val: false, BoolPos: pos}

	case token.URI:
		uri := p.tok.Literal
		p.advance()
		return &ast.URIValue{URI: uri, URIPos: pos}

	case token.KW_SLOT:
		return p.parseSlotExpr()

	case token.LBRACKET:
		return p.parseOptionList()

	case token.NEWLINE, token.EOF:
		// Empty value (e.g. digest= with nothing after =)
		return &ast.StringValue{Text: "", TextPos: pos}

	default:
		// Keyword tokens or IDENT as a bare value
		if p.tok.Type == token.IDENT || p.tok.Type.IsKeyword() {
			name := p.tok.Literal
			p.advance()
			// Handle dotted identifiers: intent.draft.prose
			if p.tok.Type == token.DOT {
				return p.parseDottedValue(name, pos)
			}
			return &ast.IdentValue{Name: name, IdentPos: pos}
		}

		p.errorf("expected value, got %v %q", p.tok.Type, p.tok.Literal)
		p.advance()
		return &ast.IdentValue{Name: "", IdentPos: pos}
	}
}

// parseDottedValue handles compound dotted values like "1.0.0" or "intent.draft.prose".
// The first part has already been consumed; we're at the DOT.
func (p *Parser) parseDottedValue(first string, pos token.Pos) ast.Value {
	buf := first
	for p.tok.Type == token.DOT {
		buf += "."
		p.advance() // consume .
		if p.tok.Type == token.IDENT || p.tok.Type == token.INT ||
			p.tok.Type == token.FLOAT || p.tok.Type.IsKeyword() {
			buf += p.tok.Literal
			p.advance()
		}
	}
	return &ast.IdentValue{Name: buf, IdentPos: pos}
}

// parsePromptBlockBody parses the body of a prompt block (after the
// opening NEWLINE has been consumed by the caller). Used for both
// standalone `prompt ... end` and named `X.prompt ... end` blocks.
func (p *Parser) parsePromptBlockBody(pos token.Pos) *ast.PromptBlock {
	pb := &ast.PromptBlock{PromptPos: pos}
	p.advance() // consume NEWLINE

	p.skipNewlines()
	for p.tok.Type != token.KW_END && p.tok.Type != token.EOF && p.tok.Type != token.SECTION {
		if p.tok.Type == token.INDENT {
			p.advance()
		}
		if p.tok.Type == token.KW_END {
			break
		}

		rolePos := p.tok.Pos
		var role string
		switch p.tok.Type {
		case token.KW_SYSTEM:
			role = "system"
		case token.KW_USER:
			role = "user"
		case token.KW_ASSISTANT:
			role = "assistant"
		default:
			p.errorf("expected role name in prompt block, got %v %q", p.tok.Type, p.tok.Literal)
			p.skipToNewline()
			p.skipNewlines()
			continue
		}
		p.advance()
		p.expect(token.EQUALS)

		if p.tok.Type != token.STRING {
			p.errorf("expected string literal for role value, got %v", p.tok.Type)
			p.skipToNewline()
			p.skipNewlines()
			continue
		}
		text := p.tok.Literal
		p.advance()

		pb.Roles = append(pb.Roles, &ast.PromptRole{Role: role, Text: text, RolePos: rolePos})
		p.expectNewline()
		p.skipNewlines()
	}

	if p.tok.Type == token.KW_END {
		p.advance()
		p.expectNewline()
	} else {
		p.errorf("expected 'end' to close prompt block")
	}

	return pb
}

// parseValueList handles the value position in kv_pair, which may be a
// space-separated list of identifiers, or a compound value with slashes/at-signs.
func (p *Parser) parseValueList() ast.Value {
	first := p.parseValue()

	// If the next token is an ident/keyword on the SAME LINE (no NEWLINE between),
	// this is a space_list.
	if p.tok.Type == token.NEWLINE || p.tok.Type == token.EOF {
		return first
	}

	// Compound raw value: path/grammar refs like "core/verb.mtx" or "verb_vocab@1"
	// If we see SLASH, AT, or other inline punctuation, consume the rest as a compound string.
	if p.tok.Type == token.SLASH || p.tok.Type == token.AT {
		return p.consumeCompoundValue(first)
	}

	// Handle x:extension prefix as standalone value
	if p.tok.Type == token.COLON {
		if ident, ok := first.(*ast.IdentValue); ok {
			compound := ident.Name + ":"
			p.advance() // consume :
			if p.tok.Type == token.IDENT || p.tok.Type.IsKeyword() {
				compound += p.tok.Literal
				p.advance()
			}
			return &ast.IdentValue{Name: compound, IdentPos: first.Pos()}
		}
	}

	// Check if we should accumulate space-separated values
	if p.tok.Type == token.IDENT || p.tok.Type.IsKeyword() {
		items := []string{}
		if ident, ok := first.(*ast.IdentValue); ok {
			firstItem := ident.Name
			// Handle x:extension prefix for first item
			if p.tok.Type == token.COLON {
				firstItem += ":"
				p.advance()
				if p.tok.Type == token.IDENT || p.tok.Type.IsKeyword() {
					firstItem += p.tok.Literal
					p.advance()
				}
			}
			items = append(items, firstItem)
		} else {
			return first
		}

		for p.tok.Type == token.IDENT || p.tok.Type.IsKeyword() {
			item := p.tok.Literal
			p.advance()
			// Handle x:extension prefix (e.g. x:brainstorm)
			if p.tok.Type == token.COLON {
				item += ":"
				p.advance()
				if p.tok.Type == token.IDENT || p.tok.Type.IsKeyword() {
					item += p.tok.Literal
					p.advance()
				}
			}
			items = append(items, item)
		}

		return &ast.SpaceListValue{Items: items, ListPos: first.Pos()}
	}

	return first
}

// consumeCompoundValue consumes remaining inline tokens after the first value
// to form a compound value like "core/verb.mtx" or "verb_vocab@1".
func (p *Parser) consumeCompoundValue(first ast.Value) ast.Value {
	buf := valueToString(first)
	for p.tok.Type != token.NEWLINE && p.tok.Type != token.EOF &&
		p.tok.Type != token.RPAREN && p.tok.Type != token.RBRACKET &&
		p.tok.Type != token.COMMA {
		buf += p.tok.Literal
		p.advance()
	}
	return &ast.IdentValue{Name: buf, IdentPos: first.Pos()}
}

func valueToString(v ast.Value) string {
	switch val := v.(type) {
	case *ast.IdentValue:
		return val.Name
	case *ast.IntValue:
		return val.Raw
	case *ast.FloatValue:
		return val.Raw
	case *ast.StringValue:
		return val.Text
	default:
		return ""
	}
}

func (p *Parser) parseSlotExpr() ast.Value {
	pos := p.tok.Pos
	parts := []string{"slot"}
	p.advance() // consume "slot"
	for p.tok.Type == token.DOT {
		p.advance() // consume .
		parts = append(parts, p.tok.Literal)
		p.advance()
	}
	return &ast.SlotExprValue{Parts: parts, ExprPos: pos}
}

func (p *Parser) parseOptionList() ast.Value {
	pos := p.tok.Pos
	p.advance() // consume [
	ol := &ast.OptionListValue{ListPos: pos}
	for p.tok.Type != token.RBRACKET && p.tok.Type != token.EOF && p.tok.Type != token.NEWLINE {
		ol.Items = append(ol.Items, p.parseValue())
	}
	p.expect(token.RBRACKET)
	return ol
}

// ---- none / ref entries ----

func (p *Parser) parseNoneEntry() *ast.NoneEntry {
	ne := &ast.NoneEntry{NonePos: p.tok.Pos}
	p.advance() // consume "none"
	p.expectNewline()
	return ne
}

func (p *Parser) parseRefEntry() *ast.RefEntry {
	re := &ast.RefEntry{
		URI:    p.tok.Literal,
		URIPos: p.tok.Pos,
	}
	p.advance() // consume URI
	p.expectNewline()
	return re
}

// ---- helpers ----

func (p *Parser) advance() {
	p.tok = p.lex.NextToken()
}

func (p *Parser) expect(ty token.Type) {
	if p.tok.Type != ty {
		p.errorf("expected %v, got %v %q", ty, p.tok.Type, p.tok.Literal)
		return
	}
	p.advance()
}

func (p *Parser) expectNewline() {
	if p.tok.Type == token.NEWLINE {
		p.advance()
		return
	}
	if p.tok.Type == token.EOF {
		return // EOF is acceptable in place of trailing newline
	}
	// Don't error if we're already at EOF or section boundary.
	// Some entries may lack a trailing newline at EOF.
}

func (p *Parser) skipNewlines() {
	for p.tok.Type == token.NEWLINE {
		p.advance()
	}
}

func (p *Parser) skipToNewline() {
	for p.tok.Type != token.NEWLINE && p.tok.Type != token.EOF {
		p.advance()
	}
	if p.tok.Type == token.NEWLINE {
		p.advance()
	}
}

func (p *Parser) errorf(format string, args ...interface{}) {
	p.errors = append(p.errors, &Error{
		Pos: p.tok.Pos,
		Msg: fmt.Sprintf(format, args...),
	})
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
