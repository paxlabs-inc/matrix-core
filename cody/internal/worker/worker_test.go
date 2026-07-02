// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/llmtest"
)

func sheetFor(root string, verifyCmds []string, doNotTouch ...string) *contract.TaskSheet {
	return &contract.TaskSheet{
		TaskID:     "t-1",
		Title:      "Create the greeting file",
		Goal:       "greet.txt exists and contains the greeting",
		Acceptance: []string{"greet.txt contains 'hello cody'"},
		Grounding:  contract.Grounding{Notes: "empty workspace"},
		Verify:     contract.Verify{Commands: verifyCmds, MustBeGreen: true},
		Deliverable: contract.Deliverable{
			Shape:      "greet.txt",
			DoNotTouch: doNotTouch,
		},
		Attempt: 1,
	}
}

func runWorker(t *testing.T, root string, sheet *contract.TaskSheet, script func(step int, req llmtest.Request) llmtest.Turn) *contract.TurnInReport {
	t.Helper()
	srv := llmtest.NewServer(t, script)
	defer srv.Close()
	w, err := New(Options{
		Sheet:  sheet,
		Root:   root,
		Client: llmtest.NewClient(t, srv),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestWorkerHappyPath(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{`grep -q "hello cody" greet.txt`})
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			// Fresh context check: only system + user(sheet).
			if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
				t.Errorf("fresh context violated: %d messages", len(req.Messages))
			}
			if !strings.Contains(req.Messages[1].Content, "TASK SHEET t-1") {
				t.Error("sheet missing from the worker's user prompt")
			}
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.txt", "content": "hello cody\n"})
		case 1:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "created greet.txt with the greeting",
				"changes": []map[string]interface{}{{"path": "greet.txt", "kind": "create", "why": "the deliverable"}},
			})
		}
	})

	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s, want done (gaps: %v)", report.Status, report.Gaps)
	}
	if !report.AllGreen() {
		t.Fatalf("report evidence not green: %+v", report.Verification)
	}
	if len(report.Changes) != 1 || report.Changes[0].Path != "greet.txt" || report.Changes[0].Kind != "create" {
		t.Fatalf("engine-tracked changes = %+v", report.Changes)
	}
	if report.Changes[0].Why != "the deliverable" {
		t.Fatalf("model's why lost: %+v", report.Changes[0])
	}
	data, err := os.ReadFile(filepath.Join(root, "greet.txt"))
	if err != nil || string(data) != "hello cody\n" {
		t.Fatalf("real file wrong: %q, %v", data, err)
	}
	if report.Verification[0].Command != `grep -q "hello cody" greet.txt` || report.Verification[0].Exit != 0 {
		t.Fatalf("evidence = %+v", report.Verification[0])
	}
}

func TestDoneRefusedWithoutVerification(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{`grep -q "hello cody" greet.txt`})
	sawRefusal := false
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.txt", "content": "hello cody\n"})
		case 1:
			// Claim done WITHOUT running verification. Must be refused.
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "done!"})
		case 2:
			if !strings.Contains(req.LastToolResult(), "verification never ran") {
				t.Errorf("expected structural refusal, got: %q", req.LastToolResult())
			} else {
				sawRefusal = true
			}
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "done with green verify"})
		}
	})
	if !sawRefusal {
		t.Fatal("done-without-verify was not refused")
	}
	if report.Status != contract.StatusDone || !report.AllGreen() {
		t.Fatalf("final report = %+v", report)
	}
}

func TestDoneRefusedWhenVerificationRed(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{`grep -q "hello cody" greet.txt`})
	sawRedRefusal := false
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			// Wrong content: verification will be red.
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.txt", "content": "goodbye\n"})
		case 1:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		case 2:
			// Try to talk the engine into done over red verification.
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "trust me"})
		default:
			if strings.Contains(req.LastToolResult(), "verification is red") {
				sawRedRefusal = true
			}
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "partial", "summary": "greeting content is wrong",
				"gaps": []string{"greet.txt does not contain the required greeting"},
			})
		}
	})
	if !sawRedRefusal {
		t.Fatal("done-over-red-verification was not refused")
	}
	if report.Status != contract.StatusPartial {
		t.Fatalf("status = %s, want partial", report.Status)
	}
	if report.AllGreen() {
		t.Fatal("red evidence reported green")
	}
	if len(report.Gaps) == 0 {
		t.Fatal("honest gaps missing from partial")
	}
}

