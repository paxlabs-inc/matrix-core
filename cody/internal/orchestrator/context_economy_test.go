// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/llmtest"
	"matrix/cody/internal/worker"
)

// TestContextEconomyAfterNAcceptedTasks is Property 4's economy arm as an
// EXHAUSTIVE structural check on the real message window: after N=4 tasks
// executed by REAL workers (real llm client over real SSE, real tool loop
// full of implementation noise — reads, execs, verify runs), the
// orchestrator's window holds EXACTLY the system charter, the plan, N sheet
// digests, and N turn-in digests. Every message is classified; anything else
// fails — so a leak of ANY shape is caught, not just a known marker string.
func TestContextEconomyAfterNAcceptedTasks(t *testing.T) {
	const n = 4
	root := t.TempDir()

	plan := &Plan{Goal: "seed four files"}
	for i := 1; i <= n; i++ {
		plan.Tasks = append(plan.Tasks, &Task{
			ID: fmt.Sprintf("t%d", i), Title: fmt.Sprintf("Create file%d.txt", i), Wave: i,
			Goal:        fmt.Sprintf("file%d.txt exists containing 'payload %d'", i, i),
			Acceptance:  []string{fmt.Sprintf("file%d.txt contains payload %d", i, i)},
			Verify:      []string{fmt.Sprintf(`grep -q "payload %d" file%d.txt`, i, i)},
			Deliverable: contract.Deliverable{Shape: fmt.Sprintf("file%d.txt", i)},
		})
		if i > 1 {
			plan.Tasks[i-1].Requires = []string{fmt.Sprintf("t%d", i-1)}
		}
	}

	// The scripted worker model: every task emits implementation noise (an
	// exec and a directory listing) before writing, verifying, and turning
	// in — the noise that must NEVER reach the orchestrator.
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
		id := 0
		for i := 1; i <= n; i++ {
			if strings.Contains(sheetPrompt, fmt.Sprintf("TASK SHEET t%d ", i)) {
				id = i
			}
		}
		if id == 0 {
			t.Errorf("worker prompt without a recognizable sheet: %q", sheetPrompt)
			return llmtest.Say("lost")
		}
		switch toolResults {
		case 0:
			return llmtest.CallTool("exec", map[string]interface{}{"cmd": fmt.Sprintf("echo IMPLEMENTATION-NOISE-%d", id)})
		case 1:
			return llmtest.CallTool("fs_list", map[string]interface{}{"path": "."})
		case 2:
			return llmtest.CallTool("fs_write", map[string]interface{}{
				"path": fmt.Sprintf("file%d.txt", id), "content": fmt.Sprintf("payload %d\n", id),
			})
		case 3:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": fmt.Sprintf("created file%d.txt", id),
				"changes": []map[string]interface{}{{"path": fmt.Sprintf("file%d.txt", id), "kind": "create", "why": "the deliverable"}},
			})
		}
	}
	srv := llmtest.NewServer(t, script)
	t.Cleanup(srv.Close)
	client := llmtest.NewClient(t, srv)

	o, err := New(Options{
		Root: root, Plan: plan, Store: openStore(t), Progress: openProgress(t, "plan-economy"),
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
	if len(res.Done) != n {
		t.Fatalf("Done = %v, Failed = %v, StopAsk = %q", res.Done, res.Failed, res.StopAsk)
	}

	// Exhaustive classification: every window message must be one of the
	// four allowed shapes; count each.
	var systems, plans, sheets, reports int
	for i, m := range res.Window {
		switch {
		case m.Role == "system" && strings.Contains(m.Content, "You are Cody prime"):
			systems++
			if i != 0 {
				t.Fatalf("system charter at index %d, want 0", i)
			}
		case m.Role == "user" && strings.HasPrefix(m.Content, "PLAN:"):
			plans++
		case m.Role == "assistant" && strings.HasPrefix(m.Content, "SHEET t"):
			sheets++
		case m.Role == "user" && strings.HasPrefix(m.Content, "TURN-IN t"):
			reports++
		default:
			t.Fatalf("unclassifiable window message %d (role %s): %q", i, m.Role, m.Content)
		}
		if strings.Contains(m.Content, "IMPLEMENTATION-NOISE") {
			t.Fatalf("worker implementation noise leaked into window message %d: %q", i, m.Content)
		}
	}
	if systems != 1 || plans != 1 || sheets != n || reports != n {
		t.Fatalf("window shape = %d system, %d plan, %d sheets, %d reports; want 1/1/%d/%d",
			systems, plans, sheets, reports, n, n)
	}
	if len(res.Window) != 2+2*n {
		t.Fatalf("window length = %d, want %d", len(res.Window), 2+2*n)
	}
}
