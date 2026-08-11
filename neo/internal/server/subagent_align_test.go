// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/memory"
)

// TestSwarmResolvesSharedSelfModelBrief proves the alignment contract's source
// side (self-model task 4.3, req.9.1): the swarm resolves ONE shared self-model
// brief from the shared Neocortex pager — the structural self-summary plus the
// how-I-fail patterns — which each sub-agent then inherits. No fakes: a real
// Engine and a real Neocortex pager seeded with a real structural self + failure
// pattern.
func TestSwarmResolvesSharedSelfModelBrief(t *testing.T) {
	e, pager := newRunTestEngine(t, "")
	ctx := context.Background()

	if _, err := pager.WriteStructuralSelf(ctx, memory.StructuralSelf{
		Summary:      "Neo assembles system, transcript, then a trailing memory tail; core_execute is the value-transfer wall.",
		GraphURI:     "matrix://self-graph/neo",
		Scope:        []string{"neo", "cody", "Neocortex"},
		TokenBudget:  40,
		ContextLimit: 256000,
	}); err != nil {
		t.Fatalf("WriteStructuralSelf: %v", err)
	}
	if _, err := pager.WriteFailureLesson(ctx, memory.FailureLesson{
		Statement:   "[failure-mode:no_progress_stall] I tend to die by repeating the same step (seen 3 times).",
		DerivedFrom: []string{"matrix://Neocortex/Event/d#1", "matrix://Neocortex/Event/d#2"},
		Scope:       "no-progress tool loops", Usefulness: "require a strategy change after a repeated fingerprint",
		Confirmed: true, ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("WriteFailurePattern: %v", err)
	}

	brief := e.subagentSelfModelBrief(ctx)
	if brief == "" {
		t.Fatal("the swarm must resolve a shared self-model brief for its sub-agents to inherit")
	}
	if !strings.Contains(brief, "core_execute is the value-transfer wall") {
		t.Errorf("brief must carry the structural self-summary:\n%s", brief)
	}
	if !strings.Contains(brief, "no_progress_stall") {
		t.Errorf("brief must carry the how-I-fail patterns:\n%s", brief)
	}
	if !strings.Contains(strings.ToLower(brief), "avoid") {
		t.Errorf("brief must frame the patterns as things to avoid:\n%s", brief)
	}
}

// TestSwarmSelfModelBriefEmptyWhenUnpopulated proves the sub-agent never blocks
// on the self-model: before any structural self / failure pattern is written,
// the brief is empty and a sub-agent simply runs on its persona + bounds.
func TestSwarmSelfModelBriefEmptyWhenUnpopulated(t *testing.T) {
	e, _ := newRunTestEngine(t, "")
	if brief := e.subagentSelfModelBrief(context.Background()); brief != "" {
		t.Errorf("an unpopulated self-model must yield an empty brief, got:\n%s", brief)
	}
}
