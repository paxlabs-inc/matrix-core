// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

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

// TestSelfAuthoringRunsOnBoundedCadence proves the self-authoring consolidation
// pass fires on a BOUNDED cadence through the REAL session death path (self-model
// task 3.2, req.5.2): recording loop deaths via session.recordLoopDeath does NOT
// author a how-I-fail memory every death — it authors only when the configured
// cadence (DeathConsolidateEvery) is reached. No fakes: a real Engine, a real
// cortex pager, the real recordLoopDeath → RecordLoopDeath → ConsolidateDeathJournal
// path.
func TestSelfAuthoringRunsOnBoundedCadence(t *testing.T) {
	e, pager := newRunTestEngine(t, "")
	if e.cfg.DeathConsolidateEvery != 3 {
		t.Fatalf("precondition: default cadence should be 3, got %d", e.cfg.DeathConsolidateEvery)
	}
	s := e.newSession("conv-cadence")
	ctx := context.Background()
	err := fmt.Errorf("%w: kept re-running the same status check without progress", agent.ErrIncomplete)

	// Deaths 1 and 2: below the cadence — nothing authored yet (NOT every turn).
	s.recordLoopDeath(ctx, "run-cadence-1", "ship the migration", 1, err, delegate.ClassNone)
	if got := selfModelPatternCount(t, pager); got != 0 {
		t.Fatalf("after 1 death (< cadence) no pattern must be authored; got %d", got)
	}
	s.recordLoopDeath(ctx, "run-cadence-2", "ship the migration", 2, err, delegate.ClassNone)
	if got := selfModelPatternCount(t, pager); got != 0 {
		t.Fatalf("after 2 deaths (< cadence) no pattern must be authored; got %d", got)
	}

	// Death 3 hits the cadence: the consolidation pass runs and authors the
	// how-I-fail memory from the accumulated journal.
	s.recordLoopDeath(ctx, "run-cadence-3", "ship the migration", 3, err, delegate.ClassNone)
	if got := selfModelPatternCount(t, pager); got != 1 {
		t.Fatalf("at the cadence the pass must author exactly ONE pattern; got %d", got)
	}

	// The counter reset: three more deaths of the same mode reinforce the SAME
	// one belief (no duplicate) at the next cadence tick.
	for i := 4; i <= 6; i++ {
		s.recordLoopDeath(ctx, fmt.Sprintf("run-cadence-%d", i), "ship the migration", i, err, delegate.ClassNone)
	}
	if got := selfModelPatternCount(t, pager); got != 1 {
		t.Fatalf("recurrence must reinforce ONE belief, not duplicate; got %d", got)
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
