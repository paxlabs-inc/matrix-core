// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"
)

// TestDeathJournalIsFirstClassAndStillRecallable proves the DURABLE death-journal
// read path (self-model task 3.1, req.4.2) over REAL cortex: a recorded death is
// (a) readable as a first-class journal via DeathJournal (the set the
// self-authoring pass and the observability surface read) AND (b) still surfaced
// by ordinary recall as an Event (the tag is additive, never excluding it). No
// fakes: a real cortex store under t.TempDir(), the real RecordLoopDeath write,
// and the real DeathJournal / RecallHits reads.
func TestDeathJournalIsFirstClassAndStillRecallable(t *testing.T) {
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
	if !strings.Contains(strings.ToLower(journal[0].URI), "event") {
		t.Errorf("death record must be a cortex Event; uri = %q", journal[0].URI)
	}
	if journal[0].CreatedAt.IsZero() {
		t.Error("death record must carry a real creation time")
	}

	// (b) Ordinary recall STILL surfaces the death as an Event (req.4.2): the
	// death-journal tag did not exclude it from the recallable Event set the way
	// the opportunity tag excludes the proactive queue.
	hits, err := p.RecallHits(ctx, "", []string{"event"}, 10, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	found := false
	for _, h := range hits {
		if strings.Contains(h.Text, "ship the migration") && strings.Contains(h.Text, "no_progress_stall") {
			found = true
			if strings.ToLower(h.Type) != "event" {
				t.Errorf("recalled death record type = %q, want event", h.Type)
			}
		}
	}
	if !found {
		t.Fatalf("a tagged death record must still be surfaced by ordinary recall; hits=%v", hits)
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
