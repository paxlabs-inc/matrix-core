// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package writeback

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
)

// Verification — Property 1 (Automatrix task 7.1): capture is grounded,
// selective, deduped, and opt-in-independent.
//
// **Validates: Requirements 1.1, 1.2, 1.3, 1.5, 2.3**
//
// These tests exercise the REAL consolidator extraction path
// (Consolidator.process via ConsolidateSync → parse → fan-out →
// pager.RememberOpportunity) against a REAL cortex store (t.TempDir() + the
// real hash embedder, opened by memory.Open). NOTHING in the code under test
// is faked: the consolidator, the pager, and cortex are all real types running
// their real logic. The ONLY controlled test double is the external LLM HTTP
// endpoint — a real llm.Client is pointed at an httptest server that returns
// the strict-JSON extract payload over the same OpenAI SSE wire shape the
// provider speaks. This is the established neo seam for deterministic
// extraction (mirrors neo/internal/agent/compaction_sync_test.go and the
// MCL gateway httptest tests); it is NOT a stub of any code path under test.

// Transcript markers the extract server dispatches on. Each marker stands in
// for the kind of turn the real extraction model would see; the server returns
// the strict-JSON the correctly-behaving model would emit for that turn.
const (
	// An implied need the user did NOT ask Neo to do this turn.
	markerImplied = "quarterly update to the team next week"
	// A direct request already being handled this turn (NOT an opportunity).
	markerDirect = "Please deploy the staging build right now"
	// Idle chit-chat with no durable opportunity.
	markerNone = "the weather is lovely today"
	// The model emits an opportunity with NO grounding (empty rationale).
	markerUngrounded = "ungrounded opportunity probe"
	// The model emits a grounded opportunity BELOW the confidence floor.
	markerLowConf = "low confidence opportunity probe"
)

// The single grounded opportunity the implied-need transcript yields. Shared
// between the server's canned model output and the test's assertions so they
// cannot drift.
const (
	expectedSummary   = "Draft the quarterly update document for the team"
	expectedRationale = "user said they need to send a quarterly update to the team next week but have not started"
)

func emptyExtract() string {
	return `{"facts":[],"user_facts":[],"preferences":[],"corrections":[],"patterns":[],"opportunities":[],"outcome":null}`
}

func impliedExtract() string {
	return fmt.Sprintf(
		`{"facts":[],"user_facts":[],"preferences":[],"corrections":[],"patterns":[],"opportunities":[{"summary":%q,"rationale":%q,"financial":false,"confidence":0.82}],"outcome":null}`,
		expectedSummary, expectedRationale,
	)
}

// ungroundedExtract emits a high-confidence opportunity with an EMPTY
// rationale — the consolidator must drop it (grounding is required, req.1.2).
func ungroundedExtract() string {
	return `{"facts":[],"user_facts":[],"preferences":[],"corrections":[],"patterns":[],"opportunities":[{"summary":"Clean up the stale TODO comments","rationale":"   ","financial":false,"confidence":0.95}],"outcome":null}`
}

// lowConfExtract emits a grounded opportunity BELOW the configured confidence
// floor (default 0.6) — the consolidator must drop it (selective, req.1.2).
func lowConfExtract() string {
	return `{"facts":[],"user_facts":[],"preferences":[],"corrections":[],"patterns":[],"opportunities":[{"summary":"Maybe reorganize the docs folder","rationale":"user vaguely mused about docs being messy","financial":false,"confidence":0.30}],"outcome":null}`
}

