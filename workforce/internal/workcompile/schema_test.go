package workcompile

import (
	"encoding/json"
	"testing"
)

func TestValidateJSONSchemaEnforcesNestedAuthorityAndCollections(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["grant","changes"],
		"properties":{
			"grant":{
				"type":"object",
				"required":["fence","scope"],
				"properties":{
					"fence":{"type":"integer"},
					"scope":{
						"type":"object",
						"required":["files","generation","capability"],
						"properties":{
							"files":{"type":"array","minItems":1,"maxItems":2,"items":{"type":"string","minLength":1}},
							"generation":{"type":"integer"},
							"capability":{"type":"object"}
						},
						"additionalProperties":false
					}
				},
				"additionalProperties":false
			},
			"changes":{"type":"array","minItems":1,"items":{"type":"object"}}
		},
		"additionalProperties":false
	}`)
	valid := json.RawMessage(`{
		"grant":{"fence":7,"scope":{"files":["main.go"],"generation":12,"capability":{}}},
		"changes":[{"path":"main.go"}]
	}`)
	if !validateJSONSchema(schema, valid) {
		t.Fatal("nested valid authority envelope was rejected")
	}
	cases := []json.RawMessage{
		json.RawMessage(`{"grant":{"fence":7,"scope":{"files":[],"generation":12,"capability":{}}},"changes":[{}]}`),
		json.RawMessage(`{"grant":{"fence":7,"scope":{"files":["main.go"],"capability":{}}},"changes":[{}]}`),
		json.RawMessage(`{"grant":{"fence":7.5,"scope":{"files":["main.go"],"generation":12,"capability":{}}},"changes":[{}]}`),
		json.RawMessage(`{"grant":{"fence":7,"scope":{"files":["main.go"],"generation":12,"capability":{},"widen":true}},"changes":[{}]}`),
		json.RawMessage(`{"grant":{"fence":7,"scope":{"files":["main.go"],"generation":12,"capability":{}}},"changes":[]}`),
	}
	for index, input := range cases {
		if validateJSONSchema(schema, input) {
			t.Fatalf("invalid nested authority case %d was accepted", index)
		}
	}
}

func TestValidateJSONSchemaSupportsEnumsPatternsAndNullableValues(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["state","digest","optional"],
		"properties":{
			"state":{"type":"string","enum":["active"]},
			"digest":{"type":"string","pattern":"^[a-f0-9]{64}$"},
			"optional":{"type":["object","null"]}
		},
		"additionalProperties":false
	}`)
	if !validateJSONSchema(schema, json.RawMessage(`{
		"state":"active",
		"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"optional":null
	}`)) {
		t.Fatal("valid enum, pattern, and nullable value was rejected")
	}
	if validateJSONSchema(schema, json.RawMessage(`{
		"state":"inactive",
		"digest":"not-a-digest",
		"optional":null
	}`)) {
		t.Fatal("invalid enum and pattern were accepted")
	}
}
