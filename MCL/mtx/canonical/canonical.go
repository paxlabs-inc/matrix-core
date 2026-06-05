// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package canonical computes the deterministic AST hash for a MatrixScript file.
//
// Per spec §10, the canonical hash:
//   - Includes all section entries in declaration order
//   - Includes string literal content (interpolation preserved as literals)
//   - Includes block structure and condition expressions
//   - Includes type annotations
//   - EXCLUDES comments, blank lines, whitespace within values, and §HASH section
//
// The hash is sha256 over the canonical byte representation of the AST.
// This is the mtx_digest that flows into the D11 compiler seed.
package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"matrix/mcl/mtx/ast"
)

// Hash computes the sha256 AST hash of a MatrixScript file.
// Returns the hex-encoded digest string.
func Hash(file *ast.File) string {
	var buf []byte
	buf = appendFile(buf, file)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// Bytes returns the canonical byte representation (before hashing).
// Useful for debugging.
func Bytes(file *ast.File) []byte {
	var buf []byte
	return appendFile(buf, file)
}

// ---- canonical encoding ----

func appendFile(buf []byte, file *ast.File) []byte {
	for _, sec := range file.Sections {
		// Skip §HASH section (spec §10.1)
		if sec.Name == "HASH" {
			continue
		}
		buf = appendSection(buf, sec)
	}
	return buf
}

func appendSection(buf []byte, sec *ast.Section) []byte {
	buf = append(buf, "§"...)
	buf = append(buf, sec.Name...)
	buf = append(buf, '\n')

	for _, entry := range sec.Entries {
		buf = appendEntry(buf, entry)
	}

	return buf
}

func appendEntry(buf []byte, entry ast.Entry) []byte {
	switch e := entry.(type) {
	case *ast.Comment:
		// Comments excluded from hash (spec §10.1)
		return buf

	case *ast.KVPair:
		buf = append(buf, strings.Join(e.Key, ".")...)
		buf = append(buf, '=')
		buf = appendValue(buf, e.Value)
		buf = append(buf, '\n')

	case *ast.SlotDecl:
		buf = append(buf, "slot "...)
		buf = append(buf, e.Name...)
		buf = append(buf, ':')
		buf = appendTypeRef(buf, &e.TypeRef)
		buf = append(buf, '\n')
		for _, mod := range e.Modifiers {
			buf = appendSlotModifier(buf, mod)
		}

	case *ast.OnBlock:
		buf = append(buf, "on "...)
		buf = appendCondition(buf, e.Condition)
		buf = append(buf, '\n')
		for _, sub := range e.Entries {
			buf = appendEntry(buf, sub)
		}
		buf = append(buf, "end\n"...)

	case *ast.PromptBlock:
		buf = append(buf, "prompt\n"...)
		for _, role := range e.Roles {
			buf = append(buf, role.Role...)
			buf = append(buf, '=')
			buf = appendQuotedString(buf, role.Text)
			buf = append(buf, '\n')
		}
		buf = append(buf, "end\n"...)

	case *ast.ResolveStmt:
		buf = append(buf, "resolve slot."...)
		buf = append(buf, e.SlotName...)
		buf = append(buf, " <- "...)
		buf = append(buf, e.CortexFn...)
		buf = append(buf, '(')
		for i, arg := range e.Args {
			if i > 0 {
				buf = append(buf, ',')
			}
			if arg.Name != "" {
				buf = append(buf, arg.Name...)
				buf = append(buf, '=')
			}
			buf = appendValue(buf, arg.Value)
		}
		buf = append(buf, ')', '\n')

	case *ast.UnknownBlock:
		buf = append(buf, "unknown slot."...)
		buf = append(buf, e.SlotName...)
		buf = append(buf, '\n')
		for _, mod := range e.Modifiers {
			buf = append(buf, mod.Key...)
			buf = append(buf, '=')
			buf = appendValue(buf, mod.Value)
			buf = append(buf, '\n')
		}
		buf = append(buf, "end\n"...)

	case *ast.ClarifyBlock:
		buf = append(buf, "clarify slot."...)
		buf = append(buf, e.SlotName...)
		buf = append(buf, '\n')
		for _, mod := range e.Modifiers {
			buf = append(buf, mod.Key...)
			buf = append(buf, '=')
			buf = appendValue(buf, mod.Value)
			buf = append(buf, '\n')
		}
		buf = append(buf, "end\n"...)

	case *ast.FailEntry:
		buf = append(buf, e.Name...)
		buf = append(buf, '\n')
		for _, mod := range e.Modifiers {
			buf = append(buf, mod.Key...)
			buf = append(buf, '=')
			buf = append(buf, mod.Value...)
			buf = append(buf, '\n')
		}

	case *ast.RefEntry:
		buf = append(buf, e.URI...)
		buf = append(buf, '\n')

	case *ast.NoneEntry:
		buf = append(buf, "none\n"...)
	}

	return buf
}

func appendCondition(buf []byte, cond ast.Condition) []byte {
	switch c := cond.(type) {
	case *ast.VerbCondition:
		buf = append(buf, "verb="...)
		buf = append(buf, c.Verb...)
	case *ast.ConfidenceCondition:
		buf = append(buf, "confidence"...)
		buf = append(buf, c.Op...)
		buf = append(buf, c.Threshold...)
	case *ast.SlotValCondition:
		buf = append(buf, "slot."...)
		buf = append(buf, c.SlotName...)
		buf = append(buf, '=')
		buf = appendValue(buf, c.Value)
	case *ast.UnknownCondition:
		buf = append(buf, "unknown"...)
	}
	return buf
}

func appendValue(buf []byte, val ast.Value) []byte {
	if val == nil {
		return buf
	}
	switch v := val.(type) {
	case *ast.StringValue:
		buf = appendQuotedString(buf, v.Text)
	case *ast.IntValue:
		buf = append(buf, v.Raw...)
	case *ast.FloatValue:
		buf = append(buf, v.Raw...)
	case *ast.BoolValue:
		if v.Val {
			buf = append(buf, "true"...)
		} else {
			buf = append(buf, "false"...)
		}
	case *ast.IdentValue:
		buf = append(buf, v.Name...)
	case *ast.URIValue:
		buf = append(buf, v.URI...)
	case *ast.SpaceListValue:
		for i, item := range v.Items {
			if i > 0 {
				buf = append(buf, ' ')
			}
			buf = append(buf, item...)
		}
	case *ast.SlotExprValue:
		buf = append(buf, strings.Join(v.Parts, ".")...)
	case *ast.OptionListValue:
		buf = append(buf, '[')
		for i, item := range v.Items {
			if i > 0 {
				buf = append(buf, ' ')
			}
			buf = appendValue(buf, item)
		}
		buf = append(buf, ']')
	}
	return buf
}

func appendTypeRef(buf []byte, tr *ast.TypeRef) []byte {
	if tr.Name == "enum" && len(tr.EnumSet) > 0 {
		buf = append(buf, "enum<"...)
		for i, v := range tr.EnumSet {
			if i > 0 {
				buf = append(buf, '|')
			}
			buf = append(buf, v...)
		}
		buf = append(buf, '>')
	} else {
		buf = append(buf, tr.Name...)
	}
	if tr.IsList {
		buf = append(buf, "[]"...)
	}
	return buf
}

func appendSlotModifier(buf []byte, mod *ast.SlotModifier) []byte {
	switch mod.Kind {
	case ast.ModRequired:
		buf = append(buf, "required\n"...)
	case ast.ModOptional:
		buf = append(buf, "optional\n"...)
	case ast.ModDefault:
		buf = append(buf, "default="...)
		buf = appendValue(buf, mod.Value)
		buf = append(buf, '\n')
	case ast.ModHint:
		buf = append(buf, "hint="...)
		buf = appendValue(buf, mod.Value)
		buf = append(buf, '\n')
	case ast.ModMax:
		buf = append(buf, "max="...)
		buf = appendValue(buf, mod.Value)
		buf = append(buf, '\n')
	}
	return buf
}

func appendQuotedString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	for _, r := range s {
		switch r {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			buf = append(buf, fmt.Sprintf("%c", r)...)
		}
	}
	buf = append(buf, '"')
	return buf
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
