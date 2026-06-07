// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

// coerceArg converts a string-typed PlanTree arg into its likely
// JSON-friendly type. ir.ToolCallPayload.Args is map[string]string for
// canonical hashing simplicity (MCL/ir/plan.go:141), but MCP servers
// expect ints/bools/objects/arrays per their JSON-Schema inputs.
// Best-effort coercion:
//
//	"true"/"false"        → bool
//	integer string        → int64
//	float string          → float64
//	JSON object "{...}"   → map[string]interface{}
//	JSON array  "[...]"   → []interface{}
//	everything else       → string (verbatim)
//
// The JSON object/array case is load-bearing for tools whose schema
// declares a nested type the plan IR cannot carry directly: the
// plan_tree@1 grammar forces every arg VALUE to a string, so a tool
// like tachyon_compile (whose `sources` is a map[path]->content) only
// ever receives its value as a JSON-encoded string. Parsing it back
// here hands the MCP server the real object instead of a string it
// cannot unmarshal (the prior behaviour broke compile/test/deploy/call
// with a "cannot unmarshal string" error). Same for constructor_args /
// args arrays and inline abi.
//
// Originally a verbatim port of cmd/mcl-e2e/walk.go:249-313. The harness
// validated this surface against real Fireworks-produced plans + npx/uvx
// MCP servers in sess#22b (75/75 assertions green).
func coerceArg(v string) interface{} {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	if v == "" {
		return v
	}
	// Structured JSON (object/array). Only attempt a parse when the
	// trimmed value is unambiguously a JSON container so plain strings
	// that merely contain braces are never mangled; on any parse error
	// we fall through and keep the verbatim string.
	if s := strings.TrimSpace(v); len(s) > 0 && (s[0] == '{' || s[0] == '[') {
		var j interface{}
		if err := json.Unmarshal([]byte(s), &j); err == nil {
			return j
		}
	}
	if isAllDigitsOptSign(v) {
		var n int64
		_, err := fmt.Sscanf(v, "%d", &n)
		if err == nil {
			return n
		}
	}
	if hasFloatShape(v) {
		var f float64
		_, err := fmt.Sscanf(v, "%g", &f)
		if err == nil {
			return f
		}
	}
	return v
}

func isAllDigitsOptSign(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i = 1
		if len(s) == 1 {
			return false
		}
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func hasFloatShape(s string) bool {
	hasDot := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			if hasDot {
				return false
			}
			hasDot = true
		case r == '-' || r == '+' || r == 'e' || r == 'E':
		default:
			return false
		}
	}
	return hasDot
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
