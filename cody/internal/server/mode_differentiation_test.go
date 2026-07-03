// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/cody/internal/llmtest"
)

const oneTaskPlanJSON = `{
  "goal": "add the greeting file",
  "tasks": [
    {"id": "t1", "title": "Create greet.txt", "goal": "greet.txt exists containing 'hello cody'",
     "acceptance": ["greet.txt contains hello cody"], "wave": 1,
     "verify": ["grep -q \"hello cody\" greet.txt"], "deliverable": {"shape": "greet.txt"}}
  ]
}`

// modeProbe records what one mode's run actually exhibited: the mode policy
// the worker was handed, whether verify-before-done gated a premature done
// claim, and the user-facing final message.
type modeProbe struct {
	mu           sync.Mutex
	workerSystem string
	refusalSeen  bool
	finalText    string
}

// modeScript drives the SAME seeded task in any mode with an initially
// non-conforming worker: it claims done while verification is RED (nothing
// written yet). The worker ENGINE must refuse (verify-before-done is
// structural, not prompted) before the worker does the real work — proving
// the constitution binds regardless of the mode policy in play.
func modeScript(t *testing.T, probe *modeProbe) func(step int, req llmtest.Request) llmtest.Turn {
	return func(step int, req llmtest.Request) llmtest.Turn {
		system := ""
		if len(req.Messages) > 0 {
			system = req.Messages[0].Content
		}
		switch {
		case strings.Contains(system, "planner"):
			return llmtest.Say(oneTaskPlanJSON)
		case strings.Contains(system, "decision adjudicator"):
			return llmtest.Say(`{"pick": 0, "rationale": "first valid candidate"}`)
		case strings.Contains(system, "Cassandra"):
			return llmtest.Say(groundedVerdict)
		}
		// The worker loop.
		probe.mu.Lock()
		probe.workerSystem = system
		probe.mu.Unlock()
		toolResults := 0
		lastTool := ""
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults++
				lastTool = m.Content
			}
		}
		if strings.Contains(lastTool, "turn_in refused") {
			probe.mu.Lock()
			probe.refusalSeen = true
			probe.mu.Unlock()
		}
		switch toolResults {
		case 0: // verify first: RED, the file does not exist yet
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		case 1: // the premature done claim the engine must refuse
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "all done",
			})
		case 2: // post-refusal: the real work
			return llmtest.CallTool("fs_write", map[string]interface{}{"path": "greet.txt", "content": "hello cody\n"})
		case 3:
			return llmtest.CallTool("verify_run", map[string]interface{}{})
		default:
			return llmtest.CallTool("turn_in", map[string]interface{}{
				"status": "done", "summary": "created greet.txt",
				"changes": []map[string]interface{}{{"path": "greet.txt", "kind": "create", "why": "the deliverable"}},
			})
		}
	}
}

// runSeededTaskInMode executes the seeded task through the REAL engine in one
// mode and returns the probe plus the workspace root.
func runSeededTaskInMode(t *testing.T, modeName string) (*modeProbe, string) {
	t.Helper()
	workspaceRoot := t.TempDir()
	// Architect is greenfield-non-prototype and would otherwise trip the Stack
	// Decision Record gate; seed a project marker so this test stays focused on
	// mode differentiation (the SDR round-trip is proven in decision_test.go).
	seedExistingProject(t, workspaceRoot)
	probe := &modeProbe{}
	gw := llmtest.NewServer(t, modeScript(t, probe))
	t.Cleanup(gw.Close)

	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)

	convID := "conv-mode-" + modeName
	runID, fresh, err := engine.Submit(convID, "add the greeting file", modeName, "", "", "")
	if err != nil || !fresh {
		t.Fatalf("submit(%s): %v fresh=%v", modeName, err, fresh)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("%s run never completed", modeName)
		}
		if led, err := engine.readLedger(convID); err == nil && led.Status != "running" {
			if led.Status != "completed" {
				t.Fatalf("%s terminal = %q", modeName, led.Status)
			}
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	events, _, cancel := engine.broker.subscribe(runID, 0)
	cancel()
	for _, ev := range events {
		if ev.Type == "chat.assistant" {
			if text, ok := ev.Fields["text"].(string); ok {
				probe.mu.Lock()
				probe.finalText = text
				probe.mu.Unlock()
			}
		}
	}
	// The work is real in both modes.
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "greet.txt"))
	if err != nil || string(data) != "hello cody\n" {
		t.Fatalf("%s: greet.txt = %q, %v", modeName, data, err)
	}
	return probe, workspaceRoot
}

// TestModeDifferentiationUnderOneConstitution is Property 5: the SAME seeded
// task under Prototype vs Architect exhibits the policy differences — plan
// depth (inline vs durable in-workspace spec files), verify cadence
// (milestones vs per-task property tests), explanation register (outcome vs
// technical) — while BOTH hold the constitution: the worker engine refuses
// the premature done claim (verify-before-done) in each mode identically.
func TestModeDifferentiationUnderOneConstitution(t *testing.T) {
	proto, protoRoot := runSeededTaskInMode(t, "prototype")
	arch, archRoot := runSeededTaskInMode(t, "architect")

	// --- the constitution held in BOTH modes ------------------------------
	for name, probe := range map[string]*modeProbe{"prototype": proto, "architect": arch} {
		if !probe.refusalSeen {
			t.Fatalf("%s: the premature done claim was never refused — verify-before-done did not gate", name)
		}
		if !strings.Contains(probe.workerSystem, "The constitution binds identically in every mode") {
			t.Fatalf("%s: the sheet's mode policy lost the constitution clause:\n%s", name, probe.workerSystem)
		}
	}

	// --- plan depth: inline vs durable spec files -------------------------
	if _, err := os.Stat(filepath.Join(protoRoot, ".cody", "spec")); !os.IsNotExist(err) {
		t.Fatalf("prototype persisted spec files (%v); planning must stay inline", err)
	}
	for _, f := range []string{"requirements.md", "tasks.md"} {
		if _, err := os.Stat(filepath.Join(archRoot, ".cody", "spec", f)); err != nil {
			t.Fatalf("architect missing durable spec file %s: %v", f, err)
		}
	}

	// --- verify cadence + planning register in the worker's policy --------
	if !strings.Contains(proto.workerSystem, "Active mode: prototype") ||
		!strings.Contains(proto.workerSystem, "terse and inline") ||
		!strings.Contains(proto.workerSystem, "milestones") {
		t.Fatalf("prototype worker policy = %q", proto.workerSystem)
	}
	if !strings.Contains(arch.workerSystem, "Active mode: architect") ||
		!strings.Contains(arch.workerSystem, "durable spec files") ||
		!strings.Contains(arch.workerSystem, "property-style tests") {
		t.Fatalf("architect worker policy = %q", arch.workerSystem)
	}

	// --- explanation register: outcome vs technical -----------------------
	if !strings.Contains(proto.finalText, "ready to use") {
		t.Fatalf("prototype final message is not outcome-register: %q", proto.finalText)
	}
	if strings.Contains(proto.finalText, "grep -q") {
		t.Fatalf("prototype final message leaked technical detail: %q", proto.finalText)
	}
	if !strings.Contains(arch.finalText, "independently verified") ||
		!strings.Contains(arch.finalText, "grep -q") {
		t.Fatalf("architect final message is not technical-register: %q", arch.finalText)
	}
}
