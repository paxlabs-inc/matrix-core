// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package writeback

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix/cortex"
)

// DEJA-VU task 1.1 verification (req 1.1/1.2/1.3): derived_from provenance
// edges at the consolidator write site + the ExpandToTranscript resolver.
//
// These tests run the REAL consolidator extraction path against a REAL cortex
// store (memory.Open on a temp dir), a REAL pager, real cortex edges, and real
// Transcript reads. The only controlled double is the external LLM HTTP
// endpoint (the established neo httptest SSE seam) — no code path under test is
// faked.

// The distinctive fact the marker transcript yields, shared between the server
// output and the assertions so they cannot drift.
const (
	markerFact   = "zephyr deploy pipeline"
	expectedFact = "The Zephyr deploy pipeline runs on Buildkite."
)

func factExtract() string {
	return fmt.Sprintf(
		`{"facts":[%q],"user_facts":[],"preferences":[],"corrections":[],"patterns":[],"opportunities":[],"outcome":null}`,
		expectedFact,
	)
}

// newFactServer returns an httptest server that emits a single-fact extract for
// the markerFact transcript and an empty extract otherwise.
func newFactServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), markerFact) {
			writeSSE(w, factExtract())
			return
		}
		writeSSE(w, emptyExtract())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProvenance_ConsolidatorWritesEdge_ResolverRoundTrips: a consolidator run
