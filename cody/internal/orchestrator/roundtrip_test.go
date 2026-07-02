// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/llmtest"
	"matrix/cody/internal/worker"
)

// seedGitRepo seeds a REAL git repo in a tempdir: a Go module whose test
// demands a Greet function that does not exist yet. The task is real work —
// `go test ./...` is red until a worker implements it.
func seedGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	root := t.TempDir()
	files := map[string]string{
		"go.mod":        "module demo\n\ngo 1.21\n",
		"greet_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif got := Greet(); got != \"hello, cody\" {\n\t\tt.Fatalf(\"Greet() = %q\", got)\n\t}\n}\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=seed", "-c", "user.email=seed@test", "add", "."},
		{"-c", "user.name=seed", "-c", "user.email=seed@test", "commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// TestSheetTurnInRoundTripSeededRepo is Property 2: the delegation contract
// executes real work with real evidence. A real orchestrator authors a
// self-contained sheet for a seeded git repo task; a REAL worker runtime
// (real llm.Client over real SSE, real edit engine, real verification runner)
// executes it in the real workspace; the turn-in report carries the real
// `go test` output — and the orchestrator's independent re-run confirms it.
func TestSheetTurnInRoundTripSeededRepo(t *testing.T) {
	root := seedGitRepo(t)

	const greetGo = "package demo\n\nfunc Greet() string {\n\treturn \"hello, cody\"\n}\n"
	script := func(step int, req llmtest.Request) llmtest.Turn {
		toolResults := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults++
			}
		}
		switch toolResults {
		case 0: // read before write: the sheet's grounding file
			return llmtest.CallTool("fs_read", map[string]interface{}{"path": "greet_test.go"})
		case 1:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.go", "content": greetGo})
		case 2:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "implemented Greet to satisfy greet_test.go",
				"changes": []map[string]interface{}{{"path": "greet.go", "kind": "create", "why": "the missing function"}},
			})
		}
	}
	srv := llmtest.NewServer(t, script)
	t.Cleanup(srv.Close)
	client := llmtest.NewClient(t, srv)

	plan := &Plan{
		Goal: "make the demo module's tests pass",
		Tasks: []*Task{{
			ID: "greet", Title: "Implement Greet", Wave: 1,
			Goal:       "greet_test.go passes: Greet() returns \"hello, cody\"",
			Acceptance: []string{"go test ./... is green", "Greet returns the greeting the test demands"},
			Grounding:  contract.Grounding{Files: []string{"greet_test.go"}, Notes: "read the test first; it defines done"},
			Verify:     []string{"go test ./..."},
			Deliverable: contract.Deliverable{
				Shape:      "greet.go implementing Greet",
				DoNotTouch: []string{"greet_test.go", "go.mod"},
			},
		}},
	}

	st := openStore(t)
	progress := openProgress(t, "plan-roundtrip")
	o, err := New(Options{
		Root: root, Plan: plan, Store: st, Progress: progress,
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			w, err := worker.New(worker.Options{Sheet: sheet, Root: root, Client: client, Grounding: grounding})
			if err != nil {
				return nil, err
			}
			return w.Run(ctx)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Done) != 1 || res.Done[0] != "greet" {
		t.Fatalf("Done = %v, Failed = %v, StopAsk = %q", res.Done, res.Failed, res.StopAsk)
	}

	// The orchestrator-authored sheet is durable and self-contained.
	sheet, err := st.LoadSheet("greet")
	if err != nil {
		t.Fatal(err)
	}
	if err := sheet.Validate(); err != nil {
		t.Fatalf("persisted sheet not self-contained: %v", err)
	}
	if len(sheet.Constraints.Constitution) == 0 || sheet.Verify.Commands[0] != "go test ./..." {
		t.Fatalf("sheet lost its constraints/verify: %+v", sheet)
	}

	// The turn-in report carries REAL verification output: the worker's own
	// `go test ./...` run, exit 0, with the go tool's real output text.
	reports, err := st.LoadReports("greet")
	if err != nil || len(reports) != 1 {
		t.Fatalf("reports = %v, %v", reports, err)
	}
	rep := reports[0]
	if rep.Status != contract.StatusDone || len(rep.Verification) == 0 {
		t.Fatalf("report = %+v", rep)
	}
	ev := rep.Verification[0]
	if ev.Command != "go test ./..." || ev.Exit != 0 {
		t.Fatalf("evidence = %+v", ev)
	}
	if !strings.Contains(ev.OutputExcerpt, "ok") || !strings.Contains(ev.OutputExcerpt, "demo") {
		t.Fatalf("evidence output is not the real go test output: %q", ev.OutputExcerpt)
	}

	// The work is real: the file exists and the module genuinely passes.
	if _, err := os.Stat(filepath.Join(root, "greet.go")); err != nil {
		t.Fatal("greet.go was not actually created")
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("final go test: %v\n%s", err, out)
	}
}
