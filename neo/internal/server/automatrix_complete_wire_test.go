// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Tests for the completion -> durable record + notify WIRING (task 5.3,
// req 6.1/6.4/6.5/8.3).
//
// Everything under test is REAL — no fakes/mocks/stubs of the runner, settle
// path, agent, pager, durable inbox, or notifier:
//
//   • the autonomous run drives the REAL restricted agent + REAL supervised
//     settle path through RunAutomatrixOpportunity, with the model behind an
//     httptest server (the established neo seam);
//   • the opportunity lifecycle is the REAL Neocortex pager (t.TempDir(), hash
//     embedder, offline);
//   • the durable record is the REAL automatrixlog store (AutomatrixDir set);
//   • the notifier is a REAL notify.New ntfy backend pointed at an httptest
//     ntfy server — the genuine out-of-app send path round-trips over HTTP.
//
// The assertions verify the seam's contract: a GENUINE gate-passed completion
// writes the durable in-app record (unread) AND fires the out-of-app ping
// (result-not-protocol copy, no Chronos/alarm/marker/Neocortex jargon); a
// FAILED/partial completion writes NO record and sends NO ping (req 8.3 — a
// non-completed task is never announced).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/notify"
	"matrix/neo/internal/tools"
)

// capturedPing is one received ntfy POST (the fields the seam sets).
type capturedPing struct {
	topic string // last path segment of the publish URL
	title string // ntfy Title header
	body  string // request body
	click string // ntfy Click header (deep link)
}

// ntfyCapture is an httptest ntfy server that records every publish it receives.
type ntfyCapture struct {
	mu    sync.Mutex
	pings []capturedPing
}

func (c *ntfyCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seg := strings.TrimPrefix(r.URL.Path, "/")
		c.mu.Lock()
		c.pings = append(c.pings, capturedPing{
			topic: seg,
			title: r.Header.Get("Title"),
			body:  string(body),
			click: r.Header.Get("Click"),
		})
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (c *ntfyCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pings)
}

func (c *ntfyCapture) all() []capturedPing {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedPing, len(c.pings))
	copy(out, c.pings)
	return out
}

// newCompleteWireEngine builds a real Engine with a real Neocortex pager, a REAL
// durable Automatrix inbox (AutomatrixDir set), the model pointed at modelURL,
// and a REAL ntfy notifier pointed at the httptest ntfy server. Returns the
// engine, the pager, and the ntfy capture.
func newCompleteWireEngine(t *testing.T, modelURL string) (*Engine, *memory.Pager, *ntfyCapture) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-automatrix-wire-test"

	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}

	var client *llm.Client
	if modelURL != "" {
		client, err = llm.New(mcllm.Config{
			Model:       "test-model",
			Endpoint:    modelURL,
			GatewayURL:  modelURL,
			Provider:    mcllm.ProviderFireworks,
			ProviderSet: true,
			APIKey:      "test-key",
		})
		if err != nil {
			t.Fatalf("llm.New: %v", err)
		}
	}

	manager := &tools.Manager{}
	runtime := openCanonicalTestRuntime(t, &cfg, manager, pager, modelURL)
	e := NewEngine(EngineOptions{
		Config:        cfg,
		Main:          client,
		Cheap:         client,
		Tools:         manager,
		Pager:         pager,
		Runtime:       runtime,
		AutomatrixDir: t.TempDir(),
		BackendURL:    "http://127.0.0.1:1",
	})
	if !e.AutomatrixInbox().Enabled() {
		t.Fatal("durable inbox must be enabled when AutomatrixDir is set")
	}

	// Real ntfy backend pointed at the httptest server (the established notify
	// seam — a genuine HTTP round-trip, not a fake notifier).
	cap := &ntfyCapture{}
	ntfy := httptest.NewServer(cap.handler())
	t.Cleanup(ntfy.Close)
	e.SetAutomatrixNotifier(notify.New(notify.Config{
		NtfyServer: ntfy.URL,
		NtfyTopic:  "wire-test-topic",
	}))

	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})
	return e, pager, cap
}