// writeSSE serializes a single OpenAI-style chat-completions SSE stream whose
// assistant content is modelContent, then [DONE]. Marshaling the chunk handles
// the JSON-string escaping of the extract payload carried in "content".
func writeSSE(w http.ResponseWriter, modelContent string) {
	chunk := map[string]any{
		"choices": []map[string]any{{
			"delta":         map[string]any{"content": modelContent},
			"finish_reason": "stop",
		}},
	}
	b, _ := json.Marshal(chunk)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

// newExtractServer returns an httptest server that inspects the incoming
// transcript and returns the strict-JSON extract a correctly-behaving model
// would emit. The default (no marker matched — e.g. the no-opportunity and
// direct-request transcripts) is an empty extract: a direct request is already
// being handled and is NEVER captured as an opportunity (req.1.3).
func newExtractServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.Contains(s, markerImplied):
			writeSSE(w, impliedExtract())
		case strings.Contains(s, markerUngrounded):
			writeSSE(w, ungroundedExtract())
		case strings.Contains(s, markerLowConf):
			writeSSE(w, lowConfExtract())
		default:
			writeSSE(w, emptyExtract())
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newExtractClient builds a REAL llm.Client whose only difference from a
// production client is its endpoint: the httptest server. GatewayURL is set so
// the POST resolves to ${srv}/v1/chat/completions and the chat-completions
// shape guard is satisfied (the handler ignores the path).
func newExtractClient(t *testing.T, srv *httptest.Server) *llm.Client {
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

// newCaptureHarness wires the REAL consolidator to a REAL cortex-backed pager
// under a fresh temp dir. The extraction client is the httptest-driven real
// client. config.Default() leaves AutomatrixEnabled=false (opt-in nominally
// OFF) and AutomatrixMinConfidence=0.6 (the capture floor).
func newCaptureHarness(t *testing.T, client *llm.Client) (*Consolidator, *memory.Pager, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-capture-test"
	p, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.SetMemoryConsent(context.Background(), true, "test user"); err != nil {
		t.Fatalf("SetMemoryConsent: %v", err)
	}
	c := New(client, nil, p, cfg)
	return c, p, cfg
}

// TestCapture_ImpliedNeedYieldsRealOpportunity: an implied-need transcript run
// through the real consolidator extraction writes exactly one real cortex
// Opportunity record, grounded and eligible-autonomous. (req.1.1)
func TestCapture_ImpliedNeedYieldsRealOpportunity(t *testing.T) {
	srv := newExtractServer(t)
	client := newExtractClient(t, srv)
	c, p, cfg := newCaptureHarness(t, client)
	ctx := context.Background()

	// Opt-in is nominally OFF — capture must still run (req.1.5), proven below.
	if cfg.AutomatrixEnabled {
		t.Fatal("precondition: AutomatrixEnabled should default to false (opt-in OFF)")
	}

	transcript := "User: I really need to send a " + markerImplied +
		" but I haven't started it yet.\nNeo: Understood, noted."
	c.ConsolidateSync(ctx, transcript, "", 0, 0)

	got, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("implied-need transcript: want exactly 1 captured opportunity, got %d", len(got))
	}
	o := got[0]
	if o.Summary != expectedSummary {
		t.Errorf("summary = %q, want %q", o.Summary, expectedSummary)
	}
	if o.Rationale != expectedRationale {
		t.Errorf("rationale = %q, want %q", o.Rationale, expectedRationale)
	}
	if !o.EligibleAutonomous {
		t.Error("a non-financial opportunity must be eligible_autonomous")
	}
	if o.Status != memory.OpportunityPending {
		t.Errorf("status = %q, want pending", o.Status)
	}
	if o.Confidence < 0.81 || o.Confidence > 0.83 {
		t.Errorf("confidence = %v, want ~0.82", o.Confidence)
	}
}

// TestCapture_NoOpportunityTranscriptYieldsNone: idle chit-chat with no
// implied need writes no opportunity (selective; most turns yield none). (req.1.2)
func TestCapture_NoOpportunityTranscriptYieldsNone(t *testing.T) {
	srv := newExtractServer(t)
	client := newExtractClient(t, srv)
	c, p, _ := newCaptureHarness(t, client)
	ctx := context.Background()

	transcript := "User: Honestly " + markerNone + ", makes me happy.\nNeo: It does sound pleasant."
	c.ConsolidateSync(ctx, transcript, "", 0, 0)

	got, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("no-opportunity transcript must capture nothing, got %d: %+v", len(got), got)
	}
}

