// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package writeback

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
)

// userFactExtract emits one durable user fact — a correctly-behaving model's
// output for a fact-bearing casual turn.
func userFactExtract() string {
	return `{"facts":[],"user_facts":[{"predicate":"location","statement":"The user lives in Berlin","evidence":[1]}],"preferences":[],"corrections":[],"patterns":[],"opportunities":[],"outcome":null}`
}

const factMarker = "I live in Berlin"

// countFacts reports how many current Fact records exist in the pager.
func countFacts(t *testing.T, p *memory.Pager) int {
	t.Helper()
	hits, err := p.RecallHits(context.Background(), "", []string{"fact"}, 50, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	return len(hits)
}

// newConsentServer returns a real extract server plus a call counter and an
// optional pre-response hook (used to simulate a mid-flight consent disable).
func newConsentServer(t *testing.T, calls *int32, before func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		_, _ = io.ReadAll(r.Body)
		if before != nil {
			before()
		}
		writeSSE(w, userFactExtract())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newConsentClient(t *testing.T, srv *httptest.Server) *llm.Client {
	t.Helper()
	client, err := llm.New(mcllm.Config{
		Model:       "test-extract-model",
		Endpoint:    srv.URL,
		GatewayURL:  srv.URL,
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		APIKey:      "test-key",
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	return client
}

func newConsentPager(t *testing.T) *memory.Pager {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-consent-test"
	p, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// With consent OFF (the default), a casual fact-bearing turn triggers NO
// extraction model call and writes NO personal fact (PRIV-01 req 8.2).
func TestConsentOff_NoExtractionCallOrWrite(t *testing.T) {
	var calls int32
	srv := newConsentServer(t, &calls, nil)
	client := newConsentClient(t, srv)
	p := newConsentPager(t)
	cfg := config.Default()
	c := New(client, nil, p, cfg)
	ctx := context.Background()

	// Consent is NOT enabled — the default off state.
	c.ConsolidateSync(ctx, "User: "+factMarker+".\nNeo: Noted.", "", 0, 0)

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("extraction model was called %d times with consent off; want 0", got)
	}
	if got := countFacts(t, p); got != 0 {
		t.Fatalf("wrote %d facts with consent off; want 0", got)
	}
}

// With consent ON, the same turn extracts and writes the durable user fact —
// proving the OFF result above is the consent gate, not a broken pipeline.
func TestConsentOn_ExtractsAndWrites(t *testing.T) {
	var calls int32
	srv := newConsentServer(t, &calls, nil)
	client := newConsentClient(t, srv)
	p := newConsentPager(t)
	if _, err := p.SetMemoryConsent(context.Background(), true, "test user"); err != nil {
		t.Fatalf("SetMemoryConsent: %v", err)
	}
	c := New(client, nil, p, config.Default())
	ctx := context.Background()

	c.ConsolidateSync(ctx, "User: "+factMarker+".\nNeo: Noted.", "", 0, 0)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("extraction model called %d times with consent on; want 1", got)
	}
	if got := countFacts(t, p); got != 1 {
		t.Fatalf("wrote %d facts with consent on; want 1", got)
	}
}

// Disabling consent WHILE an extraction is in flight blocks the write: the
// consolidator rechecks consent after the model returns and before any write
// (PRIV-01 req 8.2 — disabling stops extraction immediately).
func TestConsentDisabledMidFlight_BlocksWrite(t *testing.T) {
	p := newConsentPager(t)
	if _, err := p.SetMemoryConsent(context.Background(), true, "test user"); err != nil {
		t.Fatalf("SetMemoryConsent: %v", err)
	}
	var calls int32
	// The server disables consent the instant the extraction call lands,
	// simulating the user toggling memory off mid-turn.
	srv := newConsentServer(t, &calls, func() {
		if _, err := p.SetMemoryConsent(context.Background(), false, "test user"); err != nil {
			t.Errorf("mid-flight SetMemoryConsent(false): %v", err)
		}
	})
	client := newConsentClient(t, srv)
	c := New(client, nil, p, config.Default())
	ctx := context.Background()

	c.ConsolidateSync(ctx, "User: "+factMarker+".\nNeo: Noted.", "", 0, 0)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("extraction call count = %d; want 1 (the model was reached)", got)
	}
	if got := countFacts(t, p); got != 0 {
		t.Fatalf("wrote %d facts after mid-flight disable; want 0", got)
	}
}
