// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/tools"
)

// NEO-WORKBENCH prompt grounding: a daemon with no workspace injects no
// coding-workspace section; SetWorkspace renders the root, the active
// project's directory, the framework rule, and the no-deploy-to-show rule —
// the exact gaps behind the LinkedIn-clone incident (wrong directory,
// HTML/CSS/JS default, unasked paxc production deploy).
func TestSystemPromptWorkspaceSection(t *testing.T) {
	a := New(Options{Config: config.Default()})
	if strings.Contains(a.systemPrompt(), "Your coding workspace") {
		t.Fatal("no workspace configured: the coding-workspace section must be absent")
	}

	a.SetWorkspace("/data/workspace", "linkedin-clone", "LinkedIn Clone", "/data/workspace/linkedin-clone")
	sp := a.systemPrompt()
	for _, want := range []string{
		"Your coding workspace",
		"/data/workspace",
		"\"LinkedIn Clone\"",
		"/data/workspace/linkedin-clone",
		"Do NOT default to hand-written index.html/style.css/app.js",
		"Deploying is NOT how you show work",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("workspace section missing %q", want)
		}
	}
	// No preview launcher wired: the fallback line points at the workbench,
	// and the tool must not be named.
	if strings.Contains(sp, "workspace_preview") {
		t.Error("preview tool must not be mentioned when no launcher is wired")
	}
	if !strings.Contains(sp, "ready to preview from the workbench") {
		t.Error("missing the workbench-preview fallback line")
	}
}

func TestSystemPromptWorkspaceDefaultProject(t *testing.T) {
	a := New(Options{Config: config.Default()})
	a.SetWorkspace("/data/workspace", "default", "Workspace", "/data/workspace")
	sp := a.systemPrompt()
	if !strings.Contains(sp, "default project") {
		t.Error("default project must render the new-subdirectory guidance")
	}
	if !strings.Contains(sp, "create ONE new subdirectory of /data/workspace") {
		t.Error("default project guidance must anchor new apps under the workspace root")
	}
}

func TestSystemPromptWorkspacePreviewTool(t *testing.T) {
	m := &tools.Manager{}
	m.SetPreview(func(ctx context.Context) (string, error) { return "", nil })
	a := New(Options{Config: config.Default(), Tools: m})
	a.SetWorkspace("/data/workspace", "shop", "Shop", "/data/workspace/shop")
	sp := a.systemPrompt()
	if !strings.Contains(sp, "workspace_preview") {
		t.Error("wired preview launcher must surface the workspace_preview tool in the prompt")
	}
}
