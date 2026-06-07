// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"reflect"
	"testing"
)

func TestCoerceArg_Scalars(t *testing.T) {
	cases := []struct {
		in   string
		want interface{}
	}{
		{"true", true},
		{"false", false},
		{"", ""},
		{"42", int64(42)},
		{"-7", int64(-7)},
		{"3.14", float64(3.14)},
		{"0xDcCEd58294Dc2163312Df0a2497aC291A2B59261", "0xDcCEd58294Dc2163312Df0a2497aC291A2B59261"},
		{"paxeer-mainnet", "paxeer-mainnet"},
	}
	for _, c := range cases {
		got := coerceArg(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("coerceArg(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// The load-bearing case: tachyon_compile's `sources` is a map[path]->content,
// but the plan IR carries every arg VALUE as a string. A JSON-encoded object
// string MUST be parsed back into a map so the MCP tool receives the real
// object instead of a string it cannot unmarshal.
func TestCoerceArg_JSONObject(t *testing.T) {
	in := `{"src/Token.sol":"pragma solidity 0.8.27;","test/Token.t.sol":"// test"}`
	got := coerceArg(in)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("coerceArg(object) = %T, want map[string]interface{}", got)
	}
	if m["src/Token.sol"] != "pragma solidity 0.8.27;" {
		t.Errorf("sources map lost a key/value: %#v", m)
	}
}

// constructor_args / args arrays must survive the same way.
func TestCoerceArg_JSONArray(t *testing.T) {
	got := coerceArg(`["Matrix Flow Test","MFT",1000000]`)
	arr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("coerceArg(array) = %T, want []interface{}", got)
	}
	if len(arr) != 3 || arr[0] != "Matrix Flow Test" {
		t.Errorf("array coercion mangled values: %#v", arr)
	}
}

// A plain string that merely starts with a brace but is not valid JSON must
// fall through to the verbatim string, never get mangled.
func TestCoerceArg_InvalidJSONFallsThrough(t *testing.T) {
	in := "{not json"
	if got := coerceArg(in); got != in {
		t.Errorf("coerceArg(%q) = %#v, want verbatim string", in, got)
	}
}

// A codegen step's output threaded into a tool arg via ${node.output} arrives
// wrapped in a Markdown code fence. coerceArg must strip the fence and parse
// the inner JSON object so tachyon_compile receives a real `sources` map
// instead of an unparseable fenced string.
func TestCoerceArg_FencedJSONObject(t *testing.T) {
	in := "```json\n{\"src/MatrixFlowTest.sol\":\"// SPDX\\npragma solidity ^0.8.20;\"}\n```"
	got := coerceArg(in)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("coerceArg(fenced object) = %T, want map[string]interface{}", got)
	}
	if m["src/MatrixFlowTest.sol"] == "" {
		t.Errorf("fenced sources map lost its key/value: %#v", m)
	}
}

// A bare (unlabeled) fence around a JSON array must also strip + parse.
func TestCoerceArg_FencedJSONArray(t *testing.T) {
	in := "```\n[\"Matrix Flow Test\",\"MFT\",1000000]\n```"
	got := coerceArg(in)
	arr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("coerceArg(fenced array) = %T, want []interface{}", got)
	}
	if len(arr) != 3 || arr[0] != "Matrix Flow Test" {
		t.Errorf("fenced array coercion mangled values: %#v", arr)
	}
}

// A fenced block whose body is not valid JSON must still fall through to the
// verbatim original string (never the half-mangled fence-stripped body).
func TestCoerceArg_FencedNonJSONFallsThrough(t *testing.T) {
	in := "```solidity\ncontract C {}\n```"
	if got := coerceArg(in); got != in {
		t.Errorf("coerceArg(%q) = %#v, want verbatim string", in, got)
	}
}

// REGRESSION (real run 1ZC6TQ2X646D5JWKV4W2TZ7E66): the write_erc20_contract
// reason step emitted prose, then a ```json fence wrapping {"sources": {...}}.
// coerceArg leaves that a string (leading prose defeats the container check),
// so tachyon_compile got "cannot unmarshal string into map[string]string".
// normalizeToolArg must extract the embedded object AND unwrap the "sources"
// wrapper so the tool receives the bare path->content map.
func TestNormalizeToolArg_ProseFenceWrappedSources(t *testing.T) {
	raw := "I'll write the contract and test suite, then compile.\n\n" +
		"### 1. Compile\n\n" +
		"```json\n{\n  \"sources\": {\n    \"src/MFT.sol\": \"// SPDX\\npragma solidity ^0.8.20;\",\n    \"test/MFT.t.sol\": \"// test\"\n  }\n}\n```"
	got := normalizeToolArg("sources", raw)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("normalizeToolArg = %T, want bare path->content map", got)
	}
	if _, wrapped := m["sources"]; wrapped {
		t.Errorf("wrapper key not unwrapped: %#v", m)
	}
	if m["src/MFT.sol"] == "" || m["test/MFT.t.sol"] == "" {
		t.Errorf("sources map lost a file: %#v", m)
	}
}

// An inline (no-prose, no-fence) wrapper {"sources": {...}} — e.g. the planner
// echoing the arg name in the value — must also unwrap to the bare map.
func TestNormalizeToolArg_InlineWrapperUnwraps(t *testing.T) {
	raw := `{"sources":{"src/A.sol":"pragma solidity ^0.8.20;"}}`
	got := normalizeToolArg("sources", raw)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("normalizeToolArg = %T, want map", got)
	}
	if m["src/A.sol"] == "" {
		t.Errorf("unwrap lost content: %#v", m)
	}
}

// A bare, well-formed sources map (single file) must pass through unchanged —
// its key is a path, not the arg name, so the wrapper-unwrap never triggers.
func TestNormalizeToolArg_BareMapUntouched(t *testing.T) {
	raw := `{"src/Only.sol":"pragma solidity ^0.8.20;"}`
	got := normalizeToolArg("sources", raw)
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("normalizeToolArg = %T, want map", got)
	}
	if _, ok := m["src/Only.sol"]; !ok {
		t.Errorf("bare map mangled: %#v", m)
	}
}

// A wrapped JSON array under a matching arg name unwraps to the bare array.
func TestNormalizeToolArg_WrappedArrayUnwraps(t *testing.T) {
	raw := "```json\n{\"constructor_args\": [\"MFT\", 1000000]}\n```"
	got := normalizeToolArg("constructor_args", raw)
	arr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("normalizeToolArg = %T, want []interface{}", got)
	}
	if len(arr) != 2 || arr[0] != "MFT" {
		t.Errorf("array unwrap mangled: %#v", arr)
	}
}

// A plain string arg with NO code fence that merely embeds a JSON object (e.g.
// file content written to a generic tool) must NOT be mangled into an object.
func TestNormalizeToolArg_UnfencedEmbeddedJSONNotMangled(t *testing.T) {
	raw := "Hello\n{\"a\":1}\nWorld"
	if got := normalizeToolArg("content", raw); got != raw {
		t.Errorf("normalizeToolArg mangled a fence-free string: got %#v", got)
	}
}

// Scalar args keep their existing coercion through normalizeToolArg.
func TestNormalizeToolArg_ScalarsUnchanged(t *testing.T) {
	if got := normalizeToolArg("chain_id", "125"); got != int64(125) {
		t.Errorf("chain_id = %#v, want int64(125)", got)
	}
	if got := normalizeToolArg("to", "0xDcCEd58294Dc2163312Df0a2497aC291A2B59261"); got != "0xDcCEd58294Dc2163312Df0a2497aC291A2B59261" {
		t.Errorf("address mangled: %#v", got)
	}
}
