// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// TestRealStallProducesStructuredDeathRecord drives the REAL agent.Chat loop
// into a genuine no-progress stall (the scripted model repeats one tool call
// every step) and proves the rich death capture (self-model task 2.2, req.3.1):
// LastDeath returns a structured record whose loop-state fields are populated
// from the ACTUAL agent state at death — not placeholders. No fakes: the model
// is scripted over HTTP, but the Chat loop, the stall detector, the death
// capture, and the summary rendering are all real code paths.
func TestRealStallProducesStructuredDeathRecord(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := spiralServer(t, &calls, &mu)

	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	cfg := config.Default()
	// Disable the Cassandra controller so the raw stall path is exercised
	// (the death capture is independent of the controller either way).
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-death-capture"

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	a := New(Options{
		Config: cfg,
		Main:   client,
		Tools:  &tools.Manager{},
		Pager:  pager,
		ConvID: "conv-death-capture",
	})

	const objective = "keep searching for the same query over and over"
	err = a.Chat(context.Background(), objective)
	if err == nil || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a spiral must terminate with ErrIncomplete, got: %v", err)
	}

	death, ok := a.LastDeath()
	if !ok {
		t.Fatal("a real loop-affecting death must produce a structured record via LastDeath")
	}

	// The death reason is the real no-progress stall class.
	if death.Reason != DeathReasonStall {
		t.Errorf("death reason = %q, want %q", death.Reason, DeathReasonStall)
	}
	// The last tool is the real repeated tool the scripted model emitted.
	if death.LastTool != "web_search" {
		t.Errorf("last tool = %q, want web_search", death.LastTool)
	}
	// The repeat count reflects the real stall bound reached (NoProgressStall).
	if death.RepeatCount < cfg.NoProgressStall {
		t.Errorf("repeat count = %d, want >= NoProgressStall (%d)", death.RepeatCount, cfg.NoProgressStall)
	}
	// The steps used are real loop iterations, bounded well under the step budget.
	if death.StepsUsed <= 0 || death.StepsUsed >= cfg.StepBudget {
		t.Errorf("steps used = %d, want in (0, %d)", death.StepsUsed, cfg.StepBudget)
	}
	// The faculty is the conversation faculty (top-level Neo, no persona).
	if death.Faculty != "conversation" {
		t.Errorf("faculty = %q, want conversation", death.Faculty)
	}
	// The objective is the real active goal, not a placeholder.
	if !strings.Contains(death.Objective, "keep searching") {
		t.Errorf("objective not captured from real state: %q", death.Objective)
	}
	// The recent-signature window shows the shape of the spiral.
	if len(death.RecentSignatures) == 0 {
		t.Error("recent signatures must capture the spiral shape")
	}
	// Context fill is a real fraction in range.
	if death.ContextFillPct < 0 || death.ContextFillPct > 100 {
		t.Errorf("context fill = %d, out of range", death.ContextFillPct)
	}

	// The rendered state line carries the fields for the durable death-journal
	// entry (req.3.1) — the record CARRIES the loop state, not just a sentence.
	line := death.StateLine()
	for _, want := range []string{"reason=no_progress_stall", "faculty=conversation", "last_tool=web_search", "repeats="} {
		if !strings.Contains(line, want) {
			t.Errorf("state line missing %q:\n%s", want, line)
		}
	}
}

// TestLastDeathAbsentOnCleanTurn proves the capture is scoped to a real
// loop-affecting death: a turn that does not stall/exhaust leaves LastDeath
// empty (no false death record).
func TestLastDeathAbsentOnCleanTurn(t *testing.T) {
	a := &Agent{turn: newTurn()}
	if _, ok := a.LastDeath(); ok {
		t.Fatal("LastDeath must be absent before any death")
	}
}
