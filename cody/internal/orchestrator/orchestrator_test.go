// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"matrix/cody/internal/checkpoint"
	"matrix/cody/internal/contract"
	"matrix/cody/internal/delegate"
	"matrix/cody/internal/llmtest"
	"matrix/cody/internal/worker"
	cortex "matrix/cortex"
	"matrix/cortex/store"
)

func twoTaskPlan() *Plan {
	return &Plan{
		Goal: "seed the demo workspace",
		Tasks: []*Task{
			{
				ID: "t1", Title: "Create greet.txt", Wave: 1,
				Goal:        "greet.txt exists containing 'hello cody'",
				Acceptance:  []string{"greet.txt contains hello cody"},
				Verify:      []string{`grep -q "hello cody" greet.txt`},
				Deliverable: contract.Deliverable{Shape: "greet.txt"},
			},
			{
				ID: "t2", Title: "Create reply.txt", Wave: 2, Requires: []string{"t1"},
				Goal:        "reply.txt exists containing 'hello back'",
				Acceptance:  []string{"reply.txt contains hello back"},
				Verify:      []string{`grep -q "hello back" reply.txt`},
				Deliverable: contract.Deliverable{Shape: "reply.txt"},
			},
		},
	}
}

func openProgress(t *testing.T, conv string) *checkpoint.Progress {
	t.Helper()
	s, err := store.Open(t.TempDir(), "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return checkpoint.NewProgress(cortex.New(s), conv)
}

func openStore(t *testing.T) *contract.Store {
	t.Helper()
	st, err := contract.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// realWorkerFunc dispatches the REAL worker runtime against a scripted model:
// n==0 tool results -> write the task's file; n==1 -> exec (implementation
// noise); n==2 -> verify_run; else -> turn_in done.
func realWorkerFunc(t *testing.T, root string, alive *int32, maxAlive *int32) WorkerFunc {
	t.Helper()
	script := func(step int, req llmtest.Request) llmtest.Turn {
		sheetPrompt := ""
		toolResults := 0
		for _, m := range req.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "TASK SHEET") {
				sheetPrompt = m.Content
			}
			if m.Role == "tool" {
				toolResults++
			}
		}
		file, content := "greet.txt", "hello cody\n"
		if strings.Contains(sheetPrompt, "TASK SHEET t2") {
			file, content = "reply.txt", "hello back\n"
		}
		switch toolResults {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": file, "content": content})
		case 1:
			return llmtest.CallTool("exec", map[string]interface{}{"cmd": "echo IMPLEMENTATION-NOISE-XYZ"})
		case 2:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "created " + file,
				"changes": []map[string]interface{}{{"path": file, "kind": "create", "why": "the deliverable"}},
			})
		}
	}
	srv := llmtest.NewServer(t, script)
	t.Cleanup(srv.Close)
	client := llmtest.NewClient(t, srv)

	return func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
		n := atomic.AddInt32(alive, 1)
		defer atomic.AddInt32(alive, -1)
		for {
			cur := atomic.LoadInt32(maxAlive)
			if n <= cur || atomic.CompareAndSwapInt32(maxAlive, cur, n) {
				break
			}
		}
		w, err := worker.New(worker.Options{Sheet: sheet, Root: root, Client: client, Grounding: grounding})
		if err != nil {
			return nil, err
		}
		return w.Run(ctx)
	}
}

