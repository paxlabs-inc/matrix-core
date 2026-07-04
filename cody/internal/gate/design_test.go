// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package gate

import (
	"testing"

	"matrix/cody/internal/contract"
)

// TestScreenScreenshot proves req 13.2: a UI turn-in that changed a rendered
// surface without a screenshot artifact is rejected, one with a screenshot is
// accepted, a non-UI turn-in is never held to the requirement, and — the
// regression guard — a UI-flagged task that changed no renderable surface
// (a mislabeled pure-logic library) is NOT wedged by an unsatisfiable
// screenshot demand.
func TestScreenScreenshot(t *testing.T) {
	uiSheet := &contract.TaskSheet{TaskID: "t1", UITask: true}
	noShot := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: "Hero.tsx", Kind: "create"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	if v := ScreenScreenshot(uiSheet, noShot); v == "" {
		t.Fatal("UI turn-in that built a surface without a screenshot was accepted")
	}
	withShot := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: "Hero.tsx", Kind: "create"}},
		Verification: []contract.Evidence{{Command: "screenshot", Screenshot: "hero.png"}}}
	if v := ScreenScreenshot(uiSheet, withShot); v != "" {
		t.Fatalf("UI turn-in with a screenshot rejected: %s", v)
	}
	nonUI := &contract.TaskSheet{TaskID: "t1", UITask: false}
	if v := ScreenScreenshot(nonUI, noShot); v != "" {
		t.Fatalf("non-UI turn-in held to the screenshot bar: %s", v)
	}
	// The wedge fix: a UI-flagged task whose turn-in changed only non-rendered
	// files (a pure Go library caught by the inclusive keyword heuristic) owes
	// no screenshot — there is no surface to capture, so demanding one would
	// make the task permanently uncompletable.
	logicOnly := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: "ratelimiter.go", Kind: "create"}},
		Verification: []contract.Evidence{{Command: "go build ./...", Exit: 0}}}
	if v := ScreenScreenshot(uiSheet, logicOnly); v != "" {
		t.Fatalf("mislabeled pure-logic task wedged by screenshot demand: %s", v)
	}
}

// TestScreenDesignDrift proves req 9.3: under a Design Language Record, a UI
// turn-in whose changed files reintroduce banned defaults is rejected; a clean
// one passes; and a sheet with no DLR is not screened.
func TestScreenDesignDrift(t *testing.T) {
	root := seedWorkspace(t, map[string]string{
		"clean.css":   ".btn{color:#111;background:#b8975a}",
		"drifted.css": ".hero{background:linear-gradient(90deg,#7c3aed,indigo)}",
	})
	dlrSheet := func() *contract.TaskSheet {
		return &contract.TaskSheet{TaskID: "t1", UITask: true,
			Constraints: contract.Constraints{DesignLanguage: "editorial swiss; ink + brass"}}
	}
	cleanReport := &contract.TurnInReport{TaskID: "t1",
		Changes: []contract.Change{{Path: "clean.css", Kind: "create"}}}
	if v := ScreenDesign(root, dlrSheet(), cleanReport); v != "" {
		t.Fatalf("clean UI turn-in rejected as drift: %s", v)
	}
	driftReport := &contract.TurnInReport{TaskID: "t1",
		Changes: []contract.Change{{Path: "drifted.css", Kind: "create"}}}
	if v := ScreenDesign(root, dlrSheet(), driftReport); v == "" {
		t.Fatal("drifted UI turn-in (purple gradient) was not rejected")
	}

	// No DLR on the sheet: the drift screen does not fire.
	noDLR := &contract.TaskSheet{TaskID: "t1", UITask: true}
	if v := ScreenDesign(root, noDLR, driftReport); v != "" {
		t.Fatalf("drift screen fired without a DLR in force: %s", v)
	}
}
