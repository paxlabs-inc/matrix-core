// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"
	"testing"

	"matrix/cortex"
	"matrix/neo/internal/config"
	neomemory "matrix/neo/internal/memory"
)

func TestSupervisorResumeGuidanceNeverBecomesUserEvidence(t *testing.T) {
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "resume-input-test"
	cfg.AgentRuntime = "legacy"
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatalf("Open pager: %v", err)
	}
	defer pager.Close()

	const (
		conv      = "conversation-7"
		intent    = "run-42"
		objective = "Ship the schema migration"
		guidance  = "Continue from the saved checkpoint; do not repeat completed work."
	)
	first := New(Options{Config: cfg, Pager: pager, ConvID: conv})
	first.SetRunIdentity(intent, 1)
	first.turn = newTurn()
	first.turn.objective = objective
	first.turn.attempt = 1
	if _, err := first.prepareTurn(context.Background(), objective, "", ""); err != nil {
		t.Fatalf("prepare original turn: %v", err)
	}

	resumed := New(Options{Config: cfg, Pager: pager, ConvID: conv})
	resumed.SetRunIdentity(intent, 2)
	resumed.turn = newTurn()
	resumed.turn.objective = objective
	resumed.turn.attempt = 2
	resumed.turn.inputOrigin = originSupervisorResume
	if _, err := resumed.prepareTurn(context.Background(), objective, "", guidance); err != nil {
		t.Fatalf("prepare resumed turn: %v", err)
	}

	transcript, err := pager.Transcript(conv, 0, 0)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	userMessages := 0
	for _, message := range transcript {
		if message.Role == cortex.RoleUser {
			userMessages++
			if message.Content != objective {
				t.Fatalf("durable user message = %q", message.Content)
			}
		}
		if strings.Contains(message.Content, guidance) {
			t.Fatalf("resume guidance leaked into durable transcript: %#v", message)
		}
	}
	if userMessages != 1 {
		t.Fatalf("durable user message count = %d, want 1 across respawn", userMessages)
	}
	if len(resumed.working) != 1 || !resumed.working[0].IsGuidance() ||
		!strings.Contains(resumed.working[0].Content, guidance) {
		t.Fatalf("resumed working input = %#v, want one guidance message", resumed.working)
	}
}