func TestOrchestratorEndToEndRealWorker(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	st := openStore(t)
	progress := openProgress(t, "plan-e2e")
	var alive, maxAlive int32

	o, err := New(Options{
		Root:     root,
		Plan:     plan,
		Store:    st,
		Progress: progress,
		Worker:   realWorkerFunc(t, root, &alive, &maxAlive),
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The plan completed and the real files exist.
	if len(res.Done) != 2 || res.Done[0] != "t1" || res.Done[1] != "t2" {
		t.Fatalf("Done = %v, Failed = %v, StopAsk = %q", res.Done, res.Failed, res.StopAsk)
	}
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}
	if !plan.Complete() {
		t.Fatalf("plan not complete: %s", plan.Render())
	}

	// Exactly one worker alive at a time.
	if maxAlive != 1 {
		t.Fatalf("max concurrent workers = %d, want 1", maxAlive)
	}

	// Durable contracts: sheets + per-attempt reports + the plan.
	ids, err := st.ListSheets()
	if err != nil || len(ids) != 2 {
		t.Fatalf("sheets = %v, %v", ids, err)
	}
	reports, err := st.LoadReports("t1")
	if err != nil || len(reports) != 1 || reports[0].Status != contract.StatusDone {
		t.Fatalf("reports = %+v, %v", reports, err)
	}
	saved, err := LoadPlan(st.Root())
	if err != nil || !saved.Complete() {
		t.Fatalf("persisted plan = %+v, %v", saved, err)
	}

	// Cortex checkpoints per accepted task.
	done, err := progress.Done()
	if err != nil || !done["t1"] || !done["t2"] {
		t.Fatalf("cortex checkpoints = %v, %v", done, err)
	}

	// Context economy: the window holds plan + sheets + reports ONLY. The
	// worker's implementation noise (tool output) never entered it.
	if len(res.Window) == 0 {
		t.Fatal("empty window")
	}
	sawPlan, sawSheet, sawReport := false, false, false
	for _, m := range res.Window {
		if strings.Contains(m.Content, "IMPLEMENTATION-NOISE-XYZ") {
			t.Fatal("worker implementation noise leaked into the orchestrator window")
		}
		if strings.Contains(m.Content, "PLAN:") {
			sawPlan = true
		}
		if strings.Contains(m.Content, "SHEET t1") {
			sawSheet = true
		}
		if strings.Contains(m.Content, "TURN-IN t1") {
			sawReport = true
		}
	}
	if !sawPlan || !sawSheet || !sawReport {
		t.Fatalf("window missing plan/sheet/report: plan=%v sheet=%v report=%v", sawPlan, sawSheet, sawReport)
	}
}

// lyingWorker returns a done report with fabricated green evidence WITHOUT
// doing the work — the adversarial input the acceptance gate must reject.
func lyingWorker(sheet *contract.TaskSheet) *contract.TurnInReport {
	return &contract.TurnInReport{
		TaskID: sheet.TaskID, Status: contract.StatusDone,
		Summary:      "all done, trust me",
		Verification: []contract.Evidence{{Command: sheet.Verify.Commands[0], Exit: 0, OutputExcerpt: "ok"}},
		Attempt:      sheet.Attempt,
	}
}

func TestIndependentVerificationRejectsLyingDone(t *testing.T) {
	root := t.TempDir()
	plan := &Plan{Goal: "one file", Tasks: []*Task{{
		ID: "t1", Title: "Create greet.txt", Wave: 1,
		Goal: "greet.txt exists", Acceptance: []string{"greet.txt contains hello"},
		Verify:      []string{`grep -q "hello" greet.txt`},
		Deliverable: contract.Deliverable{Shape: "greet.txt"},
	}}}
	st := openStore(t)
	var feedbackSeen string
	dispatches := 0

	o, err := New(Options{
		Root: root, Plan: plan, Store: st,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatches++
			if sheet.Attempt == 1 {
				return lyingWorker(sheet), nil
			}
			feedbackSeen = sheet.Feedback
			// Second attempt does the real work.
			if err := os.WriteFile(filepath.Join(root, "greet.txt"), []byte("hello\n"), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "actually did it",
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
	if len(res.Done) != 1 || dispatches != 2 {
		t.Fatalf("Done = %v, dispatches = %d", res.Done, dispatches)
	}
	if !strings.Contains(feedbackSeen, "did not survive independent verification") {
		t.Fatalf("re-dispatch feedback = %q", feedbackSeen)
	}
	reports, err := st.LoadReports("t1")
	if err != nil || len(reports) != 2 {
		t.Fatalf("attempt history = %+v, %v", reports, err)
	}
}

func TestAttemptCeilingHonestFailure(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	st := openStore(t)
	progress := openProgress(t, "plan-ceiling")
	dispatches := 0

	o, err := New(Options{
		Root: root, Plan: plan, Store: st, Progress: progress, MaxAttempts: 2,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatches++
			return lyingWorker(sheet), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 || res.Failed[0] != "t1" {
		t.Fatalf("Failed = %v", res.Failed)
	}
	if dispatches != 2 {
		t.Fatalf("dispatches = %d, want the ceiling (2)", dispatches)
	}
	// The dependent task was never dispatched onto a failed foundation.
	if plan.Get("t2").Status != TaskPending {
		t.Fatalf("t2 status = %s, want pending", plan.Get("t2").Status)
	}
	// The failure is checkpointed honestly.
	all, err := progress.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != "failed" || all[0].TaskID != "t1" {
		t.Fatalf("checkpoints = %+v", all)
	}
}

func TestDeterministicFailureStopsAndAsks(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	dispatches := 0
	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t),
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatches++
			return nil, delegate.Mark(delegate.ClassDeterministic, errors.New("the sheet references a file that does not exist"))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("deterministic failure burned %d attempts, want 1", dispatches)
	}
	if res.StopAsk == "" || !strings.Contains(res.StopAsk, "does not exist") {
		t.Fatalf("StopAsk = %q", res.StopAsk)
	}
}

func TestTransientWorkerLossRedispatchesSameSheet(t *testing.T) {
	root := t.TempDir()
	plan := &Plan{Goal: "one file", Tasks: []*Task{{
		ID: "t1", Title: "Create greet.txt", Wave: 1,
		Goal: "greet.txt exists", Acceptance: []string{"file exists"},
		Verify:      []string{"test -f greet.txt"},
		Deliverable: contract.Deliverable{Shape: "greet.txt"},
	}}}
	dispatches := 0
	var goals []string
	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t),
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			dispatches++
			goals = append(goals, sheet.Goal)
			if dispatches == 1 {
				return nil, errors.New("connection reset by peer") // transient
			}
			if err := os.WriteFile(filepath.Join(root, "greet.txt"), []byte("hi\n"), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "done",
				Verification: []contract.Evidence{{Command: "test -f greet.txt", Exit: 0}},
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
	if len(res.Done) != 1 || dispatches != 2 {
		t.Fatalf("Done = %v, dispatches = %d", res.Done, dispatches)
	}
	// The fresh worker got the same durable sheet (same goal).
	if goals[0] != goals[1] {
		t.Fatalf("sheet drifted across re-dispatch: %q vs %q", goals[0], goals[1])
	}
}

func TestResumeSkipsCheckpointedTasks(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	progress := openProgress(t, "plan-resume")
	// t1 was accepted in a previous life.
	if err := progress.Record(checkpoint.Checkpoint{TaskID: "t1", Attempt: 1, Status: "done", Summary: "done earlier"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "greet.txt"), []byte("hello cody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var dispatched []string
	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t), Progress: progress,
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
		t.Fatalf("dispatched = %v, want only t2 (t1 resumed from cortex)", dispatched)
	}
	if len(res.Done) != 1 || res.Done[0] != "t2" {
		t.Fatalf("Done = %v", res.Done)
	}
}

// treeDigest hashes every file outside .cody — the orchestrator must never
// mutate the workspace itself.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, ".cody") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s:%x\n", rel, sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestOrchestratorNeverWritesCode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.go"), []byte("package demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeDigest(t, root)

	plan := &Plan{Goal: "blocked work", Tasks: []*Task{{
		ID: "t1", Title: "Anything", Wave: 1, Goal: "anything",
		Acceptance: []string{"anything"}, Verify: []string{"true"},
		Deliverable: contract.Deliverable{Shape: "anything"},
	}}}
	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t), MaxAttempts: 1,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			// The worker does nothing and says so honestly.
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusBlocked,
				Summary: "cannot proceed", Gaps: []string{"blocked on purpose"},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := treeDigest(t, root); after != before {
		t.Fatal("the orchestrator mutated the workspace — it must never write code")
	}
}

func TestPlanValidation(t *testing.T) {
	if err := (&Plan{}).Validate(); err == nil {
		t.Fatal("empty plan accepted")
	}
	p := &Plan{Tasks: []*Task{{ID: "a", Requires: []string{"ghost"}}}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("dangling requires accepted: %v", err)
	}
	dup := &Plan{Tasks: []*Task{{ID: "a"}, {ID: "a"}}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate id accepted: %v", err)
	}
}

func TestNextEligibleWaveOrder(t *testing.T) {
	p := &Plan{Tasks: []*Task{
		{ID: "b", Wave: 2, Status: TaskPending, Requires: []string{"a"}},
		{ID: "a", Wave: 1, Status: TaskPending},
		{ID: "c", Wave: 1, Status: TaskDone},
	}}
	if got := p.NextEligible(); got == nil || got.ID != "a" {
		t.Fatalf("NextEligible = %+v, want a", got)
	}
	p.Get("a").Status = TaskDone
	if got := p.NextEligible(); got == nil || got.ID != "b" {
		t.Fatalf("NextEligible after a = %+v, want b", got)
	}
	p.Get("b").Status = TaskDone
	if got := p.NextEligible(); got != nil {
		t.Fatalf("NextEligible on complete plan = %+v, want nil", got)
	}
}
