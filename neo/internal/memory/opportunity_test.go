// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"

	"matrix/cortex"
	"matrix/cortex/memory"
)

// These tests run against a REAL cortex store under t.TempDir() with the real
// hash embedder — no stubs, no fakes. They exercise the actual
// RememberOpportunity / PendingOpportunities / SetOpportunityStatus code paths
// and the real cortex Write/Update/Find machinery.

func TestOpportunityRecordRoundTrip(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	spec := OpportunitySpec{
		Summary:              "Draft the quarterly update doc you mentioned",
		Rationale:            "user said they need to send a quarterly update next week",
		EligibleAutonomous:   true,
		Confidence:           0.82,
		OriginConversationID: "conv-abc123",
	}
	uri, err := p.RememberOpportunity(ctx, spec)
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	if uri == "" {
		t.Fatal("expected a non-empty URI for a written opportunity")
	}

	got, err := p.PendingOpportunities(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("PendingOpportunities = %d records, want 1", len(got))
	}
	o := got[0]
	if o.Summary != spec.Summary {
		t.Errorf("summary = %q, want %q", o.Summary, spec.Summary)
	}
	if o.Rationale != spec.Rationale {
		t.Errorf("rationale = %q, want %q", o.Rationale, spec.Rationale)
	}
	if o.Status != OpportunityPending {
		t.Errorf("status = %q, want pending (default)", o.Status)
	}
	if !o.EligibleAutonomous {
		t.Error("eligible_autonomous should round-trip true")
	}
	if o.Confidence < 0.81 || o.Confidence > 0.83 {
		t.Errorf("confidence = %v, want ~0.82", o.Confidence)
	}
	if o.OriginConversationID != "conv-abc123" {
		t.Errorf("origin_conversation_id = %q", o.OriginConversationID)
	}
	if o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() {
		t.Error("created_at/updated_at should be stamped on write")
	}
	if o.URI == "" {
		t.Error("URI should be populated on read")
	}
}

func TestOpportunityDedupNormalizeAndSemantic(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	base := OpportunitySpec{Summary: "Summarize the API rate-limit thread", EligibleAutonomous: true, Confidence: 0.7}
	uri1, err := p.RememberOpportunity(ctx, base)
	if err != nil {
		t.Fatalf("RememberOpportunity base: %v", err)
	}

	// Case/spacing drift -> normalized-statement dedup, returns the same URI.
	drift := OpportunitySpec{Summary: "  summarize the API RATE-LIMIT thread ", EligibleAutonomous: true, Confidence: 0.9}
	uri2, err := p.RememberOpportunity(ctx, drift)
	if err != nil {
		t.Fatalf("RememberOpportunity drift: %v", err)
	}
	if uri2 != uri1 {
		t.Errorf("case/spacing drift should dedup to the existing record: got %q want %q", uri2, uri1)
	}

	// Byte-identical summary -> semantic-similarity dedup (cosine 1.0 with the
	// real embedder) also returns the existing record.
	exact := OpportunitySpec{Summary: "Summarize the API rate-limit thread", EligibleAutonomous: true, Confidence: 0.5}
	uri3, err := p.RememberOpportunity(ctx, exact)
	if err != nil {
		t.Fatalf("RememberOpportunity exact: %v", err)
	}
	if uri3 != uri1 {
		t.Errorf("exact repeat should dedup: got %q want %q", uri3, uri1)
	}

	// A genuinely distinct opportunity is NOT merged.
	distinct := OpportunitySpec{Summary: "Book the dentist appointment for next Tuesday", EligibleAutonomous: true, Confidence: 0.6}
	if _, err := p.RememberOpportunity(ctx, distinct); err != nil {
		t.Fatalf("RememberOpportunity distinct: %v", err)
	}

	pending, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected exactly 2 deduped pending opportunities, got %d", len(pending))
	}
}

