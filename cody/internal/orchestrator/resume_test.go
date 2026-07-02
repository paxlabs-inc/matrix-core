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

	"matrix/cody/internal/checkpoint"
	"matrix/cody/internal/contract"
	cortex "matrix/cortex"
	"matrix/cortex/store"
)

// openProgressAt opens (or REOPENS) a real cortex store at a fixed dir — the
// restart semantics a resume test needs.
func openProgressAt(t *testing.T, dir, conv string) (*checkpoint.Progress, func()) {
	t.Helper()
	s, err := store.Open(dir, "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint.NewProgress(cortex.New(s), conv), func() { s.Close() }
}

// TestKillMidPlanResumesAtCorrectNextTask proves durable resume end-to-end:
// process #1 accepts t1 and is killed (context canceled) before t2 completes;
// process #2 — a FRESH orchestrator over the same durable store and the same
// reopened cortex — reconstructs the lean window (plan + t1's sheet + t1's
// report) and resumes at exactly t2.
func TestKillMidPlanResumesAtCorrectNextTask(t *testing.T) {
	root := t.TempDir()
	storeDir := t.TempDir()
	cortexDir := t.TempDir()

	st, err := contract.OpenStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	// --- process #1: accept t1, die during t2 -----------------------------
	progress1, close1 := openProgressAt(t, cortexDir, "plan-kill")
	ctx, cancel := context.WithCancel(context.Background())
	var dispatched1 []string
	o1, err := New(Options{
		Root: root, Plan: twoTaskPlan(), Store: st, Progress: progress1,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatched1 = append(dispatched1, sheet.TaskID)
			if sheet.TaskID == "t2" {
				cancel() // the kill lands mid-plan, before t2 turns in
				return nil, ctx.Err()
			}
			if err := os.WriteFile(filepath.Join(root, "greet.txt"), []byte("hello cody\n"), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "created greet.txt",
				Changes:      []contract.Change{{Path: "greet.txt", Kind: "create", Why: "the deliverable"}},
				Verification: []contract.Evidence{{Command: sheet.Verify.Commands[0], Exit: 0}},
				Attempt:      sheet.Attempt,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o1.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("run #1 = %v, want context.Canceled (the kill)", err)
	}
	close1() // the process is gone

	// --- process #2: fresh everything except the durable state ------------
	progress2, close2 := openProgressAt(t, cortexDir, "plan-kill")
	defer close2()
	plan2, err := LoadPlan(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	var dispatched2 []string
	o2, err := New(Options{
		Root: root, Plan: plan2, Store: st, Progress: progress2,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatched2 = append(dispatched2, sheet.TaskID)
			if err := os.WriteFile(filepath.Join(root, "reply.txt"), []byte("hello back\n"), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "created reply.txt",
				Changes:      []contract.Change{{Path: "reply.txt", Kind: "create", Why: "the deliverable"}},
				Verification: []contract.Evidence{{Command: sheet.Verify.Commands[0], Exit: 0}},
				Attempt:      sheet.Attempt,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Resumed at exactly the correct next task: t1 never re-dispatched.
	if len(dispatched2) != 1 || dispatched2[0] != "t2" {
		t.Fatalf("process #2 dispatched %v, want only t2", dispatched2)
	}
	if len(res.Done) != 1 || res.Done[0] != "t2" {
		t.Fatalf("Done = %v", res.Done)
	}
	if !plan2.Complete() {
		t.Fatalf("plan not complete after resume: %s", plan2.Render())
	}

	// The reconstructed lean window holds t1's sheet + report digests (from
	// the durable store, keyed by the cortex checkpoint) — no transcripts.
	var sawSheet1, sawReport1 bool
	for _, m := range res.Window {
		if strings.Contains(m.Content, "SHEET t1") {
			sawSheet1 = true
		}
		if strings.Contains(m.Content, "TURN-IN t1") {
			sawReport1 = true
		}
	}
	if !sawSheet1 || !sawReport1 {
		t.Fatalf("reconstructed window missing t1 history: sheet=%v report=%v", sawSheet1, sawReport1)
	}
}

// TestStaleInProgressTaskIsReDispatched proves a task caught in_progress by a
// crash is reset and re-dispatched from its durable sheet.
func TestStaleInProgressTaskIsReDispatched(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	plan.Get("t1").Status = TaskDone
	plan.Get("t2").Status = TaskInProgress // the crash artifact
	if err := os.WriteFile(filepath.Join(root, "greet.txt"), []byte("hello cody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var dispatched []string
	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t),
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatched = append(dispatched, sheet.TaskID)
			if err := os.WriteFile(filepath.Join(root, "reply.txt"), []byte("hello back\n"), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "done",
				Verification: []contract.Evidence{{Command: sheet.Verify.Commands[0], Exit: 0}},
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
	if len(dispatched) != 1 || dispatched[0] != "t2" {
		t.Fatalf("dispatched = %v, want the stale in_progress t2", dispatched)
	}
	if len(res.Done) != 1 || res.Done[0] != "t2" {
		t.Fatalf("Done = %v", res.Done)
	}
}
