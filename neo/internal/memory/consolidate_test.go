// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	cmem "matrix/cortex/memory"
	"matrix/cortex/query"
)

// countType returns how many live memories of type t the pager holds.
func countType(t *testing.T, p *Pager, mt cmem.Type) int {
	t.Helper()
	res, err := p.cortex.Find(query.Query{Type: []cmem.Type{mt}, Limit: 512})
	if err != nil {
		t.Fatalf("Find(%s): %v", mt, err)
	}
	if res == nil {
		return 0
	}
	return len(res.Memories)
}

// TestConsolidationIsPureMemorySideChannel proves the self-authoring pass is a
// pure observability + reasoning side-channel (self-model task 3.2, req.5.4): it
// READS death Events and WRITES only self-model Belief memories — it creates no
// Goal/opportunity, no Pattern, and does not mutate or delete the death Events it
// read. It has no access to signing or plan/walk (it is a cortex-memory method),
// so the D11 replay byte-identity invariant cannot be perturbed by it.
func TestConsolidationIsPureMemorySideChannel(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		s := deathSummary(i, "ship the migration", "re-ran the same check", "no_progress_stall", "web_search")
		if _, err := p.RecordLoopDeath(ctx, s, fmt.Sprintf("run-pure-%d", i)); err != nil {
			t.Fatalf("RecordLoopDeath: %v", err)
		}
	}

	eventsBefore := countType(t, p, cmem.TypeEvent)
	beliefsBefore := countType(t, p, cmem.TypeBelief)
	goalsBefore := countType(t, p, cmem.TypeGoal)
	patternsBefore := countType(t, p, cmem.TypePattern)

	if _, err := p.ConsolidateDeathJournal(ctx); err != nil {
		t.Fatalf("ConsolidateDeathJournal: %v", err)
	}

	// The death Events it READ are untouched (not mutated or deleted).
	if got := countType(t, p, cmem.TypeEvent); got != eventsBefore {
		t.Errorf("consolidation must not touch the death Events it reads: %d → %d", eventsBefore, got)
	}
	// It WROTE exactly the self-model Belief(s).
	if got := countType(t, p, cmem.TypeBelief); got != beliefsBefore+1 {
		t.Errorf("consolidation must write exactly one Belief: %d → %d", beliefsBefore, got)
	}
	// It created NO Goal/opportunity and NO Pattern — no plan/walk or procedural
	// artifacts, no signing surface.
	if got := countType(t, p, cmem.TypeGoal); got != goalsBefore {
		t.Errorf("consolidation must create no Goal artifacts: %d → %d", goalsBefore, got)
	}
	if got := countType(t, p, cmem.TypePattern); got != patternsBefore {
		t.Errorf("consolidation must create no Pattern artifacts: %d → %d", patternsBefore, got)
	}
}

// deathSummary builds a durable death-journal summary in the exact shape
// session.recordLoopDeath writes (prefix + rich loop-state suffix), so the
// consolidation pass parses the real format — not a fabricated one.
func deathSummary(attempt int, objective, digest, reason, lastTool string) string {
	return fmt.Sprintf(
		"Loop death (attempt %d, class=none): objective %q did not finish. Where it got stuck: %s [loop-state: reason=%s faculty=conversation last_tool=%s repeats=4 distinct_tools=1 context_fill=18%% steps=6]",
		attempt, objective, digest, reason, lastTool,
	)
}

// countPatternsForMode returns how many self-authored failure-pattern beliefs
// carry the given mode marker.
func countPatternsForMode(patterns []FailurePattern, mode string) (int, FailurePattern) {
	marker := failureModeMarkerPrefix + mode + failureModeMarkerSuffix
	n := 0
	var found FailurePattern
	for _, fp := range patterns {
		if strings.Contains(fp.Statement, marker) {
			n++
			found = fp
		}
	}
	return n, found
}

