// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
)

func seedWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func sheetFor(t *testing.T) *contract.TaskSheet {
	t.Helper()
	return &contract.TaskSheet{
		TaskID: "t1", Title: "demo", Goal: "make it work",
		Acceptance:  []string{"it works"},
		Verify:      contract.Verify{Commands: []string{"true"}, MustBeGreen: true},
		Deliverable: contract.Deliverable{Shape: "code"},
	}
}

const goTest = "package demo\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {}\n"

func TestScreenPassesHonestWork(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{
		TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: "demo.go", Kind: "create", Why: "the deliverable"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0}},
	}
	if v := Screen(root, baseline, sheetFor(t), report); v != "" {
		t.Fatalf("honest work rejected: %q", v)
	}
}

func TestScreenRejectsDeletedTestFile(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "demo_test.go")); err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, baseline, sheetFor(t), report)
	if !strings.Contains(v, "deleted") {
		t.Fatalf("deleted test file not rejected: %q", v)
	}
}

func TestScreenRejectsRemovedTestCases(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	weakened := "package demo\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(root, "demo_test.go"), []byte(weakened), 0o644); err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, baseline, sheetFor(t), report)
	if !strings.Contains(v, "lost test cases") {
		t.Fatalf("removed test cases not rejected: %q", v)
	}
}

func TestScreenRejectsAddedSkips(t *testing.T) {
	root := seedWorkspace(t, map[string]string{"demo_test.go": goTest})
	baseline, err := CaptureTests(root)
	if err != nil {
		t.Fatal(err)
	}
	skipped := strings.Replace(goTest, "func TestB(t *testing.T) {}", "func TestB(t *testing.T) { t.Skip(\"later\") }", 1)
	if err := os.WriteFile(filepath.Join(root, "demo_test.go"), []byte(skipped), 0o644); err != nil {
		t.Fatal(err)
	}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, baseline, sheetFor(t), report)
	if !strings.Contains(v, "skip/disable") {
		t.Fatalf("added skip not rejected: %q", v)
	}
}

func TestScreenRejectsDoNotTouch(t *testing.T) {
	root := seedWorkspace(t, nil)
	sheet := sheetFor(t)
	sheet.Deliverable.DoNotTouch = []string{"vendor/"}
	report := &contract.TurnInReport{TaskID: "t1", Status: contract.StatusDone,
		Changes:      []contract.Change{{Path: "vendor/lib.go", Kind: "edit", Why: "should not happen"}},
		Verification: []contract.Evidence{{Command: "true", Exit: 0}}}
	v := Screen(root, TestBaseline{}, sheet, report)
	if !strings.Contains(v, "do-not-touch") {
		t.Fatalf("do-not-touch change not rejected: %q", v)
	}
}