func TestPendingOpportunitiesFiltersFinancialAndNonPending(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	// Eligible + pending — the only kind the picker may select.
	eligibleURI, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary: "Clean up the stale TODO comments", EligibleAutonomous: true, Confidence: 0.8,
	})
	if err != nil {
		t.Fatalf("remember eligible: %v", err)
	}
	// Financial (not eligible) — captured but NEVER returned by the reader.
	if _, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary: "Swap 100 PAX for USDC at the best rate", EligibleAutonomous: false, Confidence: 0.95,
	}); err != nil {
		t.Fatalf("remember financial: %v", err)
	}

	pending, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("reader must return only eligible+pending; got %d", len(pending))
	}
	if pending[0].EligibleAutonomous != true || strings.Contains(pending[0].Summary, "Swap") {
		t.Errorf("financial opportunity leaked into the autonomous picker set: %+v", pending[0])
	}

	// Transitioning the eligible one out of pending removes it from the reader.
	if err := p.SetOpportunityStatus(ctx, eligibleURI, OpportunityDone); err != nil {
		t.Fatalf("SetOpportunityStatus done: %v", err)
	}
	pending, err = p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities after done: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a done opportunity must not be returned as pending; got %d", len(pending))
	}
}

func TestSetOpportunityStatusLifecycleAtomic(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	uri, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary: "Refactor the config loader", Rationale: "user griped about config sprawl",
		EligibleAutonomous: true, Confidence: 0.6,
	})
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}

	for _, st := range []OpportunityStatus{OpportunityScheduled, OpportunityInProgress, OpportunityDone} {
		if err := p.SetOpportunityStatus(ctx, uri, st); err != nil {
			t.Fatalf("SetOpportunityStatus(%s): %v", st, err)
		}
	}

	// Resolve the current record by id and confirm status + preserved fields.
	_, id, _, perr := cortex.ParseURI(memory.URI(uri))
	if perr != nil {
		t.Fatalf("parse uri: %v", perr)
	}
	mem, err := p.cortex.ResolveLatest(id)
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if mem.Head.CurrentVersion != 4 { // v1 write + 3 updates
		t.Errorf("current version = %d, want 4 (1 write + 3 atomic updates)", mem.Head.CurrentVersion)
	}
	if !headHasOpportunityTag(mem.Head) {
		t.Error("opportunity tag must be preserved across status updates")
	}
	data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	gd, ok := asGoalData(data)
	if !ok {
		t.Fatal("current version is not a Goal record")
	}
	cur := decodeOpportunityGoal(gd, uri)
	if cur.Status != OpportunityDone {
		t.Errorf("status = %q, want done", cur.Status)
	}
	if cur.Rationale != "user griped about config sprawl" {
		t.Errorf("rationale not preserved across updates: %q", cur.Rationale)
	}
	if !cur.EligibleAutonomous {
		t.Error("eligible_autonomous not preserved across updates")
	}

	// Invalid inputs are rejected.
	if err := p.SetOpportunityStatus(ctx, uri, OpportunityStatus("bogus")); err == nil {
		t.Error("an invalid status must be rejected")
	}
	if err := p.SetOpportunityStatus(ctx, "", OpportunityDone); err == nil {
		t.Error("an empty uri must be rejected")
	}
}

func TestPendingOpportunitiesRankedAndLimited(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	// Same recency/salience posture; confidence is the discriminating factor.
	for _, s := range []OpportunitySpec{
		{Summary: "low confidence task", EligibleAutonomous: true, Confidence: 0.3},
		{Summary: "high confidence task", EligibleAutonomous: true, Confidence: 0.95},
		{Summary: "mid confidence task", EligibleAutonomous: true, Confidence: 0.6},
	} {
		if _, err := p.RememberOpportunity(ctx, s); err != nil {
			t.Fatalf("RememberOpportunity %q: %v", s.Summary, err)
		}
	}

	all, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 pending, got %d", len(all))
	}
	if all[0].Summary != "high confidence task" {
		t.Errorf("highest-confidence opportunity should rank first; got %q", all[0].Summary)
	}

	// limit is honored.
	top, err := p.PendingOpportunities(ctx, 1)
	if err != nil {
		t.Fatalf("PendingOpportunities(limit=1): %v", err)
	}
	if len(top) != 1 || top[0].Summary != "high confidence task" {
		t.Errorf("limit=1 should return only the top-ranked item, got %+v", top)
	}
}

