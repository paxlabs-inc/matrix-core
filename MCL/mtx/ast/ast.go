// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package ast defines the typed AST nodes for MatrixScript (.mtx) files.
//
// The tree structure mirrors the grammar.bnf productions:
//
//	File → []Section
//	Section → SectionHeader + []Entry
//	Entry → KVPair | SlotDecl | OnBlock | PromptBlock | ResolveStmt |
//	        UnknownBlock | ClarifyBlock | FailEntry | RefEntry | NoneEntry | Comment
//
// All nodes carry a Pos for error reporting.
package ast

import "matrix/mcl/mtx/token"

// Node is the interface satisfied by every AST node.
type Node interface {
	Pos() token.Pos
}

// File is the root AST node — a sequence of sections.
type File struct {
	Sections []*Section
	Comments []*Comment // top-level comments (before first section)
}

func (f *File) Pos() token.Pos {
	if len(f.Sections) > 0 {
		return f.Sections[0].Pos()
	}
	return token.Pos{Line: 1, Col: 1}
}

// Section represents §NAME followed by its entries.
type Section struct {
	Name    string    // e.g. "SKILL", "INPUTS", "PROCEDURE"
	NamePos token.Pos // position of the § character
	Entries []Entry
}

func (s *Section) Pos() token.Pos { return s.NamePos }

// Entry is the interface for all section-level entries.
type Entry interface {
	Node
	entryNode()
}

// ---- concrete entry types ----

// Comment preserves a # comment for display/diff purposes.
// Comments are STRIPPED before canonical hashing (D11).
type Comment struct {
	Text    string // comment text excluding the leading #
	TextPos token.Pos
}

func (c *Comment) Pos() token.Pos { return c.TextPos }
func (c *Comment) entryNode()     {}

// KVPair is key=value on a single line.
// Key may be dotted: "stage.1.id".
type KVPair struct {
	Key    []string // split on dots: ["stage", "1", "id"]
	Value  Value
	KeyPos token.Pos
}

func (kv *KVPair) Pos() token.Pos { return kv.KeyPos }
func (kv *KVPair) entryNode()     {}

// SlotDecl is `slot name: Type` + modifiers.
type SlotDecl struct {
	Name      string
	TypeRef   TypeRef
	Modifiers []*SlotModifier
	SlotPos   token.Pos
}

func (sd *SlotDecl) Pos() token.Pos { return sd.SlotPos }
func (sd *SlotDecl) entryNode()     {}

// SlotModifier is an indented modifier line under a slot declaration.
type SlotModifier struct {
	Kind   SlotModKind // required, optional, default, hint, max
	Value  Value       // nil for required/optional
	ModPos token.Pos
}

func (sm *SlotModifier) Pos() token.Pos { return sm.ModPos }

// SlotModKind is the kind of slot modifier.
type SlotModKind int

const (
	ModRequired SlotModKind = iota
	ModOptional
	ModDefault
	ModHint
	ModMax
)

// OnBlock is `on <condition> ... end`.
type OnBlock struct {
	Condition Condition
	Entries   []Entry // prompt, resolve, unknown, clarify, kv_pair, nested on
	OnPos     token.Pos
}

func (ob *OnBlock) Pos() token.Pos { return ob.OnPos }
func (ob *OnBlock) entryNode()     {}

// Condition is the interface for on-block conditions.
type Condition interface {
	Node
	conditionNode()
}

// VerbCondition is `verb=<name>`.
type VerbCondition struct {
	Verb    string
	VerbPos token.Pos
}

func (vc *VerbCondition) Pos() token.Pos { return vc.VerbPos }
func (vc *VerbCondition) conditionNode() {}

// ConfidenceCondition is `confidence<0.75` or `confidence>=0.85`.
type ConfidenceCondition struct {
	Op        string // "<", "<=", ">", ">=", "=="
	Threshold string // the float literal
	CondPos   token.Pos
}

func (cc *ConfidenceCondition) Pos() token.Pos { return cc.CondPos }
func (cc *ConfidenceCondition) conditionNode() {}

// SlotValCondition is `slot.<name>=<value>`.
type SlotValCondition struct {
	SlotName string
	Value    Value
	CondPos  token.Pos
}

func (sc *SlotValCondition) Pos() token.Pos { return sc.CondPos }
func (sc *SlotValCondition) conditionNode() {}

// UnknownCondition is the `unknown` keyword condition.
type UnknownCondition struct {
	CondPos token.Pos
}

func (uc *UnknownCondition) Pos() token.Pos { return uc.CondPos }
func (uc *UnknownCondition) conditionNode() {}

// PromptBlock is `prompt ... end` with role entries.
type PromptBlock struct {
	Roles     []*PromptRole
	PromptPos token.Pos
}

func (pb *PromptBlock) Pos() token.Pos { return pb.PromptPos }
func (pb *PromptBlock) entryNode()     {}

// PromptRole is a single `role="text"` line inside a prompt block.
type PromptRole struct {
	Role    string // "system", "user", "assistant"
	Text    string // the string literal content
	RolePos token.Pos
}

func (pr *PromptRole) Pos() token.Pos { return pr.RolePos }

// ResolveStmt is `resolve slot.<name> <- cortex.fn(args...)`.
type ResolveStmt struct {
	SlotName   string
	CortexFn   string // "cortex.find", "cortex.resolve", "cortex.context"
	Args       []*CortexArg
	ResolvePos token.Pos
}

func (rs *ResolveStmt) Pos() token.Pos { return rs.ResolvePos }
func (rs *ResolveStmt) entryNode()     {}

