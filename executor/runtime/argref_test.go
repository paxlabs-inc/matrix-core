// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"testing"

	"matrix/mcl/ir"
)

// A tool_call arg that references a prior codegen step's output via
// ${<nodeID>.output} must be resolved against that node's recorded ResultText
// before the tool sees it — the regression that sent tachyon_compile the
// literal "${n02.output}" string (which failed to unmarshal into a sources
// map). Combined with coerceArg's fence stripping, the resolved fenced JSON
// then parses into a real object.
func TestResolveOutputRefs_ToolArg(t *testing.T) {
	plan := &ir.PlanTree{
		Root: ir.PlanNode{
			ID:   "root",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{ID: "n02", Kind: ir.NodeStep, ResultText: "```json\n{\"src/MFT.sol\":\"pragma solidity ^0.8.20;\"}\n```"},
				{ID: "n03", Kind: ir.NodeToolCall},
			},
		},
	}
	outputs := collectNodeOutputs(plan)

	resolved := resolveOutputRefs("${n02.output}", outputs)
	got := coerceArg(resolved)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("resolved+coerced arg = %T, want map[string]interface{} (resolved=%q)", got, resolved)
	}
	if m["src/MFT.sol"] == "" {
		t.Errorf("resolved sources map lost its content: %#v", m)
	}
}

// REGRESSION (real run 27T11S24KRHQMG1CYRESTSCYMQ): the planner emitted the
// sources ref as Jinja-style "{{n06.outputs.sources}}" — double braces + an
// .outputs.<field> suffix. The old ${...}-only resolver left it literal, so
// tachyon_compile got the raw "{{n06.outputs.sources}}" string and failed with
// "cannot unmarshal string into map[string]string". The liberal resolver must
// resolve it to n06's output; normalizeToolArg then extracts the fenced map.
func TestResolveOutputRefs_JinjaOutputsField(t *testing.T) {
	plan := &ir.PlanTree{
		Root: ir.PlanNode{
			ID:   "root",
			Kind: ir.NodeSequential,
			Children: []ir.PlanNode{
				{ID: "n06", Kind: ir.NodeStep, ResultText: "```json\n{\"src/MRT.sol\":\"pragma solidity ^0.8.20;\",\"test/MRT.t.sol\":\"// test\"}\n```"},
				{ID: "n08", Kind: ir.NodeToolCall},
			},
		},
	}
	outputs := collectNodeOutputs(plan)

	resolved := resolveOutputRefs("{{n06.outputs.sources}}", outputs)
	got := normalizeToolArg("sources", resolved)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("normalizeToolArg = %T, want map (resolved=%q)", got, resolved)
	}
	if m["src/MRT.sol"] == "" || m["test/MRT.t.sol"] == "" {
		t.Errorf("resolved sources map lost a file: %#v", m)
	}
}

// Named-field refs must extract the scalar from the upstream node's JSON
// output envelope — NOT dump the whole blob. {{n08.outputs.project_id}} from a
// compile result {"ok":true,"data":{"project_id":"abc123",...}} → "abc123";
// {{n11.outputs.address}} from a deploy result → the bare address; and a ref
// embedded inside a JSON-array arg resolves in place.
func TestResolveOutputRefs_NamedFieldExtraction(t *testing.T) {
	outputs := map[string]string{
		"n08": `{"ok":true,"data":{"project_id":"abc123","contracts":{"MRT":{"bytecode":"0xfeed"}}}}`,
		"n11": `{"ok":true,"data":{"address":"0xDEAD","tx_hash":"0xBEEF"}}`,
		"n04": `{"ok":true,"address":"0xWALLET","chainId":125}`,
	}
	cases := map[string]string{
		"{{n08.outputs.project_id}}":          "abc123",
		"{{n11.outputs.address}}":             "0xDEAD",
		"{{n11.outputs.tx_hash}}":             "0xBEEF",
		`["{{n04.outputs.address}}"]`:         `["0xWALLET"]`,
		"{{n08.outputs.bytecode}}":            "0xfeed", // nested under contracts.MRT
	}
	for in, want := range cases {
		if got := resolveOutputRefs(in, outputs); got != want {
			t.Errorf("resolveOutputRefs(%q) = %q, want %q", in, got, want)
		}
	}
}

// When a named field is absent, fall back to the WHOLE node output (the
// sources case: the node IS the bare map, there is no "sources" key in it).
func TestResolveOutputRefs_FieldAbsentFallsBackToWholeOutput(t *testing.T) {
	fenced := "```json\n{\"src/A.sol\":\"x\"}\n```"
	outputs := map[string]string{"n06": fenced}
	got := resolveOutputRefs("{{n06.outputs.sources}}", outputs)
	if got != fenced {
		t.Errorf("absent-field fallback = %q, want whole output %q", got, fenced)
	}
	// And it still coerces into the real map downstream.
	if m, ok := normalizeToolArg("sources", got).(map[string]interface{}); !ok || m["src/A.sol"] == "" {
		t.Errorf("fallback did not coerce to sources map: %#v", normalizeToolArg("sources", got))
	}
}

// {{n01}} and {{n01.output}} resolve identically to the ${...} forms.
func TestResolveOutputRefs_JinjaBareAndOutput(t *testing.T) {
	outputs := map[string]string{"n01": "0xDeadBeef"}
	for _, in := range []string{"{{n01}}", "{{n01.output}}", "{{ n01.outputs.address }}"} {
		if got := resolveOutputRefs(in, outputs); got != "0xDeadBeef" {
			t.Errorf("resolveOutputRefs(%q) = %q, want 0xDeadBeef", in, got)
		}
	}
}

// Bare ${nodeID} (no .output suffix) resolves identically.
func TestResolveOutputRefs_BareRef(t *testing.T) {
	outputs := map[string]string{"n01": "0xDeadBeef"}
	if got := resolveOutputRefs("${n01}", outputs); got != "0xDeadBeef" {
		t.Errorf("resolveOutputRefs(${n01}) = %q, want 0xDeadBeef", got)
	}
}

// An unresolved reference (no such node, or the node produced no output) is
// left as the verbatim ${...} literal for a tool arg — never replaced with
// LLM-oriented prose that a strict tool parser could not consume.
func TestResolveOutputRefs_UnresolvedLeftLiteral(t *testing.T) {
	outputs := map[string]string{}
	in := "${n99.output}"
	if got := resolveOutputRefs(in, outputs); got != in {
		t.Errorf("resolveOutputRefs(unresolved) = %q, want verbatim %q", got, in)
	}
}

// A value with no reference passes through untouched.
func TestResolveOutputRefs_NoRef(t *testing.T) {
	outputs := map[string]string{"n01": "x"}
	in := "125"
	if got := resolveOutputRefs(in, outputs); got != in {
		t.Errorf("resolveOutputRefs(no ref) = %q, want %q", got, in)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
