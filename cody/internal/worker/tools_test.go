// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/llmtest"
	"matrix/cody/internal/policy"
)

func seedFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGrepGlobAndLineNumberedRead(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "src/app.go", "package src\n\nfunc Greet() string { return \"hello cody\" }\n")
	seedFile(t, root, "src/util.ts", "export const greet = () => 'hello cody'\n")
	seedFile(t, root, "node_modules/dep/index.js", "var greet = 'hello cody'\n")

	var grepOut, globOut, readOut string
	sheet := sheetFor(root, []string{"test -f src/app.go"})
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("grep", map[string]interface{}{"pattern": "hello cody", "include": "*.go"})
		case 1:
			grepOut = req.LastToolResult()
			return llmtest.CallTool("glob", map[string]interface{}{"pattern": "src/**"})
		case 2:
			globOut = req.LastToolResult()
			return llmtest.CallTool("fs_read", map[string]interface{}{"path": "src/app.go"})
		case 3:
			readOut = req.LastToolResult()
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "inspected"})
		}
	})
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s (gaps %v)", report.Status, report.Gaps)
	}
	// grep: the include filter keeps only the .go match; ignored dirs are skipped.
	if !strings.Contains(grepOut, "src/app.go:3:") || strings.Contains(grepOut, "util.ts") || strings.Contains(grepOut, "node_modules") {
		t.Fatalf("grep output wrong:\n%s", grepOut)
	}
	// glob: both src files, never node_modules.
	if !strings.Contains(globOut, "src/app.go") || !strings.Contains(globOut, "src/util.ts") || strings.Contains(globOut, "node_modules") {
		t.Fatalf("glob output wrong:\n%s", globOut)
	}
	// fs_read: line-numbered display.
	if !strings.Contains(readOut, "1| package src") || !strings.Contains(readOut, "3| func Greet()") {
		t.Fatalf("fs_read not line-numbered:\n%s", readOut)
	}
}

func TestExecAutoBackgroundAndDoneGate(t *testing.T) {
	prev := autoBackgroundAfter
	autoBackgroundAfter = 150 * time.Millisecond
	defer func() { autoBackgroundAfter = prev }()

	root := t.TempDir()
	sheet := sheetFor(root, []string{"test -f bg.txt"})
	var promotion string
	sawJobGateRefusal := false
	jobID := ""
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		last := req.LastToolResult()
		switch {
		case step == 0:
			// Outlives the promotion threshold, then delivers the artifact.
			return llmtest.CallTool("exec", map[string]interface{}{"cmd": "sleep 0.6 && echo done > bg.txt"})
		case step == 1:
			promotion = last
			for _, f := range strings.Fields(last) {
				if strings.HasPrefix(f, "job-") {
					jobID = strings.TrimRight(f, "]")
					break
				}
			}
			if jobID == "" {
				t.Fatalf("no job id in promotion notice: %q", last)
			}
			// Claim done while the job is still running: must be refused.
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "too eager"})
		case strings.Contains(last, "background jobs are still running"):
			sawJobGateRefusal = true
			return llmtest.CallTool("job_output", map[string]interface{}{"id": jobID})
		case strings.Contains(last, "RUNNING"):
			time.Sleep(200 * time.Millisecond)
			return llmtest.CallTool("job_output", map[string]interface{}{"id": jobID})
		case strings.Contains(last, "FINISHED"):
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "background job delivered bg.txt"})
		}
	})
	if !strings.Contains(promotion, "promoted to background job") {
		t.Fatalf("long exec was not promoted: %q", promotion)
	}
	if !sawJobGateRefusal {
		t.Fatal("done over a running background job was not refused")
	}
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s (gaps %v)", report.Status, report.Gaps)
	}
	if data, err := os.ReadFile(filepath.Join(root, "bg.txt")); err != nil || string(data) != "done\n" {
		t.Fatalf("background job artifact wrong: %q, %v", data, err)
	}
}

func TestExecFastCommandStaysInline(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{"true"})
	var execOut string
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("exec", map[string]interface{}{"cmd": "echo quick"})
		case 1:
			execOut = req.LastToolResult()
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "ran a command"})
		}
	})
	if !strings.HasPrefix(execOut, "[exit 0]") || !strings.Contains(execOut, "quick") {
		t.Fatalf("inline exec result wrong: %q", execOut)
	}
	if strings.Contains(execOut, "promoted") {
		t.Fatalf("fast command should not background: %q", execOut)
	}
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s", report.Status)
	}
}

func TestExecExplicitShortTimeoutStillKills(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{"true"})
	var execOut string
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("exec", map[string]interface{}{"cmd": "sleep 30", "timeout_secs": 1})
		case 1:
			execOut = req.LastToolResult()
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "timeout honored"})
		}
	})
	if !strings.Contains(execOut, "(timed out)") {
		t.Fatalf("explicit short timeout not enforced: %q", execOut)
	}
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s", report.Status)
	}
}

