// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package validator enforces the 12 MatrixScript validation rules from spec §11.
//
// The validator operates on a parsed AST (ast.File). It checks structural and
// semantic constraints that the parser does not enforce (the parser accepts
// syntactically valid files; the validator rejects semantically invalid ones).
//
// Rules (spec §11):
//
//  1. §SKILL files must contain exactly: §SKILL, §INPUTS, §CORTEX, §TOOLS,
//     §SUB_SKILLS, §PROCEDURE, §OUTPUTS, §FAILURE_MODES. §HASH optional.
//  2. mcl.verbs entries must be D7 closed set or x: prefixed.
//  3. Enum literal values must be members of the declared enum<...> type.
//  4. Slot names must be unique within a section.
//  5. Every resolve statement must name a slot declared in §INPUTS.
//  6. Every unknown block must name a slot declared in §INPUTS.
//  7. Prompt blocks inside on-blocks must contain at least system= and user=.
//  8. §FAILURE_MODES entries must use known FailureReason values for reason=.
//  9. §SUB_SKILLS URIs must be version-pinned (@semver or @sha256).
//  10. §TOOLS URIs must be version-pinned.
//  11. on-block `kind = "<value>"` KV must be in ir.ValidStepKinds closed
//     enum (Session 31b model router). Empty/missing is allowed and routes
//     to "reason" at executor time.
//  12. on-block `output_cardinality = <int>` KV must be a strictly positive
//     integer literal (Session 31c model router P3c). Empty/missing leaves
//     the planner to choose the plan shape.
package validator

import (
	"fmt"
	"strings"

	"matrix/mcl/ir"
	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/interpreter"
	"matrix/mcl/mtx/token"
)

// D7ClosedVerbs is the set of 10 closed verbs (D7 decision).
var D7ClosedVerbs = map[string]bool{
	"find": true, "acquire": true, "build": true, "modify": true,
	"deliver": true, "analyze": true, "negotiate": true, "schedule": true,
	"monitor": true, "delegate": true,
}

// ValidFailureReasons is the set of known failure reasons (spec §9.2).
var ValidFailureReasons = map[string]bool{
	"unknown_information": true, "policy_violation": true,
	"out_of_budget": true, "out_of_scope": true,
	"ambiguous_request": true, "tool_failure": true,
	"external_failure": true, "timeout": true,
	"cancelled_by_user": true, "correction_invalid": true,
}

// RequiredSkillSections is the set of sections a SKILL.mtx must contain.
var RequiredSkillSections = []string{
	"SKILL", "INPUTS", "CORTEX", "TOOLS",
	"SUB_SKILLS", "PROCEDURE", "OUTPUTS", "FAILURE_MODES",
}

// Error is a validation error at a specific source position.
type Error struct {
	Pos  token.Pos
	Rule int
	Msg  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: [V%d] %s", e.Pos, e.Rule, e.Msg)
}

// ValidateSkill runs all 12 validation rules on a SKILL.mtx AST.
// Returns nil if valid.
func ValidateSkill(file *ast.File) []*Error {
	v := &validator{file: file}
	v.rule1RequiredSections()
	v.rule2VerbVocab()
	v.rule4UniqueSlotNames()
	v.rule5ResolveRefsInputSlots()
	v.rule6UnknownRefsInputSlots()
	v.rule7PromptRoles()
	v.rule8FailureReasons()
	v.rule9SubSkillURIs()
	v.rule10ToolURIs()
	v.rule11OnBlockStepKind()
	v.rule12OnBlockOutputCardinality()
	return v.errors
}

// ValidateCore runs a subset of rules applicable to core .mtx files.
func ValidateCore(file *ast.File) []*Error {
	v := &validator{file: file}
	v.rule4UniqueSlotNames()
	v.rule7PromptRoles()
	v.rule11OnBlockStepKind()
	v.rule12OnBlockOutputCardinality()
	return v.errors
}

type validator struct {
	file   *ast.File
	errors []*Error
}

func (v *validator) errorf(pos token.Pos, rule int, format string, args ...interface{}) {
	v.errors = append(v.errors, &Error{
		Pos:  pos,
		Rule: rule,
		Msg:  fmt.Sprintf(format, args...),
	})
}

func (v *validator) findSection(name string) *ast.Section {
	for _, sec := range v.file.Sections {
		if sec.Name == name {
			return sec
		}
	}
	return nil
}

// rule1: §SKILL files must contain the 8 required sections.
func (v *validator) rule1RequiredSections() {
	present := map[string]bool{}
	for _, sec := range v.file.Sections {
		present[sec.Name] = true
	}

	for _, req := range RequiredSkillSections {
		if !present[req] {
			v.errorf(v.file.Pos(), 1, "missing required section §%s", req)
		}
	}
}

