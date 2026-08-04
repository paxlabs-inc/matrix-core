// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

// Tests for the ORACLE guided personalization interview (task 5.3, req 12).
//
// Everything under test is REAL: the real Engine + Neocortex pager (t.TempDir()),
// the real conversation store meta sidecar (the interview flag), the real
// tools.Manager with the real engine-wired profile writer, the real interview
// agent (advertised set + charter captured off the actual model request), and
// a real writeback.Consolidator (whose structural absence on interview agents
// is the writeback-exclusion proof).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
	"matrix/neo/internal/writeback"
)

// capturingModelServer answers every chat request with a clean SSE completion
// and RECORDS each raw request body, so a test can assert on the REAL system
// prompt the agent sent.
func capturingModelServer(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"Great — first question: what topics do you care about?"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// newInterviewTestEngine builds a real Engine with a real pager, real durable
// conversation store, real tools.Manager (real profile-writer seam wired in
// NewEngine), a REAL writeback consolidator, and the model at modelURL.
func newInterviewTestEngine(t *testing.T, modelURL string) (*Engine, *memory.Pager) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-interview-test"
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
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Main:            client,
		Cheap:           client,
		Tools:           &tools.Manager{},
		Pager:           pager,
		Consolidator:    writeback.New(client, client, pager, cfg),
		ConversationDir: t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})
	return e, pager
}

// TestInterviewSession_SurfaceAndWritebackExclusion proves the two structural
// properties of an interview session against the REAL minted agents:
// save_personalization_profile is advertised ONLY on the interview session
// (req 13.3), and the interview agent runs with NO writeback consolidator
// (req 12.3) while a normal session over the same engine keeps both inverted.
func TestInterviewSession_SurfaceAndWritebackExclusion(t *testing.T) {
	e, _ := newInterviewTestEngine(t, "")

	e.conv.SetInterview("conv-interview")
	iv := e.newSession("conv-interview")
	normal := e.newSession("conv-normal")

	has := func(s *session, name string) bool {
		for _, sc := range s.agent.AdvertisedTools() {
			if sc.Function.Name == name {
				return true
			}
		}
		return false
	}
	if !has(iv, tools.SavePersonalizationTool) {
		t.Error("interview session must advertise save_personalization_profile")
	}
	if has(normal, tools.SavePersonalizationTool) {
		t.Error("a normal session must NOT advertise save_personalization_profile (req 13.3)")
	}
	if iv.agent.WritebackEnabled() {
		t.Error("interview agent must run with NO writeback extraction (req 12.3)")
	}
	if !normal.agent.WritebackEnabled() {
		t.Error("precondition: a normal session must run writeback, else the exclusion test is vacuous")
	}
}

