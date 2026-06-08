// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"matrix/executor/tool"
)

func TestSanitizeFuncName(t *testing.T) {
	if got := sanitizeFuncName("fs__read_file"); got != "fs__read_file" {
		t.Errorf("clean name mangled: %q", got)
	}
	if got := sanitizeFuncName("a/b.c:d"); got != "a_b_c_d" {
		t.Errorf("illegal chars not sanitized: %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := sanitizeFuncName(long); len(got) != 64 {
		t.Errorf("name not truncated to 64: len=%d", len(got))
	}
}

func TestFuncName(t *testing.T) {
	if got := funcName("paxeer-net", "get_balance"); got != "paxeer-net__get_balance" {
		t.Errorf("funcName = %q", got)
	}
}

func TestSchemaToParams(t *testing.T) {
	// nil -> empty object schema
	p := schemaToParams(nil)
	if p["type"] != "object" {
		t.Errorf("nil schema: %v", p)
	}
	// invalid JSON -> empty object schema
	p = schemaToParams(json.RawMessage(`{not valid`))
	if p["type"] != "object" {
		t.Errorf("invalid schema should degrade to object: %v", p)
	}
	// valid but missing type/properties -> both injected
	p = schemaToParams(json.RawMessage(`{"required":["x"]}`))
	if p["type"] != "object" {
		t.Errorf("type not injected: %v", p)
	}
	if _, ok := p["properties"]; !ok {
		t.Error("properties not injected")
	}
	// valid full schema passes through
	p = schemaToParams(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	props, _ := p["properties"].(map[string]interface{})
	if _, ok := props["a"]; !ok {
		t.Errorf("valid schema not preserved: %v", p)
	}
}

func TestSummarizeNonText(t *testing.T) {
	if got := summarizeNonText(nil); got != "(tool returned no content)" {
		t.Errorf("nil result: %q", got)
	}
	if got := summarizeNonText(&tool.Result{}); got != "(tool returned no content)" {
		t.Errorf("empty content: %q", got)
	}
	res := &tool.Result{Content: []tool.Content{{Text: "hello"}, {Text: "world"}}}
	if got := summarizeNonText(res); !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("text content not summarized: %q", got)
	}
}

func TestCoreExecuteSchema(t *testing.T) {
	s := coreExecuteSchema()
	if s.Function.Name != CoreExecuteTool {
		t.Errorf("core_execute schema name = %q", s.Function.Name)
	}
	props, _ := s.Function.Parameters["properties"].(map[string]interface{})
	if _, ok := props["intent"]; !ok {
		t.Error("core_execute must declare an 'intent' parameter")
	}
}
