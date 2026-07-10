// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"
)

// hitWith returns the first hit whose text contains sub, or a zero hit when
// none match.
func hitWith(hits []RecallHit, sub string) (RecallHit, bool) {
	for _, h := range hits {
		if strings.Contains(h.Text, sub) {
			return h, true
		}
	}
	return RecallHit{}, false
}

// RecallHits is the structured pull behind the memory_recall tool (v3 #1):
// each hit must carry a citable cortex URI, its type, and live valid-time.
func TestRecallHitsStructuredFields(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const fact = "the gateway credit ledger lives in postgres"
	if _, err := p.RememberFact(ctx, fact); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	drain(t, p)

	// Empty query forces the deterministic type-filtered path (no dependency on
	// async embedding freshness), exactly like the Retrieve tests.
	hits, err := p.RecallHits(ctx, "", nil, 0, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	h, ok := hitWith(hits, "postgres")
	if !ok {
		t.Fatalf("expected the stored fact to be recalled; got %+v", hits)
	}
	if !strings.HasPrefix(h.URI, "matrix://cortex/") {
		t.Errorf("hit must carry a citable cortex URI, got %q", h.URI)
	}
	if h.Type != "Fact" {
		t.Errorf("hit Type = %q, want Fact", h.Type)
	}
	if h.ValidUntil != nil {
		t.Errorf("a live (un-superseded) memory must have nil ValidUntil, got %v", *h.ValidUntil)
	}
}

// A type filter must restrict results to the requested types only — the
// narrowing lever the model uses to iterate (v3 #1).
func TestRecallHitsTypeFilter(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	if _, err := p.RememberFact(ctx, "the router admin API binds 127.0.0.1:8088"); err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	if _, err := p.RecordOutcome(ctx, "shipped the bi-temporal validity pass", OutcomeSuccess, ""); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	drain(t, p)

	hits, err := p.RecallHits(ctx, "", []string{"event"}, 0, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least the one Event to be recalled")
	}
	for _, h := range hits {
		if h.Type != "Event" {
			t.Errorf("type filter [event] returned a %s hit: %q", h.Type, h.Text)
		}
	}
	if _, ok := hitWith(hits, "8088"); ok {
		t.Error("a Fact must not appear under a [event] type filter")
	}
}

// A superseded memory's valid-time is closed (relate -> cortex.CloseValidity),
// so RecallHits at "now" must drop it — the agent never pulls a stale truth
// (v3 #1 + #2). This exercises the cortex AsOf filter via the recall path,
// distinct from Retrieve's pager-side validity check.
func TestRecallHitsExcludesSuperseded(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const stale = "the executor model is grok-4.2"
	const fresh = "the executor model is xiaomimimo/mimo-v2.5-pro"
	oldURI, err := p.RememberFact(ctx, stale)
	if err != nil || oldURI == "" {
		t.Fatalf("RememberFact stale: uri=%q err=%v", oldURI, err)
	}
	newURI, err := p.RememberFact(ctx, fresh)
	if err != nil || newURI == "" {
		t.Fatalf("RememberFact fresh: uri=%q err=%v", newURI, err)
	}
	drain(t, p)

	if err := p.relate(newURI, oldURI, RelationSupersedes, ""); err != nil {
		t.Fatalf("relate supersedes: %v", err)
	}

	hits, err := p.RecallHits(ctx, "", nil, 0, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	if _, ok := hitWith(hits, "xiaomimimo/mimo-v2.5-pro"); !ok {
		t.Errorf("superseding fact should be recalled; got %+v", hits)
	}
	if _, ok := hitWith(hits, "grok-4.2"); ok {
		t.Errorf("superseded fact must be excluded at now; got %+v", hits)
	}
}
