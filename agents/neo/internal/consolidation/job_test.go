// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package consolidation

import (
	"fmt"
	"strings"
	"testing"
)

func TestMergeCoalescesRespawnsIntoOneTerminalJob(t *testing.T) {
	var merged Job
	for attempt := 1; attempt <= 4; attempt++ {
		job := Job{
			IntentID:  "run-42",
			Attempt:   attempt,
			Objective: "ship the schema migration",
			Evidence: []Evidence{{
				Kind:      EvidenceTool,
				ToolName:  "exec",
				AttemptID: fmt.Sprintf("attempt-%d", attempt),
				Content:   fmt.Sprintf(`{"exit_code":%d}`, attempt-1),
			}},
		}
		if attempt == 4 {
			job.Terminal = true
			job.Outcome = "migration shipped"
			job.Evidence = append(job.Evidence, Evidence{
				Kind: EvidenceAssistant, Content: job.Outcome,
			})
		}
		merged = Merge(merged, job)
	}

	if merged.IntentID != "run-42" || merged.Attempt != 4 || !merged.Terminal {
		t.Fatalf("merged identity/state = %#v", merged)
	}
	if merged.Outcome != "migration shipped" {
		t.Fatalf("merged outcome = %q", merged.Outcome)
	}
	if len(merged.Evidence) != 5 {
		t.Fatalf("merged evidence count = %d, want 5", len(merged.Evidence))
	}

	again := Merge(merged, Job{
		IntentID: "run-42",
		Attempt:  4,
		Evidence: []Evidence{{
			Kind: EvidenceTool, ToolName: "exec", AttemptID: "attempt-4",
			Content: `{"exit_code":3}`,
		}},
	})
	if len(again.Evidence) != len(merged.Evidence) {
		t.Fatalf("duplicate respawn evidence grew job: %d -> %d", len(merged.Evidence), len(again.Evidence))
	}
}

func TestRenderOnlyMakesEvidenceCitable(t *testing.T) {
	rendered, valid := Render(Job{
		IntentID:  "run-7",
		Objective: "internal objective context",
		Context:   []Message{{Role: "system", Content: "supervisor retry guidance"}},
		Evidence: []Evidence{
			{Kind: EvidenceUser, Content: "Andrew asked for the migration"},
			{Kind: EvidenceTool, Content: "   "},
			{Kind: EvidenceVerified, Content: "migration status is complete"},
		},
	})

	if !strings.Contains(rendered, "[E1] user") || !strings.Contains(rendered, "[E3] verified_outcome") {
		t.Fatalf("rendered evidence IDs missing:\n%s", rendered)
	}
	if strings.Contains(rendered, "[E2]") {
		t.Fatalf("blank evidence became citable:\n%s", rendered)
	}
	if !ValidCitations([]int{1, 3}, valid) {
		t.Fatal("valid evidence citations were rejected")
	}
	if ValidCitations([]int{2}, valid) || ValidCitations(nil, valid) {
		t.Fatal("missing or non-evidence citations were accepted")
	}
}