// CortexArg is a named argument in a cortex function call: `key=value`.
type CortexArg struct {
	Name   string
	Value  Value
	ArgPos token.Pos
}

func (ca *CortexArg) Pos() token.Pos { return ca.ArgPos }

// UnknownBlock is `unknown slot.<name> ... end`.
type UnknownBlock struct {
	SlotName   string
	Modifiers  []*UnknownModifier
	UnknownPos token.Pos
}

func (ub *UnknownBlock) Pos() token.Pos { return ub.UnknownPos }
func (ub *UnknownBlock) entryNode()     {}

// UnknownModifier is an indented modifier inside an unknown block.
type UnknownModifier struct {
	Key    string // "severity", "reason", "default", "options"
	Value  Value
	ModPos token.Pos
}

func (um *UnknownModifier) Pos() token.Pos { return um.ModPos }

// ClarifyBlock is `clarify slot.<name> ... end`.
type ClarifyBlock struct {
	SlotName   string
	Modifiers  []*ClarifyModifier
	ClarifyPos token.Pos
}

func (cb *ClarifyBlock) Pos() token.Pos { return cb.ClarifyPos }
func (cb *ClarifyBlock) entryNode()     {}

// ClarifyModifier is an indented modifier inside a clarify block.
type ClarifyModifier struct {
	Key    string // "prompt", "type", "required", "options", "default"
	Value  Value
	ModPos token.Pos
}

func (cm *ClarifyModifier) Pos() token.Pos { return cm.ModPos }

// FailEntry is a failure mode entry: `name` + indented modifiers.
type FailEntry struct {
	Name      string
	Modifiers []*FailModifier
	NamePos   token.Pos
}

func (fe *FailEntry) Pos() token.Pos { return fe.NamePos }
func (fe *FailEntry) entryNode()     {}

// FailModifier is an indented modifier under a failure entry.
type FailModifier struct {
	Key    string // "action", "suggest", "reason"
	Value  string
	ModPos token.Pos
}

func (fm *FailModifier) Pos() token.Pos { return fm.ModPos }

// RefEntry is a bare URI reference line (used in §TOOLS, §SUB_SKILLS).
type RefEntry struct {
	URI    string
	URIPos token.Pos
}

func (re *RefEntry) Pos() token.Pos { return re.URIPos }
func (re *RefEntry) entryNode()     {}

// NoneEntry is the `none` keyword line.
type NoneEntry struct {
	NonePos token.Pos
}

func (ne *NoneEntry) Pos() token.Pos { return ne.NonePos }
func (ne *NoneEntry) entryNode()     {}

// ---- value types ----

// Value is the interface for all value expressions in the AST.
type Value interface {
	Node
	valueNode()
}

// StringValue is a quoted string literal.
type StringValue struct {
	Text    string // decoded content (escapes processed, interpolation preserved as raw text)
	TextPos token.Pos
}

func (sv *StringValue) Pos() token.Pos { return sv.TextPos }
func (sv *StringValue) valueNode()     {}

// IntValue is an integer literal.
type IntValue struct {
	Raw    string // raw text e.g. "42"
	IntPos token.Pos
}

func (iv *IntValue) Pos() token.Pos { return iv.IntPos }
func (iv *IntValue) valueNode()     {}

// FloatValue is a floating-point literal.
type FloatValue struct {
	Raw      string // raw text e.g. "0.75"
	FloatPos token.Pos
}

func (fv *FloatValue) Pos() token.Pos { return fv.FloatPos }
func (fv *FloatValue) valueNode()     {}

// BoolValue is true or false.
type BoolValue struct {
	Val     bool
	BoolPos token.Pos
}

func (bv *BoolValue) Pos() token.Pos { return bv.BoolPos }
func (bv *BoolValue) valueNode()     {}

// IdentValue is a bare identifier used as a value.
type IdentValue struct {
	Name     string
	IdentPos token.Pos
}

func (iv *IdentValue) Pos() token.Pos { return iv.IdentPos }
func (iv *IdentValue) valueNode()     {}

// URIValue is a matrix:// URI literal.
type URIValue struct {
	URI    string
	URIPos token.Pos
}

func (uv *URIValue) Pos() token.Pos { return uv.URIPos }
func (uv *URIValue) valueNode()     {}

// SpaceListValue is a space-separated list of identifiers.
type SpaceListValue struct {
	Items   []string
	ListPos token.Pos
}

func (sl *SpaceListValue) Pos() token.Pos { return sl.ListPos }
func (sl *SpaceListValue) valueNode()     {}

// SlotExprValue is a slot reference like `slot.target.prose`.
type SlotExprValue struct {
	Parts   []string // ["slot", "target", "prose"]
	ExprPos token.Pos
}

func (se *SlotExprValue) Pos() token.Pos { return se.ExprPos }
func (se *SlotExprValue) valueNode()     {}

// OptionListValue is `[item1 item2 item3]`.
type OptionListValue struct {
	Items   []Value
	ListPos token.Pos
}

func (ol *OptionListValue) Pos() token.Pos { return ol.ListPos }
func (ol *OptionListValue) valueNode()     {}

// ---- type reference ----

// TypeRef represents a type annotation on a slot.
type TypeRef struct {
	Name    string   // base type name (e.g. "ArtifactRef", "string", "enum")
	IsList  bool     // suffixed with []
	EnumSet []string // for enum<a|b|c> — the variant names
	TypePos token.Pos
}

func (tr *TypeRef) Pos() token.Pos { return tr.TypePos }

// Copyright © 2026 Paxlabs Inc. All rights reserved.
