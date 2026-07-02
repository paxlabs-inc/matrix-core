// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"matrix/cody/internal/llmtest"
)

// TestCodydKillMidPlanResumesAtNextTask is Property 4's durability arm at the
// codyd level: engine #1 accepts t1 and is "killed" mid-t2 (abandoned with
// its worker wedged and the ledger still saying running — exactly what a
// process death leaves behind). A FRESH engine over the same durable state
// (dataDir + workspace + cortex) boots, ResumeOrphanedPlans picks the orphan
// up, and the plan completes having dispatched ONLY t2 — t1 is never re-run
// and the planner is never re-invoked (the durable plan carries the resume).
func TestCodydKillMidPlanResumesAtNextTask(t *testing.T) {
	workspaceRoot := t.TempDir()
	dataDir := t.TempDir()
	ctx := openCortex(t, t.TempDir())

	// --- engine #1: t1 lands, t2 wedges ------------------------------------
	blockT2 := make(chan struct{})
	gw1 := llmtest.NewServer(t, gatewayScript(t, blockT2))
	t.Cleanup(gw1.Close)

	engine1 := newEngine(t, workspaceRoot, dataDir, gw1.URL, ctx)
	// LIFO: close(blockT2) runs FIRST so the wedged worker unwinds, then
	// engine1.Close cancels + waits, then gw1 closes.
	t.Cleanup(engine1.Close)
	t.Cleanup(func() { close(blockT2) })

	runID1, fresh, err := engine1.Submit("conv-kill", "seed the demo workspace", "")
	if err != nil || !fresh {
		t.Fatalf("submit: %v fresh=%v", err, fresh)
	}
	// Wait until t1 is accepted (t2 is wedged mid-flight).
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("t1 never accepted")
		}
		events, err := engine1.trace.load(runID1)
		accepted := false
		if err == nil {
			for _, ev := range events {
				if ev.Type == "task.accepted" && ev.Fields["task_id"] == "t1" {
					accepted = true
				}
			}
		}
		if accepted {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	// The kill: engine #1 is simply abandoned. Its run goroutine is wedged
	// inside the worker; the ledger still says "running" — the exact durable
	// state a hard process death leaves.
	if led, err := engine1.readLedger("conv-kill"); err != nil || led.Status != "running" {
		t.Fatalf("pre-kill ledger = %+v, %v (want running)", led, err)
	}

	// --- engine #2: a fresh codyd over the same durable state --------------
	var plannerCalls, t1Dispatches int32
	gw2 := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		if strings.Contains(system, "planner") {
			atomic.AddInt32(&plannerCalls, 1)
			return llmtest.Say(planJSON)
		}
		if strings.Contains(system, "Cassandra") {
			return llmtest.Say(groundedVerdict)
		}
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
		if strings.Contains(sheetPrompt, "TASK SHEET t1") {
			atomic.AddInt32(&t1Dispatches, 1)
		}
		switch toolResults {
		case 0:
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "reply.txt", "content": "hello back\n"})
		case 1:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "created reply.txt",
				"changes": []map[string]interface{}{{"path": "reply.txt", "kind": "create", "why": "the deliverable"}},
			})
		}
	})
	t.Cleanup(gw2.Close)

	engine2 := newEngine(t, workspaceRoot, dataDir, gw2.URL, ctx)
	t.Cleanup(engine2.Close)
	if n := engine2.ResumeOrphanedPlans(); n != 1 {
		t.Fatalf("ResumeOrphanedPlans = %d, want 1", n)
	}

	// The resumed run reuses the durable run id and completes.
	deadline = time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("resumed run never completed")
		}
		if led, err := engine2.readLedger("conv-kill"); err == nil && led.Status != "running" {
			if led.Status != "completed" {
				t.Fatalf("resumed terminal = %q", led.Status)
			}
			if led.RunID != runID1 {
				t.Fatalf("resumed run id = %q, want the durable %q", led.RunID, runID1)
			}
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Resumed at exactly the correct next task.
	if n := atomic.LoadInt32(&t1Dispatches); n != 0 {
		t.Fatalf("t1 was re-dispatched %d time(s) after resume", n)
	}
	if n := atomic.LoadInt32(&plannerCalls); n != 0 {
		t.Fatalf("planner re-invoked %d time(s); the durable plan must carry the resume", n)
	}
	// Both deliverables are real.
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(filepath.Join(workspaceRoot, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}
}
