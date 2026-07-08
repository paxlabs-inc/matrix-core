// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
)

// TestGreenTurnInAcceptedCheckpointed proves the structural floor end-to-end
// after the adjudicator retirement: a real implementation whose verification
// survives the orchestrator's independent re-run is accepted first-attempt AND
// checkpointed to REAL cortex, with the honest attempt history persisted.
// (The no-fakes honesty pressure now lives inside the worker loop as the
// Cassandra 2.0 silent-voice controller — see worker/cassandra.go tests.)
func TestGreenTurnInAcceptedCheckpointed(t *testing.T) {
	root := t.TempDir()
	plan := &Plan{Goal: "a real sum implementation", Tasks: []*Task{{
		ID: "t1", Title: "Implement sum.sh", Wave: 1,
		Goal:        "sum.sh computes the sum of its two arguments",
		Acceptance:  []string{"sh sum.sh 2 3 prints 5 because it adds"},
		Verify:      []string{`test "$(sh sum.sh 2 3)" = "5"`},
		Deliverable: contract.Deliverable{Shape: "sum.sh"},
	}}}

	progress := openProgress(t, "plan-structural-floor")
	st := openStore(t)
	o, err := New(Options{
		Root: root, Plan: plan, Store: st, Progress: progress, MaxAttempts: 3,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			if err := os.WriteFile(filepath.Join(root, "sum.sh"), []byte("echo $(( $1 + $2 ))\n"), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "implemented sum.sh",
				Changes:      []contract.Change{{Path: "sum.sh", Kind: "create", Why: "real addition"}},
				Verification: []contract.Evidence{{Command: sheet.Verify.Commands[0], Exit: 0}},
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
	if len(res.Done) != 1 || res.Done[0] != "t1" {
		t.Fatalf("Done = %v, Failed = %v, StopAsk = %q", res.Done, res.Failed, res.StopAsk)
	}
	// The green turn-in was checkpointed to REAL cortex.
	done, err := progress.Done()
	if err != nil || !done["t1"] {
		t.Fatalf("cortex checkpoint = %v, %v", done, err)
	}
	// The single accepted attempt persisted as the honest history.
	reports, err := st.LoadReports("t1")
	if err != nil || len(reports) != 1 {
		t.Fatalf("attempt history = %+v, %v", reports, err)
	}
	// The accepted artifact is the real implementation.
	data, err := os.ReadFile(filepath.Join(root, "sum.sh"))
	if err != nil || !strings.Contains(string(data), "$1 + $2") {
		t.Fatalf("sum.sh = %q, %v", data, err)
	}
}

// TestGateRejectsDisabledTests is the weakened-tests arm at loop level: a
// worker that DISABLES an existing test (adds t.Skip) to go green is rejected
// even though verification passes — the sibling of the deleted-test case.
func TestGateRejectsDisabledTests(t *testing.T) {
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
			// The cheating move: disable a failing test instead of fixing it.
			skipped := strings.Replace(gateGoTest,
				"func TestB(t *testing.T) {}",
				"func TestB(t *testing.T) { t.Skip(\"flaky\") }", 1)
			if err := os.WriteFile(filepath.Join(root, "demo_test.go"), []byte(skipped), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "done",
				Changes:      []contract.Change{{Path: "demo_test.go", Kind: "edit", Why: "stabilize"}},
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
		t.Fatalf("disabled-test turn-in accepted: Done=%v Failed=%v", res.Done, res.Failed)
	}
	if len(feedback) == 0 || !strings.Contains(feedback[0], "skip/disable") {
		t.Fatalf("re-dispatch feedback missing the violation: %v", feedback)
	}
}

// TestTransientFailureBoundedByCeiling proves the re-dispatch bound: a worker
// that keeps failing transiently is re-dispatched from the same durable sheet
// exactly MaxAttempts times, then the task fails HONESTLY — no stop-and-ask
// (that is reserved for deterministic failures), and the failure is
// checkpointed to real cortex.
func TestTransientFailureBoundedByCeiling(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	progress := openProgress(t, "plan-transient-ceiling")
	dispatches := 0
	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t), Progress: progress, MaxAttempts: 3,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatches++
			return nil, errors.New("connection reset by peer") // always transient
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != 3 {
		t.Fatalf("dispatches = %d, want the ceiling (3)", dispatches)
	}
	if len(res.Failed) != 1 || res.Failed[0] != "t1" {
		t.Fatalf("Failed = %v", res.Failed)
	}
	if res.StopAsk != "" {
		t.Fatalf("transient exhaustion must not stop-and-ask: %q", res.StopAsk)
	}
	// The dependent task never built on the failed foundation.
	if plan.Get("t2").Status != TaskPending {
		t.Fatalf("t2 status = %s, want pending", plan.Get("t2").Status)
	}
	// The exhaustion is checkpointed honestly to real cortex.
	all, err := progress.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != "failed" || all[0].TaskID != "t1" {
		t.Fatalf("checkpoints = %+v", all)
	}
}
