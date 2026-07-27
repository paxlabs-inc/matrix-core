// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import "strings"

func (m *Manager) ToolSideEffectClass(name string) (string, bool) {
	if m == nil {
		return "", false
	}
	name = strings.TrimSpace(name)
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
		case SpawnSubagentsTool, PreviewTool:
			return "shell", true
		default:
			return "", false
		}
	}
	return strings.TrimSpace(bound.sideEffect), true
}