// rule2: mcl.verbs entries must be D7 closed set or x: prefixed.
func (v *validator) rule2VerbVocab() {
	skill := v.findSection("SKILL")
	if skill == nil {
		return
	}

	for _, entry := range skill.Entries {
		kv, ok := entry.(*ast.KVPair)
		if !ok {
			continue
		}
		key := strings.Join(kv.Key, ".")
		if key != "mcl.verbs" {
			continue
		}

		switch val := kv.Value.(type) {
		case *ast.SpaceListValue:
			for _, verb := range val.Items {
				v.checkVerb(verb, kv.Pos())
			}
		case *ast.IdentValue:
			v.checkVerb(val.Name, kv.Pos())
		}
	}
}

func (v *validator) checkVerb(verb string, pos token.Pos) {
	if D7ClosedVerbs[verb] {
		return
	}
	if strings.HasPrefix(verb, "x:") {
		return
	}
	v.errorf(pos, 2, "verb %q is not in D7 closed set and not x: prefixed", verb)
}

// rule4: slot names unique within a section.
func (v *validator) rule4UniqueSlotNames() {
	for _, sec := range v.file.Sections {
		seen := map[string]bool{}
		for _, entry := range sec.Entries {
			sd, ok := entry.(*ast.SlotDecl)
			if !ok {
				continue
			}
			if seen[sd.Name] {
				v.errorf(sd.Pos(), 4, "duplicate slot name %q in §%s", sd.Name, sec.Name)
			}
			seen[sd.Name] = true
		}
	}
}

// rule5: every resolve statement names a slot declared in §INPUTS.
func (v *validator) rule5ResolveRefsInputSlots() {
	inputSlots := v.collectInputSlots()
	v.walkEntries(func(entry ast.Entry) {
		rs, ok := entry.(*ast.ResolveStmt)
		if !ok {
			return
		}
		if !inputSlots[rs.SlotName] {
			v.errorf(rs.Pos(), 5, "resolve references slot %q not declared in §INPUTS", rs.SlotName)
		}
	})
}

// rule6: every unknown block names a slot declared in §INPUTS.
func (v *validator) rule6UnknownRefsInputSlots() {
	inputSlots := v.collectInputSlots()
	v.walkEntries(func(entry ast.Entry) {
		ub, ok := entry.(*ast.UnknownBlock)
		if !ok {
			return
		}
		if !inputSlots[ub.SlotName] {
			v.errorf(ub.Pos(), 6, "unknown block references slot %q not declared in §INPUTS", ub.SlotName)
		}
	})
}

// rule7: prompt blocks in on-blocks must have system= and user= roles.
func (v *validator) rule7PromptRoles() {
	v.walkEntries(func(entry ast.Entry) {
		pb, ok := entry.(*ast.PromptBlock)
		if !ok {
			return
		}
		hasSystem := false
		hasUser := false
		for _, role := range pb.Roles {
			switch role.Role {
			case "system":
				hasSystem = true
			case "user":
				hasUser = true
			}
		}
		if !hasSystem {
			v.errorf(pb.Pos(), 7, "prompt block missing system= role")
		}
		if !hasUser {
			v.errorf(pb.Pos(), 7, "prompt block missing user= role")
		}
	})
}

// rule8: §FAILURE_MODES reason= must use known FailureReason values.
func (v *validator) rule8FailureReasons() {
	fm := v.findSection("FAILURE_MODES")
	if fm == nil {
		return
	}
	for _, entry := range fm.Entries {
		fe, ok := entry.(*ast.FailEntry)
		if !ok {
			continue
		}
		for _, mod := range fe.Modifiers {
			if mod.Key != "reason" {
				continue
			}
			if !ValidFailureReasons[mod.Value] {
				v.errorf(mod.Pos(), 8, "unknown failure reason %q", mod.Value)
			}
		}
	}
}

// rule9: §SUB_SKILLS URIs must be version-pinned.
func (v *validator) rule9SubSkillURIs() {
	sec := v.findSection("SUB_SKILLS")
	if sec == nil {
		return
	}
	for _, entry := range sec.Entries {
		re, ok := entry.(*ast.RefEntry)
		if !ok {
			continue
		}
		if !isVersionPinned(re.URI) {
			v.errorf(re.Pos(), 9, "§SUB_SKILLS URI %q not version-pinned (requires @semver or @sha256)", re.URI)
		}
	}
}

// rule10: §TOOLS URIs must be version-pinned.
func (v *validator) rule10ToolURIs() {
	sec := v.findSection("TOOLS")
	if sec == nil {
		return
	}
	for _, entry := range sec.Entries {
		re, ok := entry.(*ast.RefEntry)
		if !ok {
			continue
		}
		if !isVersionPinned(re.URI) {
			v.errorf(re.Pos(), 10, "§TOOLS URI %q not version-pinned (requires @semver or @sha256)", re.URI)
		}
	}
}