// waitForPing polls the ntfy capture until at least want pings have arrived (the
// send is best-effort + non-blocking on a background goroutine) or the timeout
// elapses. Returns the received pings.
func waitForPing(t *testing.T, cap *ntfyCapture, want int, timeout time.Duration) []capturedPing {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cap.count() >= want {
			return cap.all()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected at least %d ntfy ping(s) within %v, got %d", want, timeout, cap.count())
	return nil
}

// jargon enumerates the protocol terms the consumer-facing record + ping must
// NEVER contain (req 6.5).
var jargon = []string{"chronos", "alarm", "automatrix", "marker", "Neocortex", "heartbeat"}

func assertNoJargon(t *testing.T, label, text string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, j := range jargon {
		if strings.Contains(low, j) {
			t.Errorf("%s leaked protocol jargon %q: %q", label, j, text)
		}
	}
}

// TestAutomatrixCompletionWiring_GenuinePassRecordsAndPings drives the REAL
// runner end-to-end against a model that produces a clean completion. The seam
// must (1) mark the opportunity done, (2) write the durable in-app record
// (unread, result intact), and (3) fire the out-of-app ntfy ping with
// result-not-protocol copy.
func TestAutomatrixCompletionWiring_GenuinePassRecordsAndPings(t *testing.T) {
	srv := doneModelServer(t)
	e, pager, cap := newCompleteWireEngine(t, srv.URL)

	const summary = "Explain what a useful quarterly update should contain"
	uri, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary:              summary,
		Rationale:            "a concise planning explanation would help next week",
		EligibleAutonomous:   true,
		Confidence:           0.9,
		OriginConversationID: "conv-origin",
	})
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	opp, _, err := pager.OpportunityByURI(context.Background(), uri)
	if err != nil {
		t.Fatalf("OpportunityByURI: %v", err)
	}

	if err := e.RunAutomatrixOpportunity(context.Background(), opp); err != nil {
		t.Fatalf("RunAutomatrixOpportunity: %v", err)
	}

	// (1) The opportunity reaches done only on a genuine gate pass.
	waitOpportunity(t, pager, uri, "done", statusIs(memory.OpportunityDone), 10*time.Second)

	// (3) Out-of-app ping fired with result-not-protocol copy on the configured
	// topic. The durable record is written BEFORE the ping is dispatched, so a
	// received ping guarantees the record landed — wait on it first to avoid
	// racing the settle goroutine.
	pings := waitForPing(t, cap, 1, 10*time.Second)
	p := pings[0]
	if p.topic != "wire-test-topic" {
		t.Errorf("ping topic = %q, want the configured topic", p.topic)
	}
	if strings.TrimSpace(p.title) == "" || strings.TrimSpace(p.body) == "" {
		t.Errorf("ping must carry a title + body, got title=%q body=%q", p.title, p.body)
	}
	assertNoJargon(t, "ping title", p.title)
	assertNoJargon(t, "ping body", p.body)

	// (2) Durable in-app record written + unread, carrying the result.
	inbox := e.AutomatrixInbox().List()
	if len(inbox) != 1 {
		t.Fatalf("want exactly 1 durable completion record, got %d", len(inbox))
	}
	rec := inbox[0]
	if rec.Read {
		t.Error("a fresh completion record must be unread")
	}
	if rec.OpportunitySummary != summary {
		t.Errorf("record opportunity summary = %q, want %q", rec.OpportunitySummary, summary)
	}
	if strings.TrimSpace(rec.ResultSummary) == "" {
		t.Error("record result summary must carry the run's produced answer")
	}
	if !strings.Contains(rec.ResultSummary, "outcomes, risks, and next steps") {
		t.Errorf("record result summary should reflect the run's answer, got %q", rec.ResultSummary)
	}
	assertNoJargon(t, "record opportunity summary", rec.OpportunitySummary)
	assertNoJargon(t, "record result summary", rec.ResultSummary)
}

// TestAutomatrixCompletionWiring_FailNoRecordNoPing drives the REAL runner
// against a model that rejects every call. The run cannot pass the gate, so the
// seam must announce NOTHING: no durable record and no ntfy ping (req 8.3 — a
// non-completed task is never surfaced).
func TestAutomatrixCompletionWiring_FailNoRecordNoPing(t *testing.T) {
	srv := rejectModelServer(t)
	e, pager, cap := newCompleteWireEngine(t, srv.URL)
	// Single attempt per run (no respawn) so the failure terminal is fast.
	e.cfg.SuperviseTasks = false

	uri, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary:              "Summarize the API rate-limit thread",
		EligibleAutonomous:   true,
		Confidence:           0.7,
		OriginConversationID: "conv-origin",
	})
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	opp, _, _ := pager.OpportunityByURI(context.Background(), uri)

	if err := e.RunAutomatrixOpportunity(context.Background(), opp); err != nil {
		t.Fatalf("RunAutomatrixOpportunity: %v", err)
	}

	// Wait for the failed attempt to be recorded (re-pended with attempts++).
	waitOpportunity(t, pager, uri, "re-pended with attempts==1",
		func(s memory.OpportunitySpec) bool {
			return s.Status == memory.OpportunityPending && s.Attempts == 1
		}, 10*time.Second)

	// A failed completion announces NOTHING: no durable record …
	if got := len(e.AutomatrixInbox().List()); got != 0 {
		t.Errorf("a failed completion must write no durable record, got %d", got)
	}
	// … and no external ping. Give any errant background send a moment to land
	// so the assertion is not vacuous.
	time.Sleep(200 * time.Millisecond)
	if got := cap.count(); got != 0 {
		t.Errorf("a failed completion must send no ping, got %d", got)
	}
}
