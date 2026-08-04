// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Tests for the autonomous Automatrix execution path (task 4.1, req 5.1–5.5).
//
// Everything under test is REAL — no fakes/mocks/stubs of the pager, agent, or
// session:
//
//   • the run drives the REAL restricted agent (agent.NewAutomatrix) over a
//     REAL tools.Manager, against the model HTTP endpoint behind an httptest
//     server (the established neo seam) — so the restricted surface, the
//     supervised loop, and the completion gate are the genuine code paths;
//   • the opportunity lifecycle is the REAL Neocortex pager under t.TempDir()
//     (hash embedder, fully offline) — RememberOpportunity / SetOpportunityStatus
//     / FailOpportunityAttempt / OpportunityByURI are the production methods.
//
// The assertions verify the REAL settle behavior: a genuine completion-gate
// pass marks the opportunity done; a failed attempt re-pends it with attempts++
// (bounded → dismissed at the ceiling); and the run can never reach the money
// surface (the advertised schema set is asserted clean).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// newRunTestEngine builds a real Engine with a real Neocortex pager (t.TempDir(),
// hash embedder), a real tools.Manager (whose full Schemas() advertises the
// money core_execute tool), and the main model pointed at modelURL. Returns the
// engine and the pager.
func newRunTestEngine(t *testing.T, modelURL string) (*Engine, *memory.Pager) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-automatrix-run-test"
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
		Config:     cfg,
		Main:       client,
		Cheap:      client,
		Tools:      &tools.Manager{}, // real Manager; full Schemas() advertises core_execute
		Pager:      pager,
		BackendURL: "http://127.0.0.1:1",
	})
	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})
	return e, pager
}

