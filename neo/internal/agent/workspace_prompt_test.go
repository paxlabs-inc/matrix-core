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
// project's directory, the durable-Build boundary, and the no-deploy rule —
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
		"plain static files are only right",
		"Local workspace tools and the durable Build worker are unavailable",
		"Deploying is NOT how you show work",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("workspace section missing %q", want)
		}
	}
	// No Build worker is wired: local coding is unavailable and the legacy
	// interactive preview tool must not be presented as a substitute.
	if strings.Contains(sp, "workspace_preview") {
		t.Error("preview tool must not be mentioned when no launcher is wired")
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

func TestSystemPromptDoesNotUseLegacyPreviewAsCodingFallback(t *testing.T) {
	m := &tools.Manager{}
	m.SetPreview(func(ctx context.Context) (string, error) { return "", nil })
	a := New(Options{Config: config.Default(), Tools: m})
	a.SetWorkspace("/data/workspace", "shop", "Shop", "/data/workspace/shop")
	sp := a.systemPrompt()
	if strings.Contains(sp, "call workspace_preview") {
		t.Error("legacy preview must not replace the durable Build path")
	}
	if !strings.Contains(sp, "Local workspace tools are unavailable") {
		t.Error("missing honest workspace-unavailable guidance")
	}
	if strings.Contains(sp, "Build worker") || strings.Contains(sp, "build_project") {
		t.Error("inactive Build delegation leaked into the prompt")
	}
}

func TestNativeLocalPolicyOwnsSubstantialWorkWhenBuildIsInactive(t *testing.T) {
	var b strings.Builder
	writeNativeLocalPolicy(&b, false)
	policy := b.String()
	for _, want := range []string{
		"Complete project work end to end in this Neo run",
		"runtime-owned coding checkpoint",
		"deliver the verified result directly",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("native coding policy missing %q", want)
		}
	}
	if strings.Contains(policy, "build_project") || strings.Contains(policy, "Build worker") {
		t.Fatal("inactive Build delegation leaked into native coding policy")
	}
}

func TestNativeLocalPolicyKeepsBuildForSubstantialJobs(t *testing.T) {
	var b strings.Builder
	writeNativeLocalPolicy(&b, true)
	policy := b.String()
	for _, want := range []string{
		"native file tools",
		"service_start/list/logs/stop/restart",
		"read-only git tools",
		"substantial coding jobs",
		"Do not delegate simple file inspection",
		"Once build_project reports that the durable job was accepted, STOP",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("coding delegation policy missing %q", want)
		}
	}
}

func TestWorkspacePromptUsesBuildWhenNativeRuntimeIsUnavailable(t *testing.T) {
	m := &tools.Manager{}
	m.SetBuildProject(func(context.Context, tools.BuildRequest) (string, error) { return "accepted", nil })
	a := New(Options{Config: config.Default(), Tools: m})
	a.SetWorkspace("/data/workspace", "shop", "Shop", "/data/workspace/shop")
	prompt := a.systemPrompt()
	if !strings.Contains(prompt, "Native local tools are unavailable in this session") {
		t.Fatal("workspace prompt does not explain the degraded build-only path")
	}
	if strings.Contains(prompt, "build setup with your shell") {
		t.Fatal("workspace prompt still directs Neo to the removed interactive shell path")
	}
}

func TestRecoveryHandoffLeadsWithPriorUserContext(t *testing.T) {
	a := New(Options{Config: config.Default()})
	handoff := "Latest user request:\n- Keep the checkout flow intact.\n\nRecent thread context:\n- User: the payment callback still fails.\n\nFailure context (secondary):\n- callback returned 502"
	a.SetRecoveryHandoff(handoff)
	prompt := a.systemPrompt()
	for _, want := range []string{
		"Recovered context from the immediately previous thread",
		"PRIMARY historical context",
		"Keep the checkout flow intact",
		"Failure context (secondary)",
		"newest message in this thread always wins",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("recovery prompt missing %q", want)
		}
	}
	if strings.Index(prompt, "Keep the checkout flow intact") > strings.Index(prompt, "callback returned 502") {
		t.Fatal("latest user context must appear before secondary failure context")
	}
}