func TestDoneRefusedWhenVerificationStale(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{`grep -q "hello cody" greet.txt`})
	sawStaleRefusal := false
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.txt", "content": "hello cody\n"})
		case 1:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		case 2:
			// Mutate AFTER the green verification.
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "extra.txt", "content": "afterthought\n"})
		case 3:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "done"})
		case 4:
			if strings.Contains(req.LastToolResult(), "changed after the last verification") {
				sawStaleRefusal = true
			}
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "done, re-verified"})
		}
	})
	if !sawStaleRefusal {
		t.Fatal("stale verification was not refused")
	}
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s, want done", report.Status)
	}
	if len(report.Changes) != 2 {
		t.Fatalf("changes = %+v", report.Changes)
	}
}

func TestDoNotTouchEnforced(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sheet := sheetFor(root, []string{"test -f ok.txt"}, "go.mod", "secrets/")
	refusals := 0
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "go.mod", "content": "module hacked\n"})
		case 1:
			if strings.Contains(req.LastToolResult(), "do-not-touch") {
				refusals++
			}
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "secrets/key.txt", "content": "shh\n"})
		case 2:
			if strings.Contains(req.LastToolResult(), "do-not-touch") {
				refusals++
			}
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "ok.txt", "content": "fine\n"})
		case 3:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "wrote ok.txt"})
		}
	})
	if refusals != 2 {
		t.Fatalf("do-not-touch refusals = %d, want 2", refusals)
	}
	data, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	if string(data) != "module demo\n" {
		t.Fatalf("protected file was modified: %q", data)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets")); !os.IsNotExist(err) {
		t.Fatal("protected dir was created")
	}
	if report.Status != contract.StatusDone || len(report.Changes) != 1 || report.Changes[0].Path != "ok.txt" {
		t.Fatalf("report = %+v", report)
	}
}

func TestReadFullObligationBlocksTurnIn(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{"test -f greet.txt"})
	overflowRe := regexp.MustCompile(`FULL output is at (\S+) —`)
	sawReadFullRefusal := false
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			// ~30KB of output: over the 24KB inline cap, under one fs_read.
			return llmtest.CallTool("exec", map[string]interface{}{
				"cmd": `i=0; while [ $i -lt 800 ]; do echo "log line $i with some padding text"; i=$((i+1)); done`,
			})
		case 1:
			// Try to turn in with the overflow unread.
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.txt", "content": "hi\n"})
		case 2:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		case 3:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "done"})
		case 4:
			last := req.LastToolResult()
			if !strings.Contains(last, "has not been read in full") {
				t.Errorf("expected read-full refusal, got: %q", last)
				return llmtest.Say("confused")
			}
			sawReadFullRefusal = true
			// Find the overflow path from the earlier exec result and read it.
			for _, m := range req.Messages {
				if m.Role == "tool" {
					if match := overflowRe.FindStringSubmatch(m.Content); match != nil {
						return llmtest.CallTool("fs_read", map[string]interface{}{"path": match[1]})
					}
				}
			}
			t.Error("overflow path not found in transcript")
			return llmtest.Say("lost")
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "done, output read in full"})
		}
	})
	if !sawReadFullRefusal {
		t.Fatal("turn_in with unread overflow was not refused")
	}
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s, want done (gaps %v)", report.Status, report.Gaps)
	}
}

func TestEngineSynthesizesHonestReportOnNonCooperation(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{"true"})
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		return llmtest.Say("Everything is finished! Great success!")
	})
	if report.Status != contract.StatusBlocked {
		t.Fatalf("status = %s, want blocked (no work happened)", report.Status)
	}
	if len(report.Gaps) == 0 {
		t.Fatal("synthesized report carries no honest gap")
	}
	if report.AllGreen() {
		t.Fatal("no verification ever ran; report must not be green")
	}
}

func TestWorkerRejectsInvalidSheet(t *testing.T) {
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn { return llmtest.Say("hi") })
	defer srv.Close()
	_, err := New(Options{
		Sheet:  &contract.TaskSheet{TaskID: "x"},
		Root:   t.TempDir(),
		Client: llmtest.NewClient(t, srv),
	})
	if err == nil {
		t.Fatal("New accepted a non-self-contained sheet")
	}
}
