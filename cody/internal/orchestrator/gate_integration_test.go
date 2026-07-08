// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
)

const gateGoTest = "package demo\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {}\n"

func onFilePlan(verifyCmd string) *Plan {
	return &Plan{Goal: "one task", Tasks: []*Task{{
		ID: "t1", Title: "Do the work", Wave: 1,
		Goal: "the work is done", Acceptance: []string{"the work is done"},
		Verify:      []string{verifyCmd},
		Deliverable: contract.Deliverable{Shape: "code"},
	}}}
}

// TestGateRejectsWeakenedTests proves the deterministic constitution screen is
// wired into the loop: a worker that deletes a test file to go green is
// rejected with concrete feedback even though verification passes.
func TestGateRejectsWeakenedTests(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "demo_test.go"), []byte(gateGoTest), 0o644); err != nil {
		t.Fatal(err)
	}
	var feedback []string
	o, err := New(Options{
		Root: root, Plan: onFilePlan("true"), Store: openStore(t), MaxAttempts: 2,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			if sheet.Feedback != "" {
				feedback = append(feedback, sheet.Feedback)
			}
			// The cheating move: delete the test file, then claim done.
			_ = os.Remove(filepath.Join(root, "demo_test.go"))
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "done",
				Changes:      []contract.Change{{Path: "demo_test.go", Kind: "delete", Why: "flaky"}},
				Verification: []contract.Evidence{{Command: "true", Exit: 0}},
				Attempt:      sheet.Attempt,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("cheating turn-in accepted: Done=%v Failed=%v", res.Done, res.Failed)
	}
	if len(feedback) == 0 || !strings.Contains(feedback[0], "deleted") {
		t.Fatalf("re-dispatch feedback missing the violation: %v", feedback)
	}
}

// TestStructuralFloorAcceptsGreenTurnIn proves the retired-adjudicator loop
// still accepts honest green work through the structural floor alone: the
// orchestrator's own verification re-run is green, the screens pass, and the
// task is accepted first-attempt with no re-dispatch and no LLM verdict.
func TestStructuralFloorAcceptsGreenTurnIn(t *testing.T) {
	root := t.TempDir()
	var feedback []string
	o, err := New(Options{
		Root: root, Plan: onFilePlan("test -f done.txt"), Store: openStore(t), MaxAttempts: 3,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			if sheet.Feedback != "" {
				feedback = append(feedback, sheet.Feedback)
			}
			if err := os.WriteFile(filepath.Join(root, "done.txt"), []byte("done\n"), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "did the work",
				Changes:      []contract.Change{{Path: "done.txt", Kind: "create", Why: "the deliverable"}},
				Verification: []contract.Evidence{{Command: "test -f done.txt", Exit: 0}},
				Attempt:      sheet.Attempt,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Done) != 1 {
		t.Fatalf("Done=%v Failed=%v StopAsk=%q", res.Done, res.Failed, res.StopAsk)
	}
	if len(feedback) != 0 {
		t.Fatalf("green work was re-dispatched: %v", feedback)
	}
}

// TestStructuralFloorRejectsRedRerun proves the floor is still a floor without
// the LLM layer: a done claim whose verification does not survive the
// orchestrator's independent re-run is rejected with concrete feedback.
func TestStructuralFloorRejectsRedRerun(t *testing.T) {
	root := t.TempDir()
	var feedback []string
	o, err := New(Options{
		Root: root, Plan: onFilePlan("test -f never-created.txt"), Store: openStore(t), MaxAttempts: 2,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			if sheet.Feedback != "" {
				feedback = append(feedback, sheet.Feedback)
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "claims done without the artifact",
				Changes:      []contract.Change{{Path: "unrelated.txt", Kind: "create", Why: "cover"}},
				Verification: []contract.Evidence{{Command: "test -f never-created.txt", Exit: 0}},
				Attempt:      sheet.Attempt,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Done) != 0 {
		t.Fatalf("a red re-run was accepted: Done=%v", res.Done)
	}
	if len(feedback) == 0 || !strings.Contains(feedback[0], "did not survive independent verification") {
		t.Fatalf("rejection feedback = %v", feedback)
	}
}
