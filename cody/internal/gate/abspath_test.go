// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package gate

import (
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
)

// TestScreenScreensAbsolutePathDoNotTouch proves the do-not-touch screen
// compares normalized paths: an absolute in-root change to a do-not-touch file
// is rejected identically to its relative form (before the fix, the absolute
// path never matched the relative pattern and slipped through).
func TestScreenScreensAbsolutePathDoNotTouch(t *testing.T) {
	root := seedWorkspace(t, nil)
	sheet := sheetFor(t)
	sheet.Deliverable.DoNotTouch = []string{"vendor/"}
	abs := filepath.Join(root, "vendor", "lib.go")

	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: abs, Kind: "edit", Why: "should not happen"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, TestBaseline{}, sheet, report)
	if !strings.Contains(v, "do-not-touch") {
		t.Fatalf("absolute-path do-not-touch change not rejected: %q", v)
	}
}
