// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"

	"matrix/neo/internal/llm"
)

func TestCompileRuntimeCapabilitiesSelectsMinimalLiveSurface(t *testing.T) {
	available := []llm.Tool{
		llm.NewFunctionTool("fs__read_file", "read", map[string]interface{}{"type": "object"}),
		llm.NewFunctionTool("fs__write_file", "write", map[string]interface{}{"type": "object"}),
		llm.NewFunctionTool("browser__navigate", "browse", map[string]interface{}{"type": "object"}),
	}
	contract := Compile(CompileInput{Request: "Edit the repository file."})
	got, err := CompileRuntimeCapabilities(contract, available)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 2 || got.Tools[0].Function.Name != "fs__read_file" || got.Tools[1].Function.Name != "fs__write_file" {
		t.Fatalf("unexpected selected tools: %#v", got.Tools)
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