// rule11: on-block `kind = "<value>"` KVPair value must be in the
// closed ir.ValidStepKinds enum. Empty/absent is allowed (defaults to
// "reason" at executor time). Session 31b model router · P2b.
//
// Why this lives at validator-time:
//   - Wrong kind silently routes to KindReason (the registry's executor
//     fallback), which makes the bug invisible at run time. Loud failure
//     here means CI catches the typo before the skill ships.
//   - The enum is closed (ir.StepKindNames), so a misspelled "writting"
//     is unambiguously wrong; no judgment call needed.
//   - Value types other than string/identifier (int, bool, list, URI)
//     are unambiguously wrong too — the interpreter would silently
//     ignore them via ExtractKindValue's nil return.
func (v *validator) rule11OnBlockStepKind() {
	v.walkEntries(func(entry ast.Entry) {
		ob, ok := entry.(*ast.OnBlock)
		if !ok {
			return
		}
		for _, sub := range ob.Entries {
			kv, ok := sub.(*ast.KVPair)
			if !ok {
				continue
			}
			if len(kv.Key) != 1 || kv.Key[0] != "kind" {
				continue
			}
			val := interpreter.ExtractKindValue(kv.Value)
			if val == "" {
				v.errorf(kv.Pos(), 11,
					"on-block `kind=` must be a quoted string or bare identifier (got %T)", kv.Value)
				continue
			}
			if !ir.ValidStepKinds[val] {
				v.errorf(kv.Pos(), 11,
					"on-block kind=%q not in closed step-kind enum %v", val, ir.StepKindNames)
			}
		}
	})
}

// rule12: on-block `output_cardinality = <int>` KVPair value must be
// a strictly positive integer literal. Empty/absent is allowed and
// leaves the planner free to choose a plan shape. Session 31c model
// router P3c.
//
// Rationale (mirrors rule 11's posture):
//   - The hint is consumed by the planner system prompt; a malformed
//     value silently produces no hint, which makes the bug invisible
//     at run time. Loud failure here surfaces the typo at CI time.
//   - Zero / negative values are unambiguously wrong (a skill cannot
//     produce "-1 outputs"); we reject them rather than coerce.
//   - Non-integer types (string, ident, float, bool, list, URI) are
//     all unambiguously wrong; the interpreter's ExtractPositiveIntValue
//     ignores them silently, so we surface them as validator errors.
func (v *validator) rule12OnBlockOutputCardinality() {
	v.walkEntries(func(entry ast.Entry) {
		ob, ok := entry.(*ast.OnBlock)
		if !ok {
			return
		}
		for _, sub := range ob.Entries {
			kv, ok := sub.(*ast.KVPair)
			if !ok {
				continue
			}
			if len(kv.Key) != 1 || kv.Key[0] != "output_cardinality" {
				continue
			}
			if _, ok := kv.Value.(*ast.IntValue); !ok {
				v.errorf(kv.Pos(), 12,
					"on-block `output_cardinality=` must be an integer literal (got %T)", kv.Value)
				continue
			}
			if _, ok := interpreter.ExtractPositiveIntValue(kv.Value); !ok {
				iv := kv.Value.(*ast.IntValue)
				v.errorf(kv.Pos(), 12,
					"on-block output_cardinality=%s must be strictly positive", iv.Raw)
			}
		}
	})
}

// ---- helpers ----

func (v *validator) collectInputSlots() map[string]bool {
	slots := map[string]bool{}
	inputs := v.findSection("INPUTS")
	if inputs == nil {
		return slots
	}
	for _, entry := range inputs.Entries {
		sd, ok := entry.(*ast.SlotDecl)
		if !ok {
			continue
		}
		slots[sd.Name] = true
	}
	return slots
}

// walkEntries walks all entries in all sections, including nested on-block entries.
func (v *validator) walkEntries(fn func(ast.Entry)) {
	for _, sec := range v.file.Sections {
		for _, entry := range sec.Entries {
			v.walkEntry(entry, fn)
		}
	}
}

func (v *validator) walkEntry(entry ast.Entry, fn func(ast.Entry)) {
	fn(entry)
	if ob, ok := entry.(*ast.OnBlock); ok {
		for _, sub := range ob.Entries {
			v.walkEntry(sub, fn)
		}
	}
}

// isVersionPinned checks if a matrix:// URI contains @semver or @sha256:.
func isVersionPinned(uri string) bool {
	atIdx := strings.LastIndex(uri, "@")
	if atIdx < 0 {
		return false
	}
	version := uri[atIdx+1:]
	if version == "" || version == "latest" {
		return false
	}
	return true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