// TestOpportunitiesExcludedFromAmbientWindow asserts the proactive queue never
// leaks into the prompt window: a pending opportunity is not surfaced by the
// ambient Retrieve lanes nor by explicit Recall, while an ordinary fact is.
func TestOpportunitiesExcludedFromAmbientWindow(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const oppSummary = "Compile the migration notes into a checklist"
	if _, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary: oppSummary, EligibleAutonomous: true, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	const factText = "the migration runs in three phases"
	if _, err := p.RememberFact(ctx, factText); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}

	// Empty query forces the deterministic type-filtered lanes (no async HNSW
	// dependency). The salience lane scans TypeGoal, so without the exclusion
	// the opportunity WOULD surface here every turn — this proves it does not,
	// while the ordinary fact still does.
	snips, err := p.Retrieve(ctx, "")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	sawFact := false
	for _, s := range snips {
		if strings.Contains(s.Text, oppSummary) {
			t.Errorf("opportunity leaked into ambient Retrieve: %q", s.Text)
		}
		if strings.Contains(s.Text, factText) {
			sawFact = true
		}
	}
	if !sawFact {
		t.Error("expected the ordinary fact to surface in Retrieve (sanity: lane is live)")
	}

	recall, err := p.Recall(ctx, "", nil, 10, nil)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if strings.Contains(recall, oppSummary) {
		t.Errorf("opportunity leaked into explicit Recall:\n%s", recall)
	}
	if !strings.Contains(recall, factText) {
		t.Error("expected the ordinary fact to surface in Recall (sanity: lane is live)")
	}
}

// TestQueuedOpportunitiesIncludesFinancialButPickerDoesNot proves the
// management-queue reader (QueuedOpportunities, req 7.3) surfaces BOTH eligible
// and financial pending items so the user can approve a financial one, while
// the autonomous picker (PendingOpportunities) still returns ONLY the eligible
// item — the no-money filter is untouched.
func TestQueuedOpportunitiesIncludesFinancialButPickerDoesNot(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	if _, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary: "Clean up the stale TODO comments", EligibleAutonomous: true, Confidence: 0.8,
	}); err != nil {
		t.Fatalf("RememberOpportunity eligible: %v", err)
	}
	if _, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary: "Swap 100 PAX for USDC at the best rate", EligibleAutonomous: false, Confidence: 0.95,
	}); err != nil {
		t.Fatalf("RememberOpportunity financial: %v", err)
	}

	// The management queue surfaces both (financial included, for approval).
	queued, err := p.QueuedOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("QueuedOpportunities: %v", err)
	}
	if len(queued) != 2 {
		t.Fatalf("management queue must include both pending items, got %d", len(queued))
	}
	var sawFinancial bool
	for _, o := range queued {
		if !o.EligibleAutonomous && strings.Contains(o.Summary, "Swap") {
			sawFinancial = true
		}
	}
	if !sawFinancial {
		t.Error("management queue must surface the financial item for explicit approval")
	}

	// The autonomous picker must STILL exclude the financial item.
	pending, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("autonomous picker must return only the eligible item, got %d", len(pending))
	}
	if !pending[0].EligibleAutonomous || strings.Contains(pending[0].Summary, "Swap") {
		t.Errorf("a financial item leaked into the autonomous picker set: %+v", pending[0])
	}

	// Dismissing an item drops it from the management queue too.
	if err := p.SetOpportunityStatus(ctx, queued[0].URI, OpportunityDismissed); err != nil {
		t.Fatalf("SetOpportunityStatus dismiss: %v", err)
	}
	after, err := p.QueuedOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("QueuedOpportunities after dismiss: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("a dismissed item must leave the management queue, got %d", len(after))
	}
}
