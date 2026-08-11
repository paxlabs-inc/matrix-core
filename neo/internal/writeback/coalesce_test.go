// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package writeback

import (
	"context"
	"fmt"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/consolidation"
	neomemory "matrix/neo/internal/memory"
)

func TestQueueCoalescesRespawnsByStableIntentBeforeCommit(t *testing.T) {
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "writeback-coalesce-test"
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatalf("Open pager: %v", err)
	}
	defer pager.Close()
	if _, err := pager.SetMemoryConsent(context.Background(), true, "test user"); err != nil {
		t.Fatalf("SetMemoryConsent: %v", err)
	}

	consolidator := New(nil, nil, pager, cfg)
	for attempt := 1; attempt <= 3; attempt++ {
		job := consolidation.Job{
			IntentID: "run-42", Attempt: attempt,
			Objective: "ship the migration",
			Evidence: []consolidation.Evidence{{
				Kind: consolidation.EvidenceTool, ToolName: "exec",
				AttemptID: fmt.Sprintf("attempt-%d", attempt),
				Content:   fmt.Sprintf("result-%d", attempt),
			}},
		}
		consolidator.Consolidate(job)
	}
	consolidator.drain(false)
	consolidator.queueMu.Lock()
	if len(consolidator.pending) != 1 || consolidator.pending["run-42"].Attempt != 3 {
		consolidator.queueMu.Unlock()
		t.Fatalf("nonterminal respawns were drained before terminal state: %#v", consolidator.pending)
	}
	consolidator.queueMu.Unlock()

	consolidator.Consolidate(consolidation.Job{
		IntentID: "run-42", Attempt: 4, Terminal: true,
		Objective: "ship the migration", Outcome: "migration shipped",
		Evidence: []consolidation.Evidence{{
			Kind: consolidation.EvidenceTool, ToolName: "exec",
			AttemptID: "attempt-4", Content: "result-4",
		}},
	})

	consolidator.queueMu.Lock()
	defer consolidator.queueMu.Unlock()
	if len(consolidator.pending) != 1 {
		t.Fatalf("pending jobs = %d, want one logical intent", len(consolidator.pending))
	}
	job := consolidator.pending["run-42"]
	if job.Attempt != 4 || !job.Terminal || job.Outcome != "migration shipped" {
		t.Fatalf("coalesced job = %#v", job)
	}
	if len(job.Evidence) != 4 {
		t.Fatalf("coalesced evidence = %d, want four distinct attempts", len(job.Evidence))
	}
}
