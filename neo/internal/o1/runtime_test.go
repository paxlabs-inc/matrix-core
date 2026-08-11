// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"

	"matrix/neo/internal/llm"
)

func TestCompileRuntimeCapabilitiesSelectsMinimalLiveSurface(t *testing.T) {
	available := []llm.Tool{
		llm.NewFunctionTool("build_project", "durable build", map[string]interface{}{"type": "object"}),
		llm.NewFunctionTool("browser__navigate", "browse", map[string]interface{}{"type": "object"}),
	}
	contract := Compile(CompileInput{Request: "Edit the repository file and run its tests."})
	got, err := CompileRuntimeCapabilities(contract, available)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "build_project" {
		t.Fatalf("unexpected selected tools: %#v", got.Tools)
	}
	if got.Manifests[0].Effects != EffectWrite {
		t.Fatalf("build_project effect = %q, want %q", got.Manifests[0].Effects, EffectWrite)
	}
}

func TestCompileRuntimeCapabilitiesFailsClosedOnMalformedSchema(t *testing.T) {
	contract := Compile(CompileInput{Request: "Edit the file."})
	_, err := CompileRuntimeCapabilities(contract, []llm.Tool{{
		Type: "function", Function: llm.FunctionDef{Name: "fs__write_file", Parameters: nil},
	}})
	if err == nil {
		t.Fatal("malformed live schema must fail before exposure")
	}
}

func TestCompileRuntimeCapabilitiesDoesNotHideToolsForContractAmbiguity(t *testing.T) {
	contract := Compile(CompileInput{Request: "Edit the file."})
	contract.Ambiguities = append(contract.Ambiguities, Ambiguity{ID: "manual", Material: true})
	got, err := CompileRuntimeCapabilities(contract, []llm.Tool{
		llm.NewFunctionTool("build_project", "durable build", map[string]interface{}{"type": "object"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "build_project" {
		t.Fatalf("material ambiguity stripped the live execution surface: %#v", got.Tools)
	}
}

func TestNativeLocalEffectClassification(t *testing.T) {
	params := map[string]interface{}{"type": "object"}
	cases := map[string]EffectClass{
		"read_text_file": EffectReadOnly,
		"git_status":     EffectReadOnly,
		"write_file":     EffectWrite,
		"move_file":      EffectWrite,
		"shell":          EffectWrite,
		"service_start":  EffectWrite,
		"service_stop":   EffectWrite,
	}
	for name, want := range cases {
		manifest := manifestFromTool(llm.NewFunctionTool(name, name, params))
		if manifest.Effects != want {
			t.Fatalf("%s effect = %q, want %q", name, manifest.Effects, want)
		}
	}
}

func TestCompileRuntimeCapabilitiesNoRequiredCapabilityExposesNothing(t *testing.T) {
	contract := Compile(CompileInput{Request: "Explain causality."})
	got, err := CompileRuntimeCapabilities(contract, []llm.Tool{
		llm.NewFunctionTool("fs__read_file", "read", map[string]interface{}{"type": "object"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("expected no tools, got %#v", got.Tools)
	}
}

func TestCompileRuntimeCapabilitiesResolvesExplicitLiveProviderIdentity(t *testing.T) {
	available := []llm.Tool{
		llm.NewFunctionTool("layerx__layerx_balance", "Your LayerX USDX balance.", map[string]interface{}{"type": "object"}),
		llm.NewFunctionTool("layerx__layerx_pay", "Pay another agent by DID using USDX.", map[string]interface{}{"type": "object"}),
		llm.NewFunctionTool("fs__read_file", "read", map[string]interface{}{"type": "object"}),
	}
	contract := Compile(CompileInput{
		Request: "Send thru LayerX 250 USDX to did:matrix:5c841daa-718a-407d-befc-c62da11ecbf1:2f555dcb594a7fb6",
	})
	got, err := CompileRuntimeCapabilities(contract, available)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range got.Tools {
		names = append(names, tool.Function.Name)
	}
	if len(names) != 2 || names[0] != "layerx__layerx_balance" || names[1] != "layerx__layerx_pay" {
		t.Fatalf("explicit live provider identity selected %v, want only the LayerX surface", names)
	}
}

func TestDialectForProviderUsesConfiguredWireIdentity(t *testing.T) {
	cases := map[string]ProviderDialect{
		"xiaomi": DialectMimo, "xai": DialectXAI,
		"fireworks": DialectFireworks, "baseten": DialectBaseten,
	}
	for provider, want := range cases {
		if got := DialectForProvider(provider); got != want {
			t.Fatalf("%s dialect = %s, want %s", provider, got, want)
		}
	}
}
