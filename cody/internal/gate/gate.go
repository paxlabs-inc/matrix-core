// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package gate is Cody's acceptance gate beyond the green-verification floor:
// deterministic constitution screens (tests may never be weakened or deleted
// to pass; the sheet's do-not-touch set is re-checked on the turn-in). The
// proof-of-work goal-vs-outcome LLM adjudication is RETIRED (Cassandra 2.0):
// honesty pressure now lives INSIDE the worker loop as the silent-voice
// controller (worker/cassandra.go), and acceptance is the structural floor —
// the orchestrator's independent green verification re-run plus these
// deterministic screens.
package gate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/decide"
	"matrix/cody/internal/edit"
)

// skipDirs are never scanned for test baselines.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true, ".turbo": true,
	".cody": true,
}

// TestStats fingerprints one test file: how many test cases it declares and
// how many skip/disable markers it carries.
type TestStats struct {
	Funcs int
	Skips int
}

// TestBaseline is the pre-dispatch fingerprint of every test file in the
// workspace — the ground truth a turn-in is screened against.
type TestBaseline map[string]TestStats

// IsTestFile reports whether a workspace-relative path looks like a test file
// across the supported ecosystems.
func IsTestFile(rel string) bool {
	p := filepath.ToSlash(rel)
	base := strings.ToLower(filepath.Base(p))
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.Contains(base, ".test.") || strings.Contains(base, ".spec."):
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
		return true
	case strings.HasSuffix(base, "_test.py"):
		return true
	case strings.Contains(p, "__tests__/"):
		return true
	}
	return false
}

// testCaseMarkers count declared test cases per file flavor.
var testCaseMarkers = []string{
	"func Test",    // Go
	"it(", "test(", // JS/TS
	"def test_", // Python
	"#[test]",   // Rust
}

// skipMarkers count disabled/skipped tests per file flavor.
var skipMarkers = []string{
	"t.Skip",             // Go
	".skip(",             // JS/TS (it.skip / describe.skip / test.skip)
	"xit(", "xdescribe(", // JS/TS
	"xtest(",
	"@pytest.mark.skip", // Python
	"@unittest.skip",
	"#[ignore]", // Rust
}

func statsOf(content string) TestStats {
	var s TestStats
	for _, m := range testCaseMarkers {
		s.Funcs += strings.Count(content, m)
	}
	for _, m := range skipMarkers {
		s.Skips += strings.Count(content, m)
	}
	return s
}

// CaptureTests walks the workspace and fingerprints every test file. Captured
// by the orchestrator BEFORE each dispatch so the screen compares the turn-in
// against what the worker was actually handed.
func CaptureTests(root string) (TestBaseline, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	baseline := TestBaseline{}
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != abs && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil || !IsTestFile(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		baseline[filepath.ToSlash(rel)] = statsOf(string(data))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return baseline, nil
}

// Screen runs the deterministic constitution checks on a turn-in. It returns
// "" when the turn-in passes, or a concrete rejection verdict. Violations here
// are hard: they can never be talked past because no report content is
// consulted for the facts — only the workspace itself.
func Screen(root string, baseline TestBaseline, sheet *contract.TaskSheet, report *contract.TurnInReport) string {
	var violations []string

	// Do-not-touch re-check: the engine-enforced worker refuses these at
	// mutation time, but the gate re-verifies the REPORT so a non-conforming
	// worker cannot slip an out-of-scope change through.
	for _, ch := range report.Changes {
		// Normalize through the single seam so an absolute in-root path is
		// screened identically to its relative form (a relative do-not-touch
		// pattern can never match an un-normalized absolute path).
		clean := filepath.ToSlash(filepath.Clean(ch.Path))
		if norm, err := edit.Rel(root, ch.Path); err == nil {
			clean = norm
		}
		for _, pattern := range sheet.Deliverable.DoNotTouch {
			p := filepath.ToSlash(strings.TrimSpace(pattern))
			if p == "" {
				continue
			}
			match := clean == p || strings.HasPrefix(clean, strings.TrimSuffix(p, "/")+"/")
			if !match {
				if ok, _ := filepath.Match(p, clean); ok {
					match = true
				}
			}
			if match {
				violations = append(violations, fmt.Sprintf("changed %q, which is in the sheet's do-not-touch set (%q)", ch.Path, pattern))
			}
		}
	}

	// Tests may never be weakened or deleted to pass.
	paths := make([]string, 0, len(baseline))
	for rel := range baseline {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		before := baseline[rel]
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				violations = append(violations, fmt.Sprintf("test file %q was deleted (tests may never be deleted to pass)", rel))
			}
			continue
		}
		after := statsOf(string(data))
		if after.Funcs < before.Funcs {
			violations = append(violations, fmt.Sprintf("test file %q lost test cases (%d -> %d); tests may never be removed to pass", rel, before.Funcs, after.Funcs))
		}
		if after.Skips > before.Skips {
			violations = append(violations, fmt.Sprintf("test file %q gained skip/disable markers (%d -> %d); tests may never be disabled to pass", rel, before.Skips, after.Skips))
		}
	}

	if len(violations) == 0 {
		return ""
	}
	return "constitution violation: " + strings.Join(violations, "; ")
}