// TestCapture_DirectRequestNotCaptured: a direct request the user made this
// turn is already being handled and must NEVER be captured as a proactive
// opportunity. (req.1.3)
func TestCapture_DirectRequestNotCaptured(t *testing.T) {
	srv := newExtractServer(t)
	client := newExtractClient(t, srv)
	c, p, _ := newCaptureHarness(t, client)
	ctx := context.Background()

	transcript := "User: " + markerDirect + ".\nNeo: Deploying the staging build now."
	c.ConsolidateSync(ctx, transcript, "", 0, 0)

	got, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a direct request must not be captured as an opportunity, got %d: %+v", len(got), got)
	}
}

// TestCapture_RepeatMentionDeduped: the same implied need mentioned across two
// turns dedups to a single cortex record (req.2.3) — driven entirely through
// the real RememberOpportunity normalize+semantic-similarity path.
func TestCapture_RepeatMentionDeduped(t *testing.T) {
	srv := newExtractServer(t)
	client := newExtractClient(t, srv)
	c, p, _ := newCaptureHarness(t, client)
	ctx := context.Background()

	transcript := "User: reminder, I still need to send a " + markerImplied + ".\nNeo: Noted."
	// Two separate turns surfacing the same need.
	c.ConsolidateSync(ctx, transcript, "", 0, 0)
	c.ConsolidateSync(ctx, transcript, "", 0, 0)

	got, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a repeated mention must dedup to one record, got %d: %+v", len(got), got)
	}
}

// TestCapture_RunsIndependentOfOptIn: capture runs UNCONDITIONALLY whenever the
// feature is built in, independent of the per-user opt-in, so the queue is
// warm if/when the user enables proactive execution. Default config has the
// opt-in OFF, yet the implied need is still captured. (req.1.5)
func TestCapture_RunsIndependentOfOptIn(t *testing.T) {
	srv := newExtractServer(t)
	client := newExtractClient(t, srv)
	c, p, cfg := newCaptureHarness(t, client)
	ctx := context.Background()

	if cfg.AutomatrixEnabled {
		t.Fatal("this test asserts capture with opt-in OFF; default must be false")
	}

	transcript := "User: I keep meaning to send a " + markerImplied + ".\nNeo: Got it."
	c.ConsolidateSync(ctx, transcript, "", 0, 0)

	got, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("capture must run with opt-in OFF (warm-queue invariant), got %d records", len(got))
	}
}

// TestCapture_UngroundedAndLowConfidenceDropped exercises the consolidator's
// REAL selectivity code: an opportunity emitted by the model with NO grounding
// (empty rationale) is dropped, and a grounded opportunity below the configured
// confidence floor is dropped. (req.1.2)
func TestCapture_UngroundedAndLowConfidenceDropped(t *testing.T) {
	srv := newExtractServer(t)
	client := newExtractClient(t, srv)
	ctx := context.Background()

	t.Run("ungrounded dropped", func(t *testing.T) {
		c, p, _ := newCaptureHarness(t, client)
		c.ConsolidateSync(ctx, "User: "+markerUngrounded+".\nNeo: ok", "", 0, 0)
		got, err := p.PendingOpportunities(ctx, 0)
		if err != nil {
			t.Fatalf("PendingOpportunities: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("an ungrounded (no-rationale) opportunity must be dropped, got %d: %+v", len(got), got)
		}
	})

	t.Run("below-floor confidence dropped", func(t *testing.T) {
		c, p, cfg := newCaptureHarness(t, client)
		if cfg.AutomatrixMinConfidence <= 0.30 {
			t.Fatalf("test assumes the floor (%v) is above 0.30", cfg.AutomatrixMinConfidence)
		}
		c.ConsolidateSync(ctx, "User: "+markerLowConf+".\nNeo: ok", "", 0, 0)
		got, err := p.PendingOpportunities(ctx, 0)
		if err != nil {
			t.Fatalf("PendingOpportunities: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("a below-floor-confidence opportunity must be dropped, got %d: %+v", len(got), got)
		}
	})
}
