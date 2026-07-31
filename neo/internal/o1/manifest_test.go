// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"testing"
)

func TestManifestSetSelectMinimal(t *testing.T) {
	ms := &ManifestSet{manifests: map[string]OperationManifest{
		"fs__read_file":     {Operation: "fs__read_file", Effects: EffectReadOnly, Verifier: "file exists"},
		"fs__write_file":    {Operation: "fs__write_file", Effects: EffectWrite, Verifier: "file integrity"},
		"browser__navigate": {Operation: "browser__navigate", Effects: EffectReadOnly, Verifier: "page loaded"},
		"memory_recall":     {Operation: "memory_recall", Effects: EffectReadOnly, Verifier: "results returned"},
	}}

	// Code task should select filesystem + memory, not browser.
	selected := ms.SelectMinimal([]string{"filesystem", "memory"})
	names := make(map[string]bool)
	for _, m := range selected {
		names[m.Operation] = true
	}
	if !names["fs__read_file"] || !names["fs__write_file"] || !names["memory_recall"] {
		t.Fatalf("expected filesystem + memory tools, got %v", selected)
	}
	if names["browser__navigate"] {
		t.Fatal("browser tool should not be selected for a code-only task")
	}
}

func TestManifestSetSelectMinimalEmptyCapsReturnsNone(t *testing.T) {
	ms := &ManifestSet{manifests: map[string]OperationManifest{
		"fs__read_file":  {Operation: "fs__read_file"},
		"fs__write_file": {Operation: "fs__write_file"},
	}}
	selected := ms.SelectMinimal(nil)
	if len(selected) != 0 {
		t.Fatalf("empty capabilities must fail closed, got %d", len(selected))
	}
}

func TestManifestSetWebEvidenceIncludesExa(t *testing.T) {
	ms := &ManifestSet{manifests: map[string]OperationManifest{
		"exa__exa_search":    {Operation: "exa__exa_search"},
		"web-search__search": {Operation: "web-search__search"},
		"browser__navigate":  {Operation: "browser__navigate"},
	}}
	selected := ms.SelectMinimal([]string{"web_evidence"})
	names := make(map[string]bool, len(selected))
	for _, manifest := range selected {
		names[manifest.Operation] = true
	}
	if !names["exa__exa_search"] || !names["web-search__search"] {
		t.Fatalf("web evidence must include Exa and ordinary search, got %v", selected)
	}
	if names["browser__navigate"] {
		t.Fatal("web evidence must not implicitly expose browser automation")
	}
}

func TestRunPreflightFailsClosedOnUnknownPrecondition(t *testing.T) {
	result := RunPreflight(PreflightEnvironment{}, OperationManifest{
		Operation: "test__op", Preconditions: []string{"unimplemented_check"},
	})
	if result.OK {
		t.Fatal("unknown preconditions must not expose an operation")
	}
}

func TestRunPreflightUsesSemanticVersionsAndExactLimits(t *testing.T) {
	env := PreflightEnvironment{
		ToolVersions:     map[string]string{"go": "1.10.0"},
		StorageAvailable: 10,
		MaxPayloadBytes:  16,
		FrameworkVersion: "v2.1.0",
	}
	manifest := OperationManifest{Operation: "test__op", Preconditions: []string{
		"version:go:1.9.0", "storage:10", "payload:16", "framework:2.0.9",
	}}
	if result := RunPreflight(env, manifest); !result.OK {
		t.Fatalf("valid semantic versions and exact limits should pass: %v", result.Failed)
	}
}

func TestRunPreflightPassesWhenAllMet(t *testing.T) {
	env := PreflightEnvironment{
		InstalledTools:   map[string]bool{"fs__read_file": true},
		Permissions:      map[string]bool{"fs.read": true},
		NetworkReachable: true,
		StorageAvailable: 1024 * 1024,
	}
	manifest := OperationManifest{
		Operation:     "fs__read_file",
		Preconditions: []string{"tool:fs__read_file", "permission:fs.read"},
		Permissions:   []string{"fs.read"},
		Effects:       EffectReadOnly,
	}
	result := RunPreflight(env, manifest)
	if !result.OK {
		t.Fatalf("preflight should pass, got failures: %v", result.Failed)
	}
}

func TestRunPreflightFailsOnMissingTool(t *testing.T) {
	env := PreflightEnvironment{
		InstalledTools: map[string]bool{},
	}
	manifest := OperationManifest{
		Operation:     "browser__navigate",
		Preconditions: []string{"tool:browser__navigate"},
		Dependencies:  []string{"browser__navigate"},
		Effects:       EffectReadOnly,
	}
	result := RunPreflight(env, manifest)
	if result.OK {
		t.Fatal("preflight should fail for missing tool")
	}
	if len(result.Failed) == 0 {
		t.Fatal("expected at least one failed precondition")
	}
}

func TestRunPreflightFailsOnMissingPermission(t *testing.T) {
	env := PreflightEnvironment{
		InstalledTools: map[string]bool{"fs__write_file": true},
		Permissions:    map[string]bool{}, // missing fs.write
	}
	manifest := OperationManifest{
		Operation:     "fs__write_file",
		Preconditions: []string{"tool:fs__write_file"},
		Permissions:   []string{"fs.write"},
		Effects:       EffectWrite,
	}
	result := RunPreflight(env, manifest)
	if result.OK {
		t.Fatal("preflight should fail for missing permission")
	}
}

func TestRunPreflightFailsOnNetworkRequiredButUnavailable(t *testing.T) {
	env := PreflightEnvironment{
		InstalledTools:   map[string]bool{"fetch__fetch": true},
		NetworkReachable: false,
	}
	manifest := OperationManifest{
		Operation: "fetch__fetch",
		Effects:   EffectExternal,
	}
	result := RunPreflight(env, manifest)
	if result.OK {
		t.Fatal("preflight should fail when network is required but unreachable")
	}
	found := false
	for _, f := range result.Failed {
		if f == "network_unreachable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected network_unreachable in failures, got %v", result.Failed)
	}
}

func TestManifestValidationCatchesMissingFields(t *testing.T) {
	ms := &ManifestSet{manifests: map[string]OperationManifest{
		"test__op": {Operation: "test__op"}, // missing description, effects, verifier
	}}
	err := ms.Validate()
	if err == nil {
		t.Fatal("validation should fail for incomplete manifest")
	}
}

func TestManifestValidationPassesForCompleteManifest(t *testing.T) {
	ms := &ManifestSet{manifests: map[string]OperationManifest{
		"test__op": {
			Operation:   "test__op",
			Description: "A test operation",
			Effects:     EffectReadOnly,
			Verifier:    "result check",
		},
	}}
	if err := ms.Validate(); err != nil {
		t.Fatalf("validation should pass for complete manifest: %v", err)
	}
}

func TestManifestValidationRequiresRecoveryForNonReadOnly(t *testing.T) {
	ms := &ManifestSet{manifests: map[string]OperationManifest{
		"test__write": {
			Operation:   "test__write",
			Description: "A write operation",
			Effects:     EffectWrite,
			Verifier:    "file check",
			// Missing recovery
		},
	}}
	err := ms.Validate()
	if err == nil {
		t.Fatal("validation should fail for write op without recovery spec")
	}
}
