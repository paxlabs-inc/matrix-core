// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// NEO-WORKBENCH: the synthetic workspace_preview tool is advertised only when
// a launcher is wired, dispatches through the real Manager path, and reports
// launcher failures in-band (the model reads and adapts, no harness retry).
func TestPreviewToolWiringAndDispatch(t *testing.T) {
	m := &Manager{}
	if m.PreviewEnabled() {
		t.Fatal("preview must be disabled before SetPreview")
	}
	for _, s := range m.Schemas() {
		if s.Function.Name == PreviewTool {
			t.Fatal("unwired preview tool must not be advertised")
		}
	}
	content, isErr, err := m.Dispatch(context.Background(), PreviewTool, nil)
	if err != nil || !isErr || !strings.Contains(content, "not available") {
		t.Fatalf("unwired dispatch = (%q, %v, %v), want in-band unavailable", content, isErr, err)
	}

	called := false
	m.SetPreview(func(ctx context.Context) (string, error) {
		called = true
		return "Preview of \"Shop\" is starting", nil
	})
	if !m.PreviewEnabled() {
		t.Fatal("preview must be enabled after SetPreview")
	}
	advertised := false
	for _, s := range m.Schemas() {
		if s.Function.Name == PreviewTool {
			advertised = true
		}
	}
	if !advertised {
		t.Fatal("wired preview tool must be advertised in Schemas")
	}
	content, isErr, err = m.Dispatch(context.Background(), PreviewTool, nil)
	if err != nil || isErr || !called || !strings.Contains(content, "Preview of") {
		t.Fatalf("wired dispatch = (%q, %v, %v, called=%v), want launcher result", content, isErr, err, called)
	}

	m.SetPreview(func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("sandbox quota exceeded")
	})
	content, isErr, err = m.Dispatch(context.Background(), PreviewTool, nil)
	if err != nil || !isErr || !strings.Contains(content, "sandbox quota exceeded") {
		t.Fatalf("failed dispatch = (%q, %v, %v), want in-band failure with cause", content, isErr, err)
	}
}
