// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// TestDesktopBridgeBijection spawns the REAL tools/desktop/desktop.mjs bridge
// subprocess from the REAL agents/neo.json desktop server block and proves the
// advertised (manifest) surface equals the bridge's live tools/list AND
// desktop-tools.json — no advertised-but-dead tool, no dead reference (the
// tool-coherence hard rule). No sandbox is needed: tools/list does not connect
// to a desktop.
func TestDesktopBridgeBijection(t *testing.T) {
	manifestPath := desktopOnlyManifest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, err := Spawn(ctx, Options{ManifestPath: manifestPath, SpawnTimeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("spawn desktop manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if len(m.warnings) > 0 {
		t.Fatalf("desktop bridge did not spawn cleanly: %v", m.warnings)
	}

	// Advertised function names (funcName = desktop__<tool>) reduced to bare
	// bytebotd tool names.
	got := map[string]bool{}
	for _, fn := range m.NaturalToolNames() {
		bt := m.byFunc[fn]
		if bt == nil || bt.alias != "desktop" {
			continue
		}
		got[bt.name] = true
	}
	// desktop-tools.json is the bridge's own registry (its tools/list source).
	want := desktopToolNames(t)
	if len(got) != len(want) {
		t.Fatalf("bridge advertises %d desktop tools, registry has %d", len(got), len(want))
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("registry tool %q is not advertised by the live bridge", n)
		}
	}
}

// TestDesktopLaneExcludedFromAutonomousSurfaces proves the guard: the
// disposable-desktop lane (bridge actuation + grounding synthetics) is a
// spend-capable human-shared surface and must never appear on an autonomous
// (Automatrix / sub-agent) surface (DOJO guard_surface).
func TestDesktopLaneExcludedFromAutonomousSurfaces(t *testing.T) {
	// The value-transfer predicate flags every desktop lane tool.
	for _, n := range []string{"desktop__desktop_click_mouse", "desktop__desktop_type_text", "desktop__desktop_write_file", DesktopLookTool, DesktopA11yTool} {
		if !IsValueTransferTool(n) {
			t.Errorf("%s should be excluded from autonomous surfaces", n)
		}
		if !IsDesktopTool(n) {
			t.Errorf("%s should be recognised as a desktop tool", n)
		}
	}
	// A non-desktop reversible tool is NOT flagged.
	if IsDesktopTool("browser_click") || IsValueTransferTool("browser_click") {
		t.Error("browser_click must not be treated as a desktop/value tool")
	}

	manifestPath := desktopOnlyManifest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, err := Spawn(ctx, Options{ManifestPath: manifestPath, SpawnTimeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	// Wire the grounding synthetics so they would appear on the full surface.
	m.SetDesktop(
		func(context.Context, string) (string, error) { return "", nil },
		func(context.Context) (string, error) { return "", nil },
	)

	// The full model-facing surface DOES carry the desktop lane...
	full := map[string]bool{}
	for _, s := range m.Schemas() {
		full[s.Function.Name] = true
	}
	if !full["desktop__desktop_click_mouse"] || !full[DesktopLookTool] || !full[DesktopA11yTool] {
		t.Fatal("full surface should advertise the desktop lane")
	}

	// ...but the autonomous surfaces do NOT, and the guard confirms it clean.
	sub := m.SubagentSchemas()
	if err := AssertNoValueTransferTools(sub); err != nil {
		t.Fatalf("sub-agent surface not clean: %v", err)
	}
	auto := m.AutomatrixSchemas()
	if err := AssertNoValueTransferTools(auto); err != nil {
		t.Fatalf("automatrix surface not clean: %v", err)
	}
	for _, s := range sub {
		if IsDesktopTool(s.Function.Name) {
			t.Errorf("sub-agent surface leaked desktop tool %q", s.Function.Name)
		}
	}
}

// TestDesktopGroundingDispatchGuards proves the grounding synthetics fail
// closed (typed in-band error, never a hang) when the desktop is unavailable.
func TestDesktopGroundingDispatchGuards(t *testing.T) {
	m := &Manager{byFunc: map[string]*boundTool{}}
	// Not wired: desktop_look / desktop_a11y report unavailable in-band.
	c, isErr, err := m.Dispatch(context.Background(), DesktopLookTool, map[string]interface{}{"target": "the button"})
	if err != nil || !isErr {
		t.Fatalf("unwired desktop_look = (%q,%v,%v), want in-band error", c, isErr, err)
	}
	c, isErr, err = m.Dispatch(context.Background(), DesktopA11yTool, nil)
	if err != nil || !isErr {
		t.Fatalf("unwired desktop_a11y = (%q,%v,%v), want in-band error", c, isErr, err)
	}

	// Wired: a returning look func flows through as a successful result.
	m.SetDesktop(
		func(_ context.Context, target string) (string, error) { return "clicked " + target, nil },
		nil,
	)
	c, isErr, err = m.Dispatch(context.Background(), DesktopLookTool, map[string]interface{}{"target": "Submit"})
	if err != nil || isErr || c != "clicked Submit" {
		t.Fatalf("wired desktop_look = (%q,%v,%v)", c, isErr, err)
	}
}

// --- helpers -------------------------------------------------------------

func desktopOnlyManifest(t *testing.T) string {
	t.Helper()
	src := os.Getenv("MATRIX_NEO_MANIFEST")
	if src == "" {
		src = filepath.Clean("../../../agents/neo.json")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read neo manifest %s: %v", src, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	var servers []map[string]json.RawMessage
	if err := json.Unmarshal(doc["servers"], &servers); err != nil {
		t.Fatalf("parse servers: %v", err)
	}
	var desktop map[string]json.RawMessage
	for _, s := range servers {
		var alias string
		_ = json.Unmarshal(s["alias"], &alias)
		if alias == "desktop" {
			desktop = s
			break
		}
	}
	if desktop == nil {
		t.Fatal("no desktop server in neo.json")
	}
	desktopArgs, err := json.Marshal([]string{
		filepath.Join(desktopRepositoryRoot(t), "tools", "desktop", "desktop.mjs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	desktop["args"] = desktopArgs
	onlyDesktop, _ := json.Marshal([]map[string]json.RawMessage{desktop})
	doc["servers"] = onlyDesktop
	out, _ := json.Marshal(doc)
	path := filepath.Join(t.TempDir(), "desktop-only.json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}
	return path
}

func desktopToolNames(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(
		desktopRepositoryRoot(t), "tools", "desktop", "desktop-tools.json",
	))
	if err != nil {
		t.Fatalf("read desktop-tools.json: %v", err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("parse desktop-tools.json: %v", err)
	}
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return names
}

func desktopRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve desktop test source")
	}
	return filepath.Clean(filepath.Join(
		filepath.Dir(sourceFile), "..", "..", "..",
	))
}
