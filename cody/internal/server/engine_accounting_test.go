// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"matrix/cody/internal/llmtest"
	"matrix/cody/internal/mode"
)

// TestEngineAccountingMemoryTitle proves three cross-cutting engine behaviors
// over a real plan run against the scripted gateway: (a) token accounting is
// folded into both the plan.completed event and the durable conversation
// ledger, (b) accepted turn-ins are recorded to cortex-backed project memory
// and render back through recallProjectMemory, and (c) the async small-model
// title upgrade lands in the ledger when TitleModel is set.
func TestEngineAccountingMemoryTitle(t *testing.T) {
	workspaceRoot := t.TempDir()
	seedExistingProject(t, workspaceRoot)
	dataDir := t.TempDir()
	gw := llmtest.NewServer(t, gatewayScript(t, nil))
	t.Cleanup(gw.Close)

	ctx := openCortex(t, t.TempDir())
	engine, err := NewEngine(EngineOptions{
		WorkspaceRoot:     workspaceRoot,
		DataDir:           dataDir,
		Cortex:            ctx,
		GatewayURL:        gw.URL,
		DefaultMode:       mode.Engineer,
		OrchestratorModel: "accounts/fireworks/models/test-model",
		WorkerModel:       "accounts/fireworks/models/test-model",
		// A non-empty TitleModel arms the async title upgrade; the scripted
		// gateway answers the "title coding conversations" system prompt.
		TitleModel: "accounts/fireworks/models/title-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)

	out := postChat(t, srv.URL, `{"message": "seed the demo workspace", "conversation_id": "conv-acct"}`)
	runID := out["intent_id"].(string)
	if status := waitTerminal(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal status = %q", status)
	}

	events := readEvents(t, srv.URL, runID)

	// (a) plan.completed carries non-zero token accounting.
	var sawUsage bool
	for _, ev := range events {
		if ev.Type != "plan.completed" {
			continue
		}
		usage, ok := ev.Fields["usage"].(map[string]interface{})
		if !ok {
			t.Fatalf("plan.completed missing usage: %+v", ev.Fields)
		}
		total, _ := usage["total_tokens"].(float64)
		if total <= 0 {
			t.Fatalf("plan.completed total_tokens = %v, want > 0", usage["total_tokens"])
		}
		sawUsage = true
	}
	if !sawUsage {
		t.Fatal("no plan.completed event carried usage")
	}

	// (a) the durable ledger accumulates the run's token spend at its terminal.
	led, err := engine.readLedger("conv-acct")
	if err != nil {
		t.Fatal(err)
	}
	if led.Usage.TotalTokens <= 0 {
		t.Fatalf("ledger.Usage.TotalTokens = %d, want > 0", led.Usage.TotalTokens)
	}
	if led.Usage.TotalTokens != led.Usage.PromptTokens+led.Usage.CompletionTokens {
		t.Fatalf("ledger usage not self-consistent: %+v", led.Usage)
	}

	// (b) accepted turn-ins are durable in project memory and render back.
	rendered := engine.recallProjectMemory(defaultProjectID)
	if !strings.Contains(rendered, "created greet.txt") {
		t.Fatalf("project memory missing the accepted delivery:\n%s", rendered)
	}

	// (c) the async title upgrade replaces the first-line fallback.
	deadline := time.Now().Add(20 * time.Second)
	var gotTitle string
	for time.Now().Before(deadline) {
		l, err := engine.readLedger("conv-acct")
		if err != nil {
			t.Fatal(err)
		}
		if l.Title == "Seed The Demo Workspace" {
			gotTitle = l.Title
			break
		}
		gotTitle = l.Title
		time.Sleep(50 * time.Millisecond)
	}
	if gotTitle != "Seed The Demo Workspace" {
		t.Fatalf("async title upgrade never landed: title = %q", gotTitle)
	}
}
