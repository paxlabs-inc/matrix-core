// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"centra/executor/tool"
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
	if class, ok := (&Manager{}).ToolSideEffectClass(
		CoreExecuteTool,
	); !ok || class != "write" {
		t.Fatalf(
			"core_execute effect metadata = %q, %t; want write, true",
			class, ok,
		)
	}
}

func TestEveryNativeAndSyntheticToolHasEffectMetadata(t *testing.T) {
	manager := &Manager{byFunc: map[string]*boundTool{}}
	for _, schema := range nativeSchemas() {
		name := schema.Function.Name
		class, ok := manager.ToolSideEffectClass(name)
		if !ok || class == "" {
			t.Fatalf("native tool %q has no effect metadata", name)
		}
	}
	for _, name := range []string{
		CoreExecuteTool, MemoryRecallTool, MemoryMutateTool,
		SpawnSubagentsTool, ConstructRenderTool, WriteSkillTool,
		TodoTool, PreviewTool, BuildProjectTool, DesktopLookTool,
		DesktopA11yTool, SavePersonalizationTool,
	} {
		class, ok := manager.ToolSideEffectClass(name)
		if !ok || class == "" {
			t.Fatalf("synthetic tool %q has no effect metadata", name)
		}
	}
}

func TestAdvertisedInventoryAndEffectRegistryHaveExactParity(t *testing.T) {
	manager := &Manager{
		native: newNativeTestRuntime(t),
		byFunc: map[string]*boundTool{
			"docs__lookup": {
				funcName: "docs__lookup", sideEffect: "read",
				params: map[string]interface{}{"type": "object"},
			},
		},
		order: []string{"docs__lookup"},
	}
	registry, err := manager.AdvertisedEffectRegistry()
	if err != nil {
		t.Fatal(err)
	}
	schemas := manager.Schemas()
	if len(registry) != len(schemas) {
		t.Fatalf("registry=%d inventory=%d", len(registry), len(schemas))
	}
	for _, schema := range schemas {
		name := schema.Function.Name
		metadata, ok := registry[name]
		if !ok {
			t.Fatalf("advertised tool %q absent from registry", name)
		}
		if metadata.SideEffectClass == "" || metadata.IdempotencyStrategy == "" ||
			metadata.RequiredEvidence == "" || metadata.RetryStrategy == "" ||
			metadata.ReconciliationHandler == "" {
			t.Fatalf("incomplete metadata for %q: %+v", name, metadata)
		}
	}
}
