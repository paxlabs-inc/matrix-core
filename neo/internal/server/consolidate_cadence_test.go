// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/memory"
)

// selfModelPatternCount returns how many self-model failure-pattern beliefs the
// pager currently holds (the how-I-fail memories authored by the consolidation
// pass).
func selfModelPatternCount(t *testing.T, p *memory.Pager) int {
	t.Helper()
	model, err := p.SelfModel(context.Background())
	if err != nil {
		t.Fatalf("SelfModel: %v", err)
	}
	n := 0
	for _, fp := range model.FailurePatterns {
		if strings.Contains(fp.Statement, "[failure-mode:") {
			n++
		}
	}
	return n
}

// TestLoopDeathsNeverAutoPromoteIntoCognitiveMemory proves diagnostics remain
// in the death journal even if an obsolete cadence value is configured.
func TestLoopDeathsNeverAutoPromoteIntoCognitiveMemory(t *testing.T) {
	e, pager := newRunTestEngine(t, "")
	e.cfg.DeathConsolidateEvery = 3
	s := e.newSession("conv-cadence")
	ctx := context.Background()
	err := fmt.Errorf("%w: kept re-running the same status check without progress", agent.ErrIncomplete)

	for i := 1; i <= 6; i++ {
		s.recordLoopDeath(ctx, fmt.Sprintf("run-cadence-%d", i), "ship the migration", i, err, delegate.ClassNone)
	}
	if got := selfModelPatternCount(t, pager); got != 0 {
		t.Fatalf("loop deaths must not auto-promote into failure beliefs; got %d", got)
	}
	journal, journalErr := pager.DeathJournal(ctx, 10)
	if journalErr != nil || len(journal) != 6 {
		t.Fatalf("diagnostic death journal = %d, %v; want 6", len(journal), journalErr)
	}
	if promoted, err := pager.ConsolidateDeathJournal(ctx); err != nil || promoted != 0 {
		t.Fatalf("obsolete death consolidation promoted=%d err=%v", promoted, err)
	}
}

func TestFailureLessonsRequireConfirmationEvidenceScopeAndExpiry(t *testing.T) {
	_, pager := newRunTestEngine(t, "")
	ctx := context.Background()
	if _, err := pager.WriteFailurePattern(ctx, "legacy bypass", []string{"event:1"}); err == nil {
		t.Fatal("legacy unconfirmed failure-pattern bypass remained writable")
	}
	lesson := memory.FailureLesson{
		Statement:   "Use a different read strategy after a stable missing-path fingerprint.",
		DerivedFrom: []string{"event:1"}, Scope: "filesystem reads",
		Usefulness: "avoid repeating deterministic misses", Confirmed: true,
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	}
	if _, err := pager.WriteFailureLesson(ctx, lesson); err == nil {
		t.Fatal("single-source lesson passed the independent-evidence gate")
	}
	lesson.DerivedFrom = append(lesson.DerivedFrom, "event:2")
	uri, err := pager.WriteFailureLesson(ctx, lesson)
	if err != nil {
		t.Fatal(err)
	}
	model, err := pager.SelfModel(ctx)
	if err != nil || len(model.FailurePatterns) != 1 {
		t.Fatalf("confirmed lesson model=%+v err=%v", model, err)
	}
	if err := pager.RetireFailurePattern(ctx, uri); err != nil {
		t.Fatal(err)
	}
	model, err = pager.SelfModel(ctx)
	if err != nil || len(model.FailurePatterns) != 0 {
		t.Fatalf("retired lesson remained active: %+v err=%v", model, err)
	}
}

// TestSelfAuthoringDisabledAtZeroCadence proves DeathConsolidateEvery <= 0
// disables the pass entirely: deaths are still journaled durably, but no
// how-I-fail memory is authored.
func TestSelfAuthoringDisabledAtZeroCadence(t *testing.T) {
	e, pager := newRunTestEngine(t, "")
	e.cfg.DeathConsolidateEvery = 0
	s := e.newSession("conv-cadence-off")
	ctx := context.Background()
	err := fmt.Errorf("%w: kept re-running the same status check without progress", agent.ErrIncomplete)

	for i := 1; i <= 5; i++ {
		s.recordLoopDeath(ctx, fmt.Sprintf("run-cadence-off-%d", i), "ship the migration", i, err, delegate.ClassNone)
	}
	if got := selfModelPatternCount(t, pager); got != 0 {
		t.Fatalf("cadence 0 must disable authoring; got %d patterns", got)
	}
	// The deaths were still journaled durably (the read path is unaffected).
	journal, err2 := pager.DeathJournal(ctx, 10)
	if err2 != nil {
		t.Fatalf("DeathJournal: %v", err2)
	}
	if len(journal) != 5 {
		t.Fatalf("all 5 deaths must still be journaled; got %d", len(journal))
	}
}
