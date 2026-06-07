// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"matrix/mcl/ir"
)

// outputRefPattern matches plan-node output references the planner emits to
// wire a prior node's produced output into a later node — both into a
// NodeStep's Step.Inputs (resolved in cmd/mcl-execute) AND into a
// NodeToolCall's args (resolved in execToolCall).
//
// We are deliberately LIBERAL in what we accept (Postel's law): synth LLMs
// emit the reference in several interchangeable spellings, and an unrecognised
// one is catastrophic — the literal placeholder reaches a strict tool parser
// and fails to unmarshal (observed in prod as tachyon_compile getting the raw
// string "{{n06.outputs.sources}}" → "cannot unmarshal string into map"). All
// of the following resolve to the upstream node's recorded output:
//
//	${n06}                 ${n06.output}      ${n06.text}
//	{{n06}}                {{n06.output}}     {{n06.outputs.sources}}
//
// Group 1 is the nodeID (alphanumerics, _, -). Group 2 is the optional dotted
// suffix (e.g. ".output", ".text", ".outputs", ".outputs.project_id"). A bare
// or whole-output suffix substitutes the entire node output; a NAMED field
// suffix (.outputs.<field> / .<field>) extracts that field from the upstream
// node's JSON output so a scalar like project_id / address / tx_hash threads
// through cleanly instead of dumping the whole result envelope.
var outputRefPattern = regexp.MustCompile(`(?:\$\{|\{\{)\s*([A-Za-z0-9_-]+)((?:\.[A-Za-z0-9_]+)*)\s*\}{1,2}`)

// wholeOutputSuffixes are the dotted suffixes that mean "substitute the entire
// node output" rather than extract a named field.
var wholeOutputSuffixes = map[string]bool{"": true, "output": true, "outputs": true, "text": true, "result": true}

// collectNodeOutputs walks the plan tree and returns nodeID → recorded runtime
// output text (PlanNode.ResultText). The walker populates ResultText as steps
// and tool calls complete, so a later node can resolve ${...} references
// against the real upstream output. Mirrors the cmd/mcl-execute helper of the
// same name (which feeds NodeStep inputs); kept here so the runtime walker can
// resolve NodeToolCall args without importing package main.
func collectNodeOutputs(plan *ir.PlanTree) map[string]string {
	out := map[string]string{}
	if plan == nil {
		return out
	}
	var walk func(n *ir.PlanNode)
	walk = func(n *ir.PlanNode) {
		if n == nil {
			return
		}
		if n.ResultText != "" {
			out[n.ID] = n.ResultText
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(&plan.Root)
	return out
}

// resolveOutputRefs replaces every ${<nodeID>.output} reference in val with the
// upstream node's actual recorded output. Unlike the NodeStep-input resolver
// (whose unresolved-marker prose is tuned for an LLM consumer), this is for
// TOOL-CALL args feeding strict parsers: an unresolved reference is left as the
// verbatim ${...} literal rather than substituting prose that a tool could not
// parse. The common, load-bearing case is a codegen step that emits a Solidity
// `sources` map which a following tachyon_compile consumes via ${node.output};
// without this, the literal placeholder reached the engine and failed to
// unmarshal ("cannot unmarshal string into map").
func resolveOutputRefs(val string, outputs map[string]string) string {
	if !strings.Contains(val, "${") && !strings.Contains(val, "{{") {
		return val
	}
	return outputRefPattern.ReplaceAllStringFunc(val, func(match string) string {
		m := outputRefPattern.FindStringSubmatch(match)
		if len(m) < 3 {
			return match
		}
		out, ok := outputs[m[1]]
		if !ok || out == "" {
			return match // unknown node / no output yet → leave the literal
		}
		field := refField(m[2])
		if field == "" {
			return out // whole-output reference
		}
		// Named-field reference: pull the scalar out of the node's JSON
		// output. Falls back to the whole output when the field is absent
		// (e.g. the planner asked for {{n06.outputs.sources}} but the node
		// IS the bare sources map — the whole output is the value).
		if v, ok := extractField(out, field); ok {
			return v
		}
		return out
	})
}

// refField reduces a dotted ref suffix to the named field it requests, or ""
// for a whole-output reference. ".outputs.project_id" → "project_id";
// ".address" → "address"; ".output"/".outputs"/".text"/"" → "".
func refField(suffix string) string {
	toks := strings.Split(strings.Trim(suffix, "."), ".")
	last := toks[len(toks)-1]
	if wholeOutputSuffixes[last] {
		return ""
	}
	return last
}

// extractField parses a node's recorded output (tolerant of Markdown fences /
// surrounding prose / result envelopes like {"ok":true,"data":{...}}) and
// returns the named field's value as a string, recursively searching nested
// objects/arrays for the first matching key. Scalars stringify directly;
// containers (e.g. an abi array) are re-encoded as JSON.
func extractField(nodeOutput, field string) (string, bool) {
	root, ok := decodeEmbeddedJSON(nodeOutput)
	if !ok {
		return "", false
	}
	found, ok := findKey(root, field)
	if !ok {
		return "", false
	}
	switch x := found.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	case nil:
		return "", false
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

// findKey returns the first value stored under key anywhere in the JSON value
// v (depth-first), unwrapping result envelopes so {"ok":true,"data":{"x":1}}
// resolves "x".
func findKey(v interface{}, key string) (interface{}, bool) {
	switch x := v.(type) {
	case map[string]interface{}:
		if val, ok := x[key]; ok {
			return val, true
		}
		for _, sub := range x {
			if val, ok := findKey(sub, key); ok {
				return val, true
			}
		}
	case []interface{}:
		for _, sub := range x {
			if val, ok := findKey(sub, key); ok {
				return val, true
			}
		}
	}
	return nil, false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