// uiDriftExts are the changed-file extensions the design-drift screen inspects.
var uiDriftExts = map[string]bool{
	".css": true, ".scss": true, ".tsx": true, ".jsx": true,
	".vue": true, ".svelte": true, ".html": true,
}

// ScreenDesign is the deterministic design-drift screen (req 9.3): when a UI
// sheet carries a Design Language Record, the changed UI files are scanned for
// the banned AI-tells the record forbids (purple/indigo gradient heroes,
// gradient/glass blobs, default framework blue, stock component themes, emoji
// chrome). A hit is a hard rejection — the same banned-defaults doctrine that
// screened the DLR at authoring time screens its downstream implementation.
// Returns "" when the turn-in passes or the sheet is not a DLR-bound UI task.
func ScreenDesign(root string, sheet *contract.TaskSheet, report *contract.TurnInReport) string {
	if !sheet.UITask || strings.TrimSpace(sheet.Constraints.DesignLanguage) == "" {
		return ""
	}
	var violations []string
	for _, ch := range report.Changes {
		if ch.Kind == "delete" {
			continue
		}
		if !uiDriftExts[strings.ToLower(filepath.Ext(ch.Path))] {
			continue
		}
		rel, err := edit.Rel(root, ch.Path)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		if tells := decide.BannedTellsInText(string(data)); len(tells) > 0 {
			violations = append(violations, fmt.Sprintf("%s reintroduces banned defaults: %s", ch.Path, strings.Join(tells, ", ")))
		}
	}
	if len(violations) == 0 {
		return ""
	}
	return "design-language drift: " + strings.Join(violations, "; ")
}

// ScreenScreenshot enforces the screenshot-evidence requirement on UI turn-ins
// (req 13.2): a UI task's turn-in must carry a screenshot artifact in its
// verification evidence — design quality is verified against what was built,
// not asserted. The requirement is EVIDENCE-BASED: it applies only when the
// turn-in actually changed a renderable UI file. UITask is set by an
// inclusive keyword heuristic (orchestrator.IsUITask), so a pure-logic task
// (e.g. a Go library) can be flagged UI by a false positive; requiring a
// screenshot of a surface that does not exist would wedge such a task
// permanently with no satisfiable action. Returns "" when the sheet is not a
// UI task, changed no rendered surface, or a screenshot is present.
//
// Capability-aware degradation (req 4.3): when the sheet says the environment
// CANNOT capture a screenshot (no reachable browser service), demanding one is
// unsatisfiable by construction — the screen degrades to a pass. The
// deterministic verification re-run and the adjudicator still hold the turn-in
// to the sheet's acceptance criteria; only the impossible demand is dropped.
func ScreenScreenshot(sheet *contract.TaskSheet, report *contract.TurnInReport) string {
	if !sheet.UITask || !sheet.ScreenshotCapable {
		return ""
	}
	changedUI := false
	for _, ch := range report.Changes {
		if ch.Kind == "delete" {
			continue
		}
		if uiDriftExts[strings.ToLower(filepath.Ext(ch.Path))] {
			changedUI = true
			break
		}
	}
	if !changedUI {
		return ""
	}
	for _, ev := range report.Verification {
		if ev.Screenshot != "" {
			return ""
		}
	}
	return "UI turn-in changed a rendered surface but carries no screenshot artifact: capture a screenshot of the rendered result as evidence (req 13.2)"
}
