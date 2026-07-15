// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

// Tests for the ORACLE brief content policy + recommendation history (task 5.5,
// req 16/17). The deterministic post-check and objective composition are pure
// functions tested directly; the delivery/no-repeat/feedback loops run on the
// REAL engine, durable history store, cortex pager, and HTTP surface.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/briefhistory"
	"matrix/neo/internal/memory"
)

const testBrief = `Good morning. Here is your brief.

News:
- OpenAI shipped a new eval suite (published 2026-07-14) — relevant to your AI research interest. Source: https://news.test/openai-evals
- Quantum-chip yields doubled (published 2026-07-14). Source: https://news.test/quantum-yields

Recommendation: the album "Golden Hour" — country, matches your taste for storytelling lyrics.

Neo can help today: I can draft your weekly plan — just say the word.`

func TestBriefPolicyViolation_Deterministic(t *testing.T) {
	// A clean brief passes.
	if v := briefPolicyViolation(testBrief, nil, nil); v != "" {
		t.Errorf("clean brief must pass, got %q", v)
	}
	// Repeating an already-delivered source fails (req 17.1).
	if v := briefPolicyViolation(testBrief, nil, []string{"https://news.test/openai-evals"}); v == "" {
		t.Error("a repeated news source must violate the policy")
	}
	// Repeating an already-delivered recommendation fails (req 17.1),
	// case-insensitively.
	if v := briefPolicyViolation(testBrief, []string{`THE ALBUM "GOLDEN HOUR" — country, matches your taste for storytelling lyrics.`}, nil); v == "" {
		t.Error("a repeated recommendation must violate the policy")
	}
	// More recommendations than the cap fails (req 16.2).
	over := testBrief + "\nRecommendation: another thing.\nRecommendation: yet another thing."
	if v := briefPolicyViolation(over, nil, nil); v == "" {
		t.Error("3 recommendations must violate the cap of 2")
	}
	// More distinct source links than the budget fails (req 16.1 proxy).
	var many strings.Builder
	many.WriteString("brief with too many sources:\n")
	for i := 0; i < briefMaxLinks+1; i++ {
		many.WriteString("- item. Source: https://news.test/item-")
		many.WriteByte(byte('a' + i))
		many.WriteString("\n")
	}
	if v := briefPolicyViolation(many.String(), nil, nil); v == "" {
		t.Errorf("%d links must violate the budget of %d", briefMaxLinks+1, briefMaxLinks)
	}
}

func TestExtractBriefRecsAndURLs(t *testing.T) {
	recs := extractBriefRecs(testBrief)
	if len(recs) != 1 || !strings.Contains(recs[0], "Golden Hour") {
		t.Errorf("recs = %v", recs)
	}
	urls := extractBriefURLs(testBrief)
	if len(urls) != 2 || urls[0] != "https://news.test/openai-evals" {
		t.Errorf("urls = %v", urls)
	}
}

// TestBriefObjective_ProfilePolicyNoRepeat proves the composed objective
// carries the confirmed profile, the content-policy rules, and the durable
// no-repeat set (req 16, 17.1).
func TestBriefObjective_ProfilePolicyNoRepeat(t *testing.T) {
	obj := buildBriefObjective(
		briefSettingsState{Length: "short", Sections: []string{"news", "music"}},
		"- Interests: quantum computing\n- Recommendation adventurousness: surprising",
		[]string{"the album Golden Hour"},
		[]string{"https://news.test/openai-evals"},
	)
	for _, want := range []string{
		"quantum computing",
		"surprising",
		"At most 5 news items",
		"publication date",
		"Recommendation:",
		"Neo can help today",
		"disclosure",
		"Do NOT repeat",
		"the album Golden Hour",
		"https://news.test/openai-evals",
		"news, music",
		"short",
	} {
		if !strings.Contains(obj, want) {
			t.Errorf("objective missing %q", want)
		}
	}
}