func TestLoopDetectionStopsStuckWorker(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{"test -f never.txt"})
	calls := 0
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		calls++
		// The same failing read, forever: identical tool, input, and output.
		return llmtest.CallTool("fs_read", map[string]interface{}{"path": "missing.txt"})
	})
	if report.Status != contract.StatusBlocked {
		t.Fatalf("status = %s, want blocked", report.Status)
	}
	if len(report.Gaps) == 0 || !strings.Contains(report.Gaps[0], "loop detected") {
		t.Fatalf("gaps = %v, want loop detection", report.Gaps)
	}
	if calls != loopMaxRepeats {
		t.Fatalf("worker stopped after %d identical calls, want %d", calls, loopMaxRepeats)
	}
}

func TestLoopDetectionIgnoresProgressingRepeats(t *testing.T) {
	var d loopDetector
	for i := 0; i < loopWindow*2; i++ {
		out := strings.Repeat("x", i+1) // output changes every call
		if d.observe("job_output", `{"id":"job-1"}`, out) {
			t.Fatalf("progressing repeat flagged as loop at call %d", i+1)
		}
	}
}

func TestPostEditDiagnosticsAppended(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{"test -f ok.json"})
	var badOut, goodOut string
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "bad.json", "content": "{\"a\": 1,}\n"})
		case 1:
			badOut = req.LastToolResult()
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "ok.json", "content": "{\"a\": 1}\n"})
		case 2:
			goodOut = req.LastToolResult()
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "wrote json"})
		}
	})
	if !strings.Contains(badOut, "[diagnostics]") || !strings.Contains(badOut, "invalid JSON") {
		t.Fatalf("broken file produced no diagnostics: %q", badOut)
	}
	if strings.Contains(goodOut, "[diagnostics]") {
		t.Fatalf("clean file produced diagnostics: %q", goodOut)
	}
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s", report.Status)
	}
}

func TestSkillsIndexInPromptAndLoadOnDemand(t *testing.T) {
	skillsRoot := t.TempDir()
	seedFile(t, skillsRoot, "api-design/SKILL.md",
		"---\nname: api-design\ndescription: REST API design patterns.\n---\n# API Design\nUse plural resource nouns.\n")
	skills, err := policy.LoadSkills(skillsRoot)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	sheet := sheetFor(root, []string{"true"})
	var skillBody string
	sawIndex := false
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			if strings.Contains(req.Messages[0].Content, "api-design — REST API design patterns.") {
				sawIndex = true
			}
			return llmtest.CallTool("skill_load", map[string]interface{}{"name": "api-design"})
		case 1:
			skillBody = req.LastToolResult()
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "skill loaded"})
		}
	})
	defer srv.Close()
	w, err := New(Options{Sheet: sheet, Root: root, Client: llmtest.NewClient(t, srv), Skills: skills})
	if err != nil {
		t.Fatal(err)
	}
	report, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sawIndex {
		t.Fatal("skills index (name + description) missing from the system prompt")
	}
	if !strings.Contains(skillBody, "Use plural resource nouns.") {
		t.Fatalf("skill_load body = %q", skillBody)
	}
	if report.Status != contract.StatusDone {
		t.Fatalf("status = %s", report.Status)
	}
}

func TestScaffoldSuiteSurfacedInPrompt(t *testing.T) {
	scaffoldDir := t.TempDir()
	seedFile(t, scaffoldDir, "scaffold-go.sh", "#!/usr/bin/env bash\n")
	seedFile(t, scaffoldDir, "scaffold-nextjs.sh", "#!/usr/bin/env bash\n")
	seedFile(t, scaffoldDir, "_common.sh", "# shared\n")

	root := t.TempDir()
	sheet := sheetFor(root, []string{"true"})
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		return llmtest.Say("noop")
	})
	defer srv.Close()
	w, err := New(Options{Sheet: sheet, Root: root, Client: llmtest.NewClient(t, srv), ScaffoldDir: scaffoldDir})
	if err != nil {
		t.Fatal(err)
	}
	prompt := w.systemPrompt()
	if !strings.Contains(prompt, "Scaffolder suite") || !strings.Contains(prompt, "go, nextjs") {
		t.Fatalf("scaffolder section missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "_common") {
		t.Fatalf("library file leaked into the stack list:\n%s", prompt)
	}
	// Unset dir: the section is omitted entirely.
	w2, err := New(Options{Sheet: sheet, Root: root, Client: llmtest.NewClient(t, srv)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w2.systemPrompt(), "Scaffolder suite") {
		t.Fatal("scaffolder section rendered without a configured dir")
	}
}

func TestWorkerUsageAccounting(t *testing.T) {
	root := t.TempDir()
	sheet := sheetFor(root, []string{"test -f greet.txt"})
	report := runWorker(t, root, sheet, func(step int, req llmtest.Request) llmtest.Turn {
		switch step {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.txt", "content": "hello cody\n"})
		case 1:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{"status": "done", "summary": "done"})
		}
	})
	// The scripted endpoint emits a usage chunk per turn (3 turns).
	if report.Usage.TotalTokens != 30 || report.Usage.PromptTokens != 21 || report.Usage.CompletionTokens != 9 {
		t.Fatalf("usage = %+v, want accumulated 21/9/30", report.Usage)
	}
}
