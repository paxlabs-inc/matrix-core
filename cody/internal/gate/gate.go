// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package gate is Cody's acceptance gate beyond the green-verification floor:
// deterministic constitution screens (tests may never be weakened or deleted
// to pass; the sheet's do-not-touch set is re-checked on the turn-in) plus
// Cassandra-style goal-vs-outcome adjudication — does the outcome satisfy the
// sheet's goal and acceptance criteria, judged against real evidence, never by
// string-matching the worker's report. The gate follows the proven Cassandra
// posture: deterministic screens decide hard violations; the LLM verdict
// decides completeness/groundedness; an adjudicator hiccup fails OPEN (the
// independent green verification record remains the structural floor).
package gate

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix/cassandra"
	"matrix/cody/internal/contract"
	"matrix/cody/internal/decide"
	"matrix/cody/internal/llm"
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
		clean := filepath.ToSlash(filepath.Clean(ch.Path))
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
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ch.Path)))
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
// verification evidence, and the gate rejects a UI turn-in without one — design
// quality is verified against what was built, not asserted. Returns "" when the
// sheet is not a UI task or a screenshot is present.
func ScreenScreenshot(sheet *contract.TaskSheet, report *contract.TurnInReport) string {
	if !sheet.UITask {
		return ""
	}
	for _, ev := range report.Verification {
		if ev.Screenshot != "" {
			return ""
		}
	}
	return "UI turn-in carries no screenshot artifact: a UI task must capture a screenshot of the rendered result as evidence (req 13.2)"
}

// BuildRequest renders the sheet as the contract the adjudicator judges
// against: the goal, the testable acceptance criteria, and the constitution
// clauses the outcome must not have violated.
func BuildRequest(sheet *contract.TaskSheet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "== THE TASK SHEET (the contract to satisfy) ==\nTask %s: %s\n\nGOAL (what done means):\n%s\n\nACCEPTANCE CRITERIA (each must be satisfied by the executed evidence):\n", sheet.TaskID, sheet.Title, sheet.Goal)
	for i, a := range sheet.Acceptance {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a)
	}
	if len(sheet.Constraints.Constitution) > 0 {
		b.WriteString("\nCONSTITUTION (an outcome violating any of these is NOT complete, no matter what the worker claims):\n")
		for _, c := range sheet.Constraints.Constitution {
			b.WriteString("- " + c + "\n")
		}
	}
	b.WriteString("\nDecide whether the turn-in below satisfies this sheet, judged ONLY against the executed evidence — never against what the worker said it would do. A stub/mock/fake or placeholder introduced to make verification pass is an unverified claim of completion.")
	return b.String()
}

// BuildEvidence renders the ground truth for the verdict: the worker's factual
// change record plus the ORCHESTRATOR'S OWN independent verification re-run —
// the worker's narration is the claim, never the evidence.
func BuildEvidence(report *contract.TurnInReport, rerun string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- THE WORKER'S TURN-IN (the claim) ---\nstatus: %s\nsummary: %s\n", report.Status, report.Summary)
	for _, ch := range report.Changes {
		fmt.Fprintf(&b, "changed: [%s] %s — %s\n", ch.Kind, ch.Path, ch.Why)
	}
	for _, g := range report.Gaps {
		b.WriteString("admitted gap: " + g + "\n")
	}
	for _, a := range report.Assumptions {
		b.WriteString("assumption: " + a + "\n")
	}
	b.WriteString("\n--- INDEPENDENT VERIFICATION (re-run by the orchestrator, ground truth) ---\n")
	if strings.TrimSpace(rerun) == "" {
		b.WriteString("(no output)\n")
	} else {
		b.WriteString(rerun + "\n")
	}
	return b.String()
}

// Adjudicate renders the goal-vs-outcome verdict on a done claim. Empty
// verdict = accept. An adjudicator error fails OPEN (returns accept): the
// independent green verification record is the structural floor, and a
// Cassandra hiccup never wedges a plan (the proven Neo posture).
func Adjudicate(ctx context.Context, adj *cassandra.Adjudicator, sheet *contract.TaskSheet, report *contract.TurnInReport, rerun string) string {
	if adj == nil {
		return ""
	}
	v, err := adj.Adjudicate(ctx, cassandra.AuditInput{
		Request:  BuildRequest(sheet),
		Evidence: BuildEvidence(report, rerun),
	})
	if err != nil {
		return "" // fail open: verification is green and the screens passed
	}
	if v.Grounded && len(v.UnverifiedClaims) == 0 && v.CoverageComplete() {
		return ""
	}
	var reasons []string
	for _, m := range v.Missing {
		reasons = append(reasons, "missing: "+m)
	}
	for _, u := range v.UnverifiedClaims {
		reasons = append(reasons, "unverified claim: "+u)
	}
	if len(reasons) == 0 && v.Rationale != "" {
		reasons = append(reasons, v.Rationale)
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "the outcome does not satisfy the sheet's goal and acceptance criteria")
	}
	return "goal-vs-outcome adjudication rejected the turn-in: " + strings.Join(reasons, "; ")
}

// llmDecoder adapts Cody's llm.Client to the cassandra.Decoder seam.
type llmDecoder struct{ client *llm.Client }

// NewLLMDecoder wraps a Cody llm client (temperature 0 recommended) as a
// cassandra Decoder.
func NewLLMDecoder(c *llm.Client) cassandra.Decoder { return llmDecoder{client: c} }

func (d llmDecoder) Decode(ctx context.Context, system, user string) (string, error) {
	res, err := d.client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(system),
		llm.UserMessage(user),
	}})
	if err != nil {
		return "", err
	}
	return res.Message.Content, nil
}
