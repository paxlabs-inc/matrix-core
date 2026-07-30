// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"
)

// TestDeathJournalIsFirstClassAndExcludedFromAmbientRecall proves the DURABLE death-journal
// read path (self-model task 3.1, req.4.2) over REAL cortex: a recorded death is
// (a) readable as a first-class journal via DeathJournal (the set the
// self-authoring pass and the observability surface read) AND (b) excluded from
// ordinary memory recall. No
// fakes: a real cortex store under t.TempDir(), the real RecordLoopDeath write,
// and the real DeathJournal / RecallHits reads.
func TestDeathJournalIsFirstClassAndExcludedFromAmbientRecall(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const summary = `Loop death (attempt 2, class=none): objective "ship the migration" did not finish. Where it got stuck: kept re-running the same status check without editing a file [loop-state: reason=no_progress_stall faculty=conversation last_tool=web_search repeats=4 distinct_tools=1 context_fill=12% steps=6]`
	if _, err := p.RecordLoopDeath(ctx, summary, "conv-death-journal"); err != nil {
		t.Fatalf("RecordLoopDeath: %v", err)
	}

	// (a) First-class journal read: DeathJournal returns the record as a member
	// of the death journal, with the full summary preserved.
	journal, err := p.DeathJournal(ctx, 10)
	if err != nil {
		t.Fatalf("DeathJournal: %v", err)
	}
	if len(journal) != 1 {
		t.Fatalf("DeathJournal returned %d records, want 1", len(journal))
	}
	if journal[0].Summary != summary {
		t.Errorf("DeathJournal summary not preserved:\n got: %q\nwant: %q", journal[0].Summary, summary)
	}
	if !strings.Contains(strings.ToLower(journal[0].URI), "/session/neo-death-journal/") {
		t.Errorf("death record must use the derived journal lane; uri = %q", journal[0].URI)
	}
	if journal[0].IntentID != "conv-death-journal" {
		t.Errorf("intent identity not preserved: %q", journal[0].IntentID)
	}
	if journal[0].CreatedAt.IsZero() {
		t.Error("death record must carry a real creation time")
	}

	// (b) Ordinary memory recall excludes the derived failure journal.
	hits, err := p.RecallHits(ctx, "", []string{"event"}, 10, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("death journal leaked into ordinary recall: hits=%v", hits)
	}
}

// TestDeathJournalNewestFirstAndBounded proves the journal is ordered newest-
// first (a respawn cares most about the freshest death) and honors the limit.
func TestDeathJournalNewestFirstAndBounded(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	summaries := []string{
		`Loop death (attempt 1, class=none): objective "task A" did not finish. Where it got stuck: first`,
		`Loop death (attempt 1, class=none): objective "task B" did not finish. Where it got stuck: second`,
		`Loop death (attempt 1, class=none): objective "task C" did not finish. Where it got stuck: third`,
	}
	for _, s := range summaries {
		if _, err := p.RecordLoopDeath(ctx, s, "conv-order"); err != nil {
			t.Fatalf("RecordLoopDeath: %v", err)
		}
	}

	journal, err := p.DeathJournal(ctx, 2)
	if err != nil {
		t.Fatalf("DeathJournal: %v", err)
	}
	if len(journal) != 2 {
		t.Fatalf("limit=2 returned %d records", len(journal))
	}
	// ULID-backed cortex Versions are monotonic in creation order, so the two
	// returned entries are the two most recent writes, newest first.
	if !strings.Contains(journal[0].Summary, "task C") {
		t.Errorf("newest death must sort first; got %q", journal[0].Summary)
	}
	if !strings.Contains(journal[1].Summary, "task B") {
		t.Errorf("second-newest death must sort second; got %q", journal[1].Summary)
	}
}
