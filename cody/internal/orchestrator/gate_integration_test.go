// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cassandra"
	"matrix/cody/internal/contract"
	"matrix/cody/internal/gate"
	"matrix/cody/internal/llmtest"
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

// TestGateAdjudicationRejectsThenAccepts proves the goal-vs-outcome verdict is
// wired into the loop over the REAL adjudicator + real llm client: the first
// turn-in is rejected as ungrounded, the second (post-feedback) is accepted.
func TestGateAdjudicationRejectsThenAccepts(t *testing.T) {
	root := t.TempDir()
	verdicts := []string{
		`{"grounded": false, "coverage": "full", "unverified_claims": ["a placeholder implementation was introduced to satisfy verification"], "certainty": 0.85}`,
		`{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": [], "certainty": 0.9}`,
	}
	call := 0
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		v := verdicts[call]
		if call < len(verdicts)-1 {
			call++
		}
		return llmtest.Say(v)
	})
	t.Cleanup(srv.Close)
	adj := &cassandra.Adjudicator{Primary: gate.NewLLMDecoder(llmtest.NewClient(t, srv))}

	var feedback []string
	o, err := New(Options{
		Root: root, Plan: onFilePlan("test -f done.txt"), Store: openStore(t),
		Adjudicator: adj, MaxAttempts: 3,
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
	if len(feedback) != 1 || !strings.Contains(feedback[0], "placeholder implementation") {
		t.Fatalf("adjudication feedback = %v", feedback)
	}
}