// TestSelfAuthoringConsolidatesSameModeIntoOneReinforcedMemory proves the
// self-authoring pass (self-model task 3.2, req.5.1/5.2/5.3/5.5) over REAL
// cortex: repeated deaths of the SAME mode consolidate into ONE reinforced
// failure-pattern belief (never N duplicates), a distinct mode gets its own
// belief, and the authored pattern is recallable and carries an actionable
// lesson. No fakes: real RecordLoopDeath writes, the real DeathJournal read, and
// real cortex Belief writes/updates.
func TestSelfAuthoringConsolidatesSameModeIntoOneReinforcedMemory(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	// Three distinct logical tasks died by the SAME mode.
	for i := 1; i <= 3; i++ {
		s := deathSummary(i, "ship the schema migration", "re-ran the same status check without editing a file", "no_progress_stall", "web_search")
		if _, err := p.RecordLoopDeath(ctx, s, fmt.Sprintf("run-consolidate-%d", i)); err != nil {
			t.Fatalf("RecordLoopDeath: %v", err)
		}
	}

	n, err := p.ConsolidateDeathJournal(ctx)
	if err != nil {
		t.Fatalf("ConsolidateDeathJournal: %v", err)
	}
	if n != 1 {
		t.Fatalf("three same-mode deaths must author exactly ONE pattern, got %d", n)
	}

	model, err := p.SelfModel(ctx)
	if err != nil {
		t.Fatalf("SelfModel: %v", err)
	}
	count, fp := countPatternsForMode(model.FailurePatterns, "no_progress_stall")
	if count != 1 {
		t.Fatalf("want exactly 1 no_progress_stall pattern, got %d: %#v", count, model.FailurePatterns)
	}
	if !strings.Contains(fp.Statement, "seen 3 times") {
		t.Errorf("consolidated pattern must count the recurrences; got %q", fp.Statement)
	}
	// It carries an actionable, first-person lesson usable at reasoning time.
	if !strings.Contains(strings.ToLower(fp.Statement), "change tactic") {
		t.Errorf("pattern must carry an actionable lesson; got %q", fp.Statement)
	}
	// The supporting deaths are recorded as evidence.
	if len(fp.DerivedFrom) != 3 {
		t.Errorf("pattern must cite its 3 supporting deaths; got %d", len(fp.DerivedFrom))
	}

	// Two MORE same-mode deaths plus one of a DIFFERENT mode (step_budget).
	for i := 4; i <= 5; i++ {
		s := deathSummary(i, "ship the schema migration", "re-ran the same status check without editing a file", "no_progress_stall", "web_search")
		if _, err := p.RecordLoopDeath(ctx, s, fmt.Sprintf("run-consolidate-%d", i)); err != nil {
			t.Fatalf("RecordLoopDeath: %v", err)
		}
	}
	for i := 1; i <= 3; i++ {
		if _, err := p.RecordLoopDeath(ctx, deathSummary(i, "write the whole paper in one pass", "ran out of steps at section 3 of 8", "step_budget", "write_file"), fmt.Sprintf("run-budget-%d", i)); err != nil {
			t.Fatalf("RecordLoopDeath: %v", err)
		}
	}

	if _, err := p.ConsolidateDeathJournal(ctx); err != nil {
		t.Fatalf("ConsolidateDeathJournal (2nd): %v", err)
	}
	model, err = p.SelfModel(ctx)
	if err != nil {
		t.Fatalf("SelfModel (2nd): %v", err)
	}

	// STILL exactly one no_progress_stall belief — the recurrence REINFORCED the
	// one belief (now seen 5 times) rather than writing a duplicate (req.5.2).
	count, fp = countPatternsForMode(model.FailurePatterns, "no_progress_stall")
	if count != 1 {
		t.Fatalf("recurrence must reinforce ONE belief, not duplicate; got %d no_progress_stall patterns", count)
	}
	if !strings.Contains(fp.Statement, "seen 5 times") {
		t.Errorf("reinforced pattern must reflect the higher recurrence count; got %q", fp.Statement)
	}
	// The distinct mode authored its OWN belief.
	if c, _ := countPatternsForMode(model.FailurePatterns, "step_budget"); c != 1 {
		t.Fatalf("a distinct mode must author its own pattern; got %d step_budget patterns", c)
	}

	// Recallable and usable at reasoning time (req.5.3): ordinary recall over the
	// belief type surfaces the authored how-I-fail pattern.
	recalled, err := p.Recall(ctx, "", []string{"belief"}, 16, nil)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.Contains(recalled, "failure-mode:no_progress_stall") {
		t.Fatalf("the authored failure pattern must be recallable:\n%s", recalled)
	}
}

func TestRepeatedRespawnsOfOneIntentStayOneIncident(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	for attempt := 1; attempt <= 4; attempt++ {
		summary := deathSummary(attempt, "ship the schema migration", "same task retry", "no_progress_stall", "web_search")
		if _, err := p.RecordLoopDeath(ctx, summary, "run-one-task"); err != nil {
			t.Fatalf("RecordLoopDeath: %v", err)
		}
	}
	written, err := p.ConsolidateDeathJournal(ctx)
	if err != nil {
		t.Fatalf("ConsolidateDeathJournal: %v", err)
	}
	if written != 0 {
		t.Fatalf("four respawns of one task must not become a cross-task failure pattern; wrote %d", written)
	}
}
