// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"strings"
	"testing"
)

// The agent-facing preview launcher fails closed: with no sandbox controller
// configured the tool reports plainly instead of pretending a preview started,
// and without an active run on the context it cannot resolve a project.
func TestAgentPreviewFailsClosed(t *testing.T) {
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	if _, err := e.agentPreview(context.Background()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured agentPreview err = %v, want not-configured", err)
	}
	// Unconfigured previews must also leave the tool unadvertised.
	if e.tools != nil && e.tools.PreviewEnabled() {
		t.Fatal("preview tool must not be wired when the sandbox controller is absent")
	}
}