// TestBriefDelivery_RecordsHistoryAndNoRepeat drives the REAL delivery path
// end-to-end: a policy-clean brief is delivered (conversation turn + history
// record with its recs + URLs), and the SAME content on a later date is
// refused by the no-repeat post-check — not delivered, date not marked, so the
// next wake retries (req 17.1).
func TestBriefDelivery_RecordsHistoryAndNoRepeat(t *testing.T) {
	e, gov := newBriefRunTestEngine(t, "")
	e.briefHistory = briefhistory.Open(t.TempDir())
	if err := gov.SetEnabled(context.Background(), true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	e.deliverBrief("brief-conv", "run-1", "2026-07-15", testBrief)

	if !gov.Settings().AlreadyDelivered("2026-07-15") {
		t.Fatal("a policy-clean brief must be delivered and its date marked")
	}
	recIDs, urls := e.briefHistory.RecentDelivered(0)
	if len(recIDs) != 1 || len(urls) != 2 {
		t.Fatalf("history after delivery: recs %v urls %v", recIDs, urls)
	}

	// The same content the next day violates no-repeat: nothing delivered,
	// date NOT marked (a later wake retries with a fresh run).
	turnsBefore := len(e.conv.History("brief-conv"))
	e.deliverBrief("brief-conv", "run-2", "2026-07-16", testBrief)
	if gov.Settings().AlreadyDelivered("2026-07-16") {
		t.Error("a repeat brief must NOT mark its date delivered")
	}
	if got := len(e.conv.History("brief-conv")); got != turnsBefore {
		t.Error("a repeat brief must not land a conversation turn")
	}
}

// TestBriefFeedbackRoute_HistoryAndProfileFlow drives POST /brief/feedback on
// the real HTTP surface: the explicit reaction lands in the durable history AND
// flows into the profile as an explicit user-attributed preference entry —
// readable back through the pager's real learned-guidance surface (req 17.2).
func TestBriefFeedbackRoute_HistoryAndProfileFlow(t *testing.T) {
	e, _ := newBriefRunTestEngine(t, "")
	e.briefHistory = briefhistory.Open(t.TempDir())
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]string{
		"item":    "the album Golden Hour",
		"verdict": briefhistory.VerdictNotInterested,
	})
	resp, err := http.Post(ts.URL+"/brief/feedback", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Durable history record.
	recs := e.briefHistory.Recent(0)
	if len(recs) != 1 || recs[0].Kind != briefhistory.KindFeedback || recs[0].Verdict != briefhistory.VerdictNotInterested {
		t.Fatalf("history = %+v", recs)
	}

	// Explicit user-attributed profile flow: the preference reads back through
	// the pager's real learned-guidance surface.
	deadline := time.Now().Add(5 * time.Second)
	for {
		found := false
		for _, g := range e.pager.LearnedGuidance(context.Background()) {
			if strings.Contains(g, "Golden Hour") {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("feedback preference did not reach the pager's learned guidance")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// An invalid verdict is rejected and persists nothing.
	bad, _ := json.Marshal(map[string]string{"item": "x", "verdict": "meh"})
	badResp, err := http.Post(ts.URL+"/brief/feedback", "application/json", bytes.NewReader(bad))
	if err != nil {
		t.Fatalf("POST bad: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad verdict status = %d, want 400", badResp.StatusCode)
	}
	if got := len(e.briefHistory.Recent(0)); got != 1 {
		t.Errorf("rejected feedback must not persist, got %d records", got)
	}
}

// TestBriefRun_ObjectiveCarriesProfile proves the live dispatch path composes
// the objective from the REAL saved profile + history (the seam the runner
// actually uses): with a profile saved and history seeded, the supervised run
// dispatched by a wake sends the model an objective carrying both.
func TestBriefRun_ObjectiveCarriesProfile(t *testing.T) {
	srv, lastBody := capturingModelServer(t)
	e, gov := newBriefRunTestEngine(t, srv.URL)
	e.briefHistory = briefhistory.Open(t.TempDir())
	e.SetBriefRunner(e.RunMorningBrief)
	if err := gov.SetEnabled(context.Background(), true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := e.pager.SavePersonalizationProfile(context.Background(), memory.PersonalizationProfile{
		Interests: []string{"quantum computing"},
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if err := e.briefHistory.AppendDelivery("2026-07-14", "brief-conv", []string{"rec-golden-hour"}, []string{"https://news.test/old"}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	if !e.MaybeHandleMorningBriefWake(context.Background(), "x", agent.MorningBriefWakeMessage) {
		t.Fatal("wake must be intercepted")
	}
	// The run completes in the background; wait for the model request.
	deadline := time.Now().Add(10 * time.Second)
	for lastBody() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	body := lastBody()
	if body == "" {
		t.Fatal("no model request captured")
	}
	for _, want := range []string{"quantum computing", "rec-golden-hour", "https://news.test/old", "At most 5 news items"} {
		if !strings.Contains(body, want) {
			t.Errorf("brief-run model request missing %q", want)
		}
	}
	// Settle the background goroutine before Cleanup closes the engine.
	waitBounded(&e.briefWG, 10*time.Second)
}