// over a real transcript writes a fact memory AND a derived_from provenance
// edge to the exact session slice; ExpandToTranscript follows the edge back to
// the verbatim messages with correct provenance metadata. (req 1.1, 1.3)
func TestProvenance_ConsolidatorWritesEdge_ResolverRoundTrips(t *testing.T) {
	srv := newFactServer(t)
	client := newExtractClient(t, srv)
	c, p, _ := newCaptureHarness(t, client)
	ctx := context.Background()

	conv := "conv-prov-exact"
	msgs := []cortex.Message{
		{ConversationID: conv, Role: cortex.RoleUser, Content: "Tell me about the " + markerFact + "."},
		{ConversationID: conv, Role: cortex.RoleAssistant, Content: expectedFact},
		{ConversationID: conv, Role: cortex.RoleUser, Content: "Great, thanks."},
	}
	var seqLo, seqHi uint64
	for i, m := range msgs {
		uri, err := p.AppendMessage(m)
		if err != nil {
			t.Fatalf("AppendMessage #%d: %v", i, err)
		}
		_, seq, ok := cortex.ParseSessionURI(uri)
		if !ok {
			t.Fatalf("ParseSessionURI(%q) failed", uri)
		}
		if i == 0 {
			seqLo, seqHi = seq, seq
		}
		if seq > seqHi {
			seqHi = seq
		}
	}

	transcript := "USER: Tell me about the " + markerFact + ".\nASSISTANT: " + expectedFact
	c.ConsolidateSync(ctx, transcript, conv, seqLo, seqHi)

	// Locate the written fact deterministically (type scan, no embedder race).
	hits, err := p.RecallHits(ctx, "", []string{"fact"}, 20, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	memURI := ""
	for _, h := range hits {
		if strings.Contains(h.Text, "Buildkite") {
			memURI = h.URI
			break
		}
	}
	if memURI == "" {
		t.Fatalf("consolidator did not write the expected fact; hits=%+v", hits)
	}

	slice, err := p.ExpandToTranscript(memURI, 0)
	if err != nil {
		t.Fatalf("ExpandToTranscript: %v", err)
	}
	if slice == nil {
		t.Fatal("resolver returned nil for a memory with a provenance edge")
	}
	if !slice.Exact {
		t.Error("slice from a write-time provenance edge must be Exact")
	}
	if slice.ConversationID != conv {
		t.Errorf("conv = %q, want %q", slice.ConversationID, conv)
	}
	if slice.SeqLo != seqLo || slice.SeqHi != seqHi {
		t.Errorf("seq span = [%d,%d], want [%d,%d]", slice.SeqLo, slice.SeqHi, seqLo, seqHi)
	}
	found := false
	for _, m := range slice.Messages {
		if m.Content == expectedFact {
			found = true
		}
	}
	if !found {
		t.Fatalf("verbatim source message not in the resolved slice: %+v", slice.Messages)
	}
	if slice.Date.IsZero() {
		t.Error("resolved slice should carry a non-zero date")
	}
}

// TestProvenance_EdgeFailureDoesNotFailWrite: a provenance-link failure is
// contained and never touches the memory write (req 1.2 fail-open). A malformed
// memory URI makes LinkSessionProvenance error, yet the separately-written real
// memory still resolves.
func TestProvenance_EdgeFailureDoesNotFailWrite(t *testing.T) {
	srv := newFactServer(t)
	client := newExtractClient(t, srv)
	_, p, _ := newCaptureHarness(t, client)
	ctx := context.Background()

	uri, err := p.RememberFact(ctx, "The primary datastore is Pebble on NVMe")
	if err != nil {
		t.Fatalf("RememberFact: %v", err)
	}
	if strings.TrimSpace(uri) == "" {
		t.Fatal("RememberFact returned no URI")
	}

	// A malformed URI must surface an error from the link path...
	if lerr := p.LinkSessionProvenance("not-a-valid-uri", "conv-x", 0, 1); lerr == nil {
		t.Fatal("LinkSessionProvenance on a malformed URI must return an error")
	}
	// ...and a blank conv must be a silent no-op (best-effort).
	if lerr := p.LinkSessionProvenance(uri, "", 0, 1); lerr != nil {
		t.Fatalf("blank-conv link must be a no-op, got %v", lerr)
	}

	// The real memory is unaffected by the failed link.
	hits, err := p.RecallHits(ctx, "", []string{"fact"}, 20, nil)
	if err != nil {
		t.Fatalf("RecallHits: %v", err)
	}
	ok := false
	for _, h := range hits {
		if strings.Contains(h.Text, "Pebble on NVMe") {
			ok = true
		}
	}
	if !ok {
		t.Fatal("the memory write must survive a failed provenance link")
	}
}

// TestProvenance_FallbackApproximate: a memory with NO provenance edge resolves
// via the CreatedAt-proximity fallback ladder, marked approximate. (req 1.3)
func TestProvenance_FallbackApproximate(t *testing.T) {
	srv := newFactServer(t)
	client := newExtractClient(t, srv)
	_, p, _ := newCaptureHarness(t, client)
	ctx := context.Background()

	conv := "conv-prov-approx"
	for i, m := range []cortex.Message{
		{ConversationID: conv, Role: cortex.RoleUser, Content: "an orphan-era exchange about caching layers"},
		{ConversationID: conv, Role: cortex.RoleAssistant, Content: "we use a two-tier cache"},
	} {
		if _, err := p.AppendMessage(m); err != nil {
			t.Fatalf("AppendMessage #%d: %v", i, err)
		}
	}

	// A memory written WITHOUT any provenance edge (no consolidator link).
	uri, err := p.RememberFact(ctx, "The caching layer is two-tier")
	if err != nil {
		t.Fatalf("RememberFact: %v", err)
	}

	slice, err := p.ExpandToTranscript(uri, 1)
	if err != nil {
		t.Fatalf("ExpandToTranscript: %v", err)
	}
	if slice == nil {
		t.Fatal("fallback ladder returned nil; expected an approximate slice")
	}
	if slice.Exact {
		t.Error("an edge-less memory must resolve as approximate, not exact")
	}
	if slice.ConversationID != conv {
		t.Errorf("approx conv = %q, want %q", slice.ConversationID, conv)
	}
	if len(slice.Messages) == 0 {
		t.Fatal("approximate slice should carry at least the nearest message")
	}
}
