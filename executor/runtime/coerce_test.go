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