// TestPersonalizationSave_ConfirmationGate drives the REAL dispatch path with
// the REAL engine-wired writer over the REAL Neocortex: an unconfirmed save is
// refused in-band and persists NOTHING; a confirmed save persists the single
// record; a second confirmed save VERSIONS the same record rather than adding
// one; skipped groups stay empty (req 12.2/12.3, 13.1).
func TestPersonalizationSave_ConfirmationGate(t *testing.T) {
	e, pager := newInterviewTestEngine(t, "")
	ctx := context.Background()

	// Unconfirmed → refused, nothing persisted.
	msg, isErr, err := e.tools.Dispatch(ctx, tools.SavePersonalizationTool, map[string]interface{}{
		"user_confirmed": false,
		"interests":      []interface{}{"AI research"},
	})
	if err != nil {
		t.Fatalf("unconfirmed dispatch transport error: %v", err)
	}
	if !isErr {
		t.Errorf("unconfirmed save must be an in-band refusal, got %q", msg)
	}
	if _, _, ok, perr := pager.PersonalizationProfile(ctx); perr != nil || ok {
		t.Fatalf("nothing may persist without confirmation (ok=%v err=%v)", ok, perr)
	}

	// Confirmed, groups 3-5 skipped → persists exactly what was said.
	_, isErr, err = e.tools.Dispatch(ctx, tools.SavePersonalizationTool, map[string]interface{}{
		"user_confirmed":   true,
		"interests":        []interface{}{"AI research", "country music"},
		"day_to_day_goals": []interface{}{"plan the week"},
	})
	if err != nil || isErr {
		t.Fatalf("confirmed dispatch failed: isErr=%v err=%v", isErr, err)
	}
	prof, uri1, ok, perr := pager.PersonalizationProfile(ctx)
	if perr != nil || !ok {
		t.Fatalf("confirmed save must persist the record (ok=%v err=%v)", ok, perr)
	}
	if len(prof.Interests) != 2 || prof.Interests[0] != "AI research" {
		t.Errorf("interests = %v", prof.Interests)
	}
	// Skipped groups leave their fields empty (req 12.2).
	if prof.Adventurousness != "" || prof.BriefPreferences.Length != "" || len(prof.Media.Music.Liked) != 0 {
		t.Errorf("skipped groups must stay empty, got %+v", prof)
	}

	// A second confirmed save (the repeat/edit path) VERSIONS the single record.
	_, isErr, err = e.tools.Dispatch(ctx, tools.SavePersonalizationTool, map[string]interface{}{
		"user_confirmed":  true,
		"interests":       []interface{}{"AI research"},
		"adventurousness": "surprising",
	})
	if err != nil || isErr {
		t.Fatalf("second confirmed dispatch failed: isErr=%v err=%v", isErr, err)
	}
	prof2, uri2, ok, perr := pager.PersonalizationProfile(ctx)
	if perr != nil || !ok {
		t.Fatalf("second read: ok=%v err=%v", ok, perr)
	}
	if prof2.Adventurousness != "surprising" || len(prof2.Interests) != 1 {
		t.Errorf("edit must replace the record content, got %+v", prof2)
	}
	if uri1 == uri2 {
		t.Error("a confirmed edit must bump the record version (same URI implies no versioned update)")
	}
}

// TestInterviewStartRoute mints an interview-flagged conversation over the real
// HTTP surface; the flag is durable conversation meta.
func TestInterviewStartRoute(t *testing.T) {
	e, _ := newInterviewTestEngine(t, "")
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/interview/start", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got map[string]string
	if derr := json.NewDecoder(resp.Body).Decode(&got); derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	convID := got["conversation_id"]
	if convID == "" {
		t.Fatal("no conversation_id returned")
	}
	if !e.conv.IsInterview(convID) {
		t.Error("the minted conversation must be durably interview-flagged")
	}

	getResp, err := http.Get(ts.URL + "/interview/start")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", getResp.StatusCode)
	}
}

// TestInterviewCharter_FiveGroupsAndRepeatWithExisting captures the REAL model
// request an interview agent sends and asserts the charter it carries: the
// five question groups, one-at-a-time + skippable discipline, the
// confirmation-before-save rule, the sensitive-traits prohibition, and — with
// a profile already saved — the repeat/edit re-entry with existing answers
// (req 12.1/12.2/12.3).
func TestInterviewCharter_FiveGroupsAndRepeatWithExisting(t *testing.T) {
	srv, lastBody := capturingModelServer(t)
	e, pager := newInterviewTestEngine(t, srv.URL)

	// Seed an existing profile so the charter re-enters with it.
	if _, err := pager.SavePersonalizationProfile(context.Background(), memory.PersonalizationProfile{
		Interests:       []string{"quantum computing"},
		Adventurousness: "balanced",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	e.conv.SetInterview("conv-repeat")
	sess := e.newSession("conv-repeat")
	if err := sess.agent.Chat(context.Background(), "hi, let's set up my profile"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	body := lastBody()
	if body == "" {
		t.Fatal("no model request captured")
	}
	for _, want := range []string{
		"Personalization interview",
		"ONE group at a time",
		"EVERY question is skippable",
		"save_personalization_profile",
		"explicit confirmation",
		"sensitive traits",
		// Repeat/edit re-entry with the saved answers (req 12.1).
		"REVISIT",
		"quantum computing",
		"balanced",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("interview model request missing %q", want)
		}
	}
}
