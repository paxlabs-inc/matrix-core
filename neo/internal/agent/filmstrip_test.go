// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"testing"

	"matrix/neo/internal/config"
)

// BROWSER-FILMSTRIP req.3 — the deterministic, model-invisible auto-capture
// gating. These drive the REAL screenshotForCall logic: the browser boundary is
// the one injected seam (captureFn), which stands in for the live MCP screenshot
// that the end-to-end proof exercises for real. The agent's decisions — flag on/
// off, view-changing filter, per-run cap, direct-screenshot passthrough, error
// suppression — are the real code under test.

func newFilmstripAgent(t *testing.T, cfg config.Config) (*Agent, *int) {
	t.Helper()
	calls := 0
	a := &Agent{cfg: cfg, turn: newTurn(), captureFn: func(ctx context.Context, name string) string {
		calls++
		return fmt.Sprintf("/media/shot-%d.jpg", calls)
	}}
	return a, &calls
}

func TestScreenshotForCall_CapturesOnViewChange(t *testing.T) {
	a, calls := newFilmstripAgent(t, config.Config{BrowserAutoshot: true, BrowserAutoshotMax: 40})
	ctx := context.Background()

	got := a.screenshotForCall(ctx, "browser__browser_navigate", "", false)
	if got == "" {
		t.Fatal("a successful navigation must produce an auto-captured frame")
	}
	if *calls != 1 {
		t.Fatalf("capture calls = %d, want 1", *calls)
	}
	if a.turn.autoshotCount != 1 {
		t.Fatalf("autoshotCount = %d, want 1", a.turn.autoshotCount)
	}
}

func TestScreenshotForCall_DirectScreenshotPassthrough(t *testing.T) {
	a, calls := newFilmstripAgent(t, config.Config{BrowserAutoshot: true, BrowserAutoshotMax: 40})
	// A direct browser_take_screenshot already carries its persisted URL; the
	// auto-capture must NOT fire again (no double capture).
	got := a.screenshotForCall(context.Background(), "browser__browser_take_screenshot", "/media/direct.jpg", false)
	if got != "/media/direct.jpg" {
		t.Fatalf("direct screenshot URL must pass through unchanged, got %q", got)
	}
	if *calls != 0 {
		t.Fatalf("direct screenshot must not trigger auto-capture, calls = %d", *calls)
	}
}

func TestScreenshotForCall_FlagOffSuppresses(t *testing.T) {
	a, calls := newFilmstripAgent(t, config.Config{BrowserAutoshot: false, BrowserAutoshotMax: 40})
	if got := a.screenshotForCall(context.Background(), "browser__browser_navigate", "", false); got != "" {
		t.Fatalf("NEO_BROWSER_AUTOSHOT off must suppress capture, got %q", got)
	}
	if *calls != 0 {
		t.Fatalf("flag off must not call capture, calls = %d", *calls)
	}
}

func TestScreenshotForCall_NonViewChangingAndErrorsSkip(t *testing.T) {
	a, calls := newFilmstripAgent(t, config.Config{BrowserAutoshot: true, BrowserAutoshotMax: 40})
	ctx := context.Background()

	// Read-only browser tools are not view-changing.
	if got := a.screenshotForCall(ctx, "browser__browser_snapshot", "", false); got != "" {
		t.Errorf("browser_snapshot is read-only, must not capture, got %q", got)
	}
	if got := a.screenshotForCall(ctx, "browser__browser_console_messages", "", false); got != "" {
		t.Errorf("console read must not capture, got %q", got)
	}
	// Non-browser tools never capture.
	if got := a.screenshotForCall(ctx, "fs__read_text_file", "", false); got != "" {
		t.Errorf("fs read must not capture, got %q", got)
	}
	// A failed view-changing action must not capture (the page didn't change).
	if got := a.screenshotForCall(ctx, "browser__browser_navigate", "", true); got != "" {
		t.Errorf("failed navigation must not capture, got %q", got)
	}
	if *calls != 0 {
		t.Fatalf("none of the above may call capture, calls = %d", *calls)
	}
}

func TestScreenshotForCall_PerRunCapHolds(t *testing.T) {
	a, calls := newFilmstripAgent(t, config.Config{BrowserAutoshot: true, BrowserAutoshotMax: 2})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		a.screenshotForCall(ctx, "browser__browser_click", "", false)
	}
	if *calls != 2 {
		t.Fatalf("per-run cap must bound captures to 2, got %d", *calls)
	}
	if a.turn.autoshotCount != 2 {
		t.Fatalf("autoshotCount = %d, want 2 (capped)", a.turn.autoshotCount)
	}

	// A direct screenshot still passes through even past the cap (it is the
	// model's own call, not an auto-capture, and costs no extra shot).
	if got := a.screenshotForCall(ctx, "browser__browser_take_screenshot", "/media/direct.jpg", false); got != "/media/direct.jpg" {
		t.Errorf("direct screenshot must pass through past the cap, got %q", got)
	}
}

func TestIsViewChangingBrowser(t *testing.T) {
	changing := []string{
		"browser__browser_navigate", "browser__browser_navigate_back",
		"browser__browser_click", "browser__browser_fill_form",
		"browser__browser_select_option", "browser__browser_type",
		"browser__browser_press_key",
	}
	for _, n := range changing {
		if !isViewChangingBrowser(n) {
			t.Errorf("%s should be view-changing", n)
		}
	}
	notChanging := []string{
		"browser__browser_snapshot", "browser__browser_take_screenshot",
		"browser__browser_console_messages", "browser__browser_wait_for",
		"fs__read_text_file", "web-search__web_search", "navigate", "",
	}
	for _, n := range notChanging {
		if isViewChangingBrowser(n) {
			t.Errorf("%s should NOT be view-changing", n)
		}
	}
}
