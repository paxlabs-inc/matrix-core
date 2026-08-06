// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"fmt"
	"strings"
)

// EffectMetadata is the complete execution policy for one callable operation.
// It is deliberately derived from the same registry entry that owns dispatch,
// so the model-facing inventory cannot drift from recovery behavior.
type EffectMetadata struct {
	SideEffectClass       string
	IdempotencyStrategy   string
	RequiredEvidence      string
	RetryStrategy         string
	ReconciliationHandler string
}

func (m *Manager) ToolEffectMetadata(name string) (EffectMetadata, bool) {
	class, ok := m.ToolSideEffectClass(name)
	if !ok {
		return EffectMetadata{}, false
	}
	metadata := EffectMetadata{
		RequiredEvidence:      "structured-tool-outcome",
		IdempotencyStrategy:   "logical-turn-call-key",
		ReconciliationHandler: "authoritative-effect-journal",
	}
	switch strings.TrimSpace(class) {
	case "read":
		metadata.SideEffectClass = "read-only"
		metadata.RetryStrategy = "bounded-safe-retry"
		metadata.ReconciliationHandler = "repeat-read"
	case "write":
		metadata.SideEffectClass = "conditionally-idempotent"
		metadata.RetryStrategy = "reconcile-before-retry"
	case "shell":
		metadata.SideEffectClass = "non-idempotent-reconcilable"
		metadata.RetryStrategy = "never-retry-unknown"
	case "network":
		metadata.SideEffectClass = "non-idempotent-reconcilable"
		metadata.RetryStrategy = "reconcile-before-retry"
	default:
		return EffectMetadata{}, false
	}
	return metadata, true
}

// AdvertisedEffectRegistry returns exactly the currently advertised inventory.
// Conditional synthetic tools appear only while their owning seam is live.
func (m *Manager) AdvertisedEffectRegistry() (map[string]EffectMetadata, error) {
	if m == nil {
		return nil, fmt.Errorf("tool manager is unavailable")
	}
	registry := make(map[string]EffectMetadata)
	for _, schema := range m.Schemas() {
		name := strings.TrimSpace(schema.Function.Name)
		metadata, ok := m.ToolEffectMetadata(name)
		if name == "" || !ok {
			return nil, fmt.Errorf("advertised tool %q has no effect metadata", name)
		}
		if _, duplicate := registry[name]; duplicate {
			return nil, fmt.Errorf("advertised tool %q is duplicated", name)
		}
		registry[name] = metadata
	}
	return registry, nil
}

func (m *Manager) ToolSideEffectClass(name string) (string, bool) {
	if m == nil {
		return "", false
	}
	name = strings.TrimSpace(name)
	if class, ok := nativeToolSideEffectClass(name); ok {
		return class, true
	}
	bound, ok := m.byFunc[name]
	if !ok || bound == nil {
		switch name {
		case MemoryRecallTool:
			return "read", true
		case DesktopLookTool, DesktopA11yTool:
			return "network", true
		case CoreExecuteTool, MemoryMutateTool,
			ConstructRenderTool, WriteSkillTool,
			TodoTool, SavePersonalizationTool:
			return "write", true
		case SpawnSubagentsTool, PreviewTool, BuildProjectTool:
			return "shell", true
		default:
			return "", false
		}
	}
	return strings.TrimSpace(bound.sideEffect), true
}

func nativeToolSideEffectClass(name string) (string, bool) {
	switch name {
	case nativeReadFile, nativeReadMany, nativeListDir, nativeTree,
		nativeSearchFiles, nativeFileInfo, nativeShellOutput,
		nativeServiceList, nativeServiceLogs, nativeGitStatus,
		nativeGitDiff, nativeGitLog, nativeGitShow, nativeGitBranch:
		return "read", true
	case nativeWriteFile, nativeEditFile, nativePatchFiles,
		nativeCreateDir, nativeMoveFile:
		return "write", true
	case nativeShell, nativeServiceStart, nativeServiceStop,
		nativeServiceRestart:
		return "shell", true
	default:
		return "", false
	}
}
