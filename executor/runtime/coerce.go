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
	// candidate is unambiguously a JSON container so plain strings that
	// merely contain braces are never mangled; on any parse error we fall
	// through and keep the verbatim string. A codegen step that emits a
	// `sources` map and is threaded in via ${node.output} arrives wrapped in
	// a Markdown code fence (```json ... ```), so we also try the
	// fence-stripped body — never the mangled original on failure.
	for _, s := range jsonCandidates(v) {
		if len(s) > 0 && (s[0] == '{' || s[0] == '[') {
			var j interface{}
			if err := json.Unmarshal([]byte(s), &j); err == nil {
				return j
			}
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

// normalizeToolArg coerces a (already ref-resolved) tool-call arg VALUE, with
// the arg KEY available so it can repair the two realistic ways an upstream
// codegen/reason step mis-shapes a structured arg before a strict MCP tool
// (e.g. tachyon_compile's `sources` map) consumes it:
//
//  1. Wrapper nesting — the step emits {"sources": {<path>:<content>}} (echoing
//     the tool-call arg name) instead of the bare {<path>:<content>} map. When
//     the parsed value is a single-key object whose key equals the arg name and
//     whose inner value is itself a container, unwrap it.
//  2. Prose/fence wrapping — the step narrates ("I'll write the contract…") and
//     then emits the JSON inside a ```json fence. coerceArg only parses when the
//     trimmed value (or a LEADING fence body) is a container, so leading prose
//     defeats it and the raw string reaches the engine ("cannot unmarshal string
//     into map"). When the raw value carries a Markdown code fence — the
//     unambiguous "an LLM wrapped my structured output" signal — scan for the
//     first embedded JSON container and use it. Gating on the fence keeps a
//     legitimate string arg that merely embeds braces (e.g. a file's content)
//     from being mangled.
//
// Falls back to coerceArg's result unchanged for every other arg.
func normalizeToolArg(key, raw string) interface{} {
	coerced := coerceArg(raw)
	switch c := coerced.(type) {
	case map[string]interface{}:
		return unwrapWrapper(key, c)
	case string:
		if containsCodeFence(raw) {
			if v, ok := decodeEmbeddedJSON(raw); ok {
				if m, ok := v.(map[string]interface{}); ok {
					return unwrapWrapper(key, m)
				}
				return v
			}
		}
		return c
	default:
		return coerced
	}
}

// unwrapWrapper returns m's single inner container when m is exactly
// {<key>: <container>} (the arg name echoed as a wrapper), else m verbatim. The
// exact key match + container-only inner keeps a real single-file `sources` map
// (whose key is a path like "src/A.sol", never the arg name) untouched.
func unwrapWrapper(key string, m map[string]interface{}) interface{} {
	if len(m) == 1 {
		if inner, ok := m[key]; ok {
			switch inner.(type) {
			case map[string]interface{}, []interface{}:
				return inner
			}
		}
	}
	return m
}

// decodeEmbeddedJSON returns the first complete JSON object/array embedded in s,
// ignoring any surrounding prose or Markdown fence. It walks each '{'/'[' and
// lets encoding/json decode one value from that offset (the decoder respects
// string quoting, so braces inside Solidity source strings never confuse it)
// and stops at the first position that yields a container. Trailing prose after
// the value is ignored by the streaming decoder.
func decodeEmbeddedJSON(s string) (interface{}, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			continue
		}
		switch v.(type) {
		case map[string]interface{}, []interface{}:
			return v, true
		}
	}
	return nil, false
}

// containsCodeFence reports whether s carries a Markdown code fence.
func containsCodeFence(s string) bool {
	return strings.Contains(s, "```")
}

// jsonCandidates returns the substrings of v worth attempting a JSON parse on,
// in priority order: the trimmed value first, then a fence-stripped body when v
// is wrapped in a Markdown code fence. coerceArg only ever returns a parse that
// SUCCEEDS, so offering the fence-stripped candidate can never mangle a plain
// string (a failed parse falls through to the verbatim original).
func jsonCandidates(v string) []string {
	trimmed := strings.TrimSpace(v)
	stripped := stripCodeFence(trimmed)
	if stripped == trimmed {
		return []string{trimmed}
	}
	return []string{trimmed, stripped}
}

// stripCodeFence removes a leading Markdown code fence (``` or ```lang) and a
// matching trailing fence from s, returning the inner body. An upstream codegen
// step's text output (threaded into a tool arg via ${node.output}) commonly
// arrives fenced; the strict tool parser needs the bare JSON. Returns the
// trimmed input unchanged when there is no leading fence.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		// Single-line "```...": nothing parseable inside.
		return strings.TrimSpace(strings.TrimPrefix(t, "```"))
	}
	body := t[nl+1:]
	if idx := strings.LastIndex(body, "```"); idx >= 0 {
		body = body[:idx]
	}
	return strings.TrimSpace(body)
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