// doneModelServer returns an httptest server that answers any chat request with
// a single final-answer SSE chunk and no tool calls — the deterministic clean
// completion (light path: no tools touched → the turn finishes, agent.Chat
// returns nil → the supervisor reports task done).
func doneModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"Here is the finished draft you needed."},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rejectModelServer returns an httptest server that answers with a 400 — a
// deterministic provider reject (non-retryable, so the agent fails fast without
// burning the retry ladder). The supervised attempt ends without a clean
// completion → the supervisor reports a non-done terminal.
func rejectModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"InvalidParameter","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitOpportunity polls the REAL pager until the opportunity at uri satisfies
// pred (or the timeout elapses), returning the matching spec. The autonomous
// run settles on a background goroutine, so the test waits on the durable
// record rather than on internal timing.
func waitOpportunity(t *testing.T, p *memory.Pager, uri, desc string, pred func(memory.OpportunitySpec) bool, timeout time.Duration) memory.OpportunitySpec {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last memory.OpportunitySpec
	for time.Now().Before(deadline) {
		spec, ok, err := p.OpportunityByURI(context.Background(), uri)
		if err != nil {
			t.Fatalf("OpportunityByURI: %v", err)
		}
		if ok {
			last = spec
			if pred(spec) {
				return spec
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("opportunity %s did not become %s within %v (last status %q, attempts %d)", uri, desc, timeout, last.Status, last.Attempts)
	return last
}

func statusIs(want memory.OpportunityStatus) func(memory.OpportunitySpec) bool {
	return func(s memory.OpportunitySpec) bool { return s.Status == want }
}

// TestAutomatrixSession_RestrictedSurfaceHasNoMoney proves the structural
// guarantee at the SESSION boundary (req 3.2/5.3): the session newAutomatrixSession
// mints runs on the restricted surface, whose advertised tool schema set
// contains no value-moving/signing tool — while a NORMAL session over the SAME
// real tools.Manager advertises a strictly broader surface (spawn_subagents
// et al., which the restricted surface structurally excludes), so the
// restriction is real and specific to the autonomous path. core_execute is no
// longer the control: under the NO-BARRIER posture it is advertised only when
// something is actually escalated, which nothing is by default.
func TestAutomatrixSession_RestrictedSurfaceHasNoMoney(t *testing.T) {
	e, _ := newRunTestEngine(t, "")

	auto := e.newAutomatrixSession("conv-auto")
	if err := tools.AssertNoValueTransferTools(auto.agent.AdvertisedTools()); err != nil {
		t.Errorf("automatrix session advertised a value-moving tool: %v", err)
	}
	restricted := map[string]bool{}
	for _, s := range auto.agent.AdvertisedTools() {
		restricted[s.Function.Name] = true
	}
	if restricted["spawn_subagents"] || restricted[tools.CoreExecuteTool] {
		t.Errorf("automatrix surface must exclude decomposition and money delegation; got %v", restricted)
	}

	// Control: a normal session over the same manager advertises tools the
	// restricted surface excludes — proving the restriction is the automatrix
	// path's doing, not an empty manager.
	normal := e.newSession("conv-normal")
	normalHasSpawn := false
	for _, s := range normal.agent.AdvertisedTools() {
		if s.Function.Name == "spawn_subagents" {
			normalHasSpawn = true
			break
		}
	}
	if !normalHasSpawn {
		t.Error("precondition: a normal session must advertise spawn_subagents, else the restricted-surface test is vacuous")
	}
}

// TestRunAutomatrixOpportunity_GatePassMarksDone drives the runner end-to-end
// against a model that produces a clean completion. The opportunity must end up
// done (req 5.5: mark done only on a genuine pass), and the per-day-counter
// hand-off (scheduled→in_progress) must have happened (it can't reach done
// otherwise).
func TestRunAutomatrixOpportunity_GatePassMarksDone(t *testing.T) {
	srv := doneModelServer(t)
	e, pager := newRunTestEngine(t, srv.URL)

	uri, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary:              "Draft the quarterly update doc you mentioned",
		Rationale:            "user said they owe a quarterly update next week",
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
	got := waitOpportunity(t, pager, uri, "done", statusIs(memory.OpportunityDone), 10*time.Second)
	if got.Attempts != 0 {
		t.Errorf("a clean completion must not bump attempts, got %d", got.Attempts)
	}
}

// TestRunAutomatrixOpportunity_FailLeavesPendingAndIncrementsAttempts drives the
// runner against a model that rejects every call. The run cannot pass the gate,
// so the opportunity must be left PENDING (re-runnable on a later wake) with
// attempts incremented — and nothing is surfaced to the user (req 5.5).
func TestRunAutomatrixOpportunity_FailLeavesPendingAndIncrementsAttempts(t *testing.T) {
	srv := rejectModelServer(t)
	e, pager := newRunTestEngine(t, srv.URL)
	// Single attempt per run (no respawn) so the failure terminal is fast and
	// deterministic; the agent's own retry ladder is short-circuited by the 400.
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
	// A failed attempt re-pends with attempts++ — wait for that distinct state
	// (the record STARTS pending@0, so we must wait for the re-pend, not the
	// mere status).
	got := waitOpportunity(t, pager, uri, "re-pended with attempts==1",
		func(s memory.OpportunitySpec) bool {
			return s.Status == memory.OpportunityPending && s.Attempts == 1
		}, 10*time.Second)
	if got.Attempts != 1 {
		t.Errorf("a failed attempt must increment attempts to 1, got %d", got.Attempts)
	}
}

// TestRunAutomatrixOpportunity_DismissedAtAttemptCeiling proves the bound: once
// a failing opportunity reaches the attempt ceiling it is DISMISSED rather than
// re-queued forever (req 5.5 "eventually dismiss it"). Seeded one attempt below
// the ceiling so a single failed run trips it.
func TestRunAutomatrixOpportunity_DismissedAtAttemptCeiling(t *testing.T) {
	srv := rejectModelServer(t)
	e, pager := newRunTestEngine(t, srv.URL)
	e.cfg.SuperviseTasks = false

	uri, err := pager.RememberOpportunity(context.Background(), memory.OpportunitySpec{
		Summary:              "Compile the migration notes into a checklist",
		EligibleAutonomous:   true,
		Confidence:           0.8,
		OriginConversationID: "conv-origin",
		Attempts:             automatrixMaxAttempts - 1,
	})
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}
	opp, _, _ := pager.OpportunityByURI(context.Background(), uri)

	if err := e.RunAutomatrixOpportunity(context.Background(), opp); err != nil {
		t.Fatalf("RunAutomatrixOpportunity: %v", err)
	}
	got := waitOpportunity(t, pager, uri, "dismissed", statusIs(memory.OpportunityDismissed), 10*time.Second)
	if got.Attempts != automatrixMaxAttempts {
		t.Errorf("dismissal must land at the attempt ceiling %d, got %d", automatrixMaxAttempts, got.Attempts)
	}
}

// TestRunAutomatrixOpportunity_RefusesWithoutURI proves the runner refuses an
// opportunity it cannot track (no durable URI) rather than running blind — so
// the wake handler does NOT advance the per-day counter for it.
func TestRunAutomatrixOpportunity_RefusesWithoutURI(t *testing.T) {
	e, _ := newRunTestEngine(t, "")
	err := e.RunAutomatrixOpportunity(context.Background(), memory.OpportunitySpec{
		Summary:            "an opportunity with no URI",
		EligibleAutonomous: true,
	})
	if err == nil {
		t.Error("RunAutomatrixOpportunity must return an error for an opportunity with no URI")
	}
}
