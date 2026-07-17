// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/task"
	"matrix/neo/internal/tools"
)

// answerServer is a real OpenAI-SSE endpoint whose model always converges on a
// plain final answer — the minimal healthy model for driving the REAL agent
// loop end-to-end through dispatch/drive/superviseTask.
func answerServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"All done."},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// blockingServer is a real SSE endpoint that parks every model call until the
// request context is cancelled — the shape of a run the user stops mid-flight.
func blockingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body first: net/http only notices a client disconnect (and
		// cancels r.Context()) once the request body is consumed — what a real
		// provider does before generating. Without this the handler outlives
		// the cancelled call and Close hangs on the "active" connection.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testEngine(t *testing.T, gatewayURL string) *Engine {
	t.Helper()
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  gatewayURL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.TaskMaxRespawns = 0 // one attempt: these tests probe lifecycle seams, not retry policy
	return NewEngine(EngineOptions{
		Config:          cfg,
		Main:            client,
		Tools:           &tools.Manager{},
		ConversationDir: t.TempDir(),
		TaskDir:         t.TempDir(),
	})
}

// collectUntilClosed drains a real broker subscription (replay + live) until
// the topic closes or the deadline passes, returning every event seen.
func collectUntilClosed(t *testing.T, e *Engine, runID string, deadline time.Duration) []Event {
	t.Helper()
	replay, ch, cancel := e.broker.subscribe(runID, 0)
	defer cancel()
	events := append([]Event{}, replay...)
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timer.C:
			return events
		}
	}
}

func hasTerminal(events []Event, status string) bool {
	for _, ev := range events {
		if ev.Type == "message.complete" && ev.Fields["status"] == status {
			return true
		}
	}
	return false
}

func closingTurn(events []Event) (string, bool) {
	for _, ev := range events {
		if ev.Type == "chat.assistant" && ev.Fields["final"] == true {
			text, _ := ev.Fields["text"].(string)
			return text, true
		}
	}
	return "", false
}

// A user-stopped run must close with BOTH the interrupted terminal AND a calm
// final closing turn — never a bare terminal the client's grace window turns
// into "task stopped, try again".
func TestInterruptedRunEmitsClosingTurn(t *testing.T) {
	srv := blockingServer(t)
	e := testEngine(t, srv.URL)
	s := e.sessions.get("conv-interrupt")

	runID, fresh := s.submit("do a long thing")
	if !fresh {
		t.Fatalf("first submit must dispatch a fresh run")
	}

	// Let the run reach the parked model call, then stop it — the real
	// barge-in primitive (POST /intents/{id}/stop path).
	time.Sleep(100 * time.Millisecond)
	if got := s.interruptActive(); got != runID {
		t.Fatalf("interruptActive: got %q want %q", got, runID)
	}

	events := collectUntilClosed(t, e, runID, 10*time.Second)
	if !hasTerminal(events, "interrupted") {
		t.Fatalf("a stopped run must emit message.complete{status:interrupted}; events: %+v", events)
	}
	text, ok := closingTurn(events)
	if !ok {
		t.Fatalf("a stopped run must emit a final closing turn; events: %+v", events)
	}
	if !strings.Contains(text, "Stopped") {
		t.Fatalf("the interrupt closing turn should be the calm stop copy, got %q", text)
	}
	if st, k := e.tasks.Get("conv-interrupt"); !k || st.Status != task.StatusInterrupted {
		t.Fatalf("the task ledger must record interrupted, got %+v (ok=%v)", st, k)
	}
}

// A panic inside the supervised turn must not strand the stream: drive's
// backstop recovers, emits an honest closing turn + terminal, and closes the
// topic. The panic is REAL — the session's agent is gone (nil), so the real
// superviseTask dereferences it on the real code path.
func TestDrivePanicBackstopClosesStream(t *testing.T) {
	srv := answerServer(t)
	e := testEngine(t, srv.URL)
	s := e.sessions.get("conv-panic")
	s.agent = nil // corrupted state: the next attempt panics inside superviseTask

	r := s.dispatch("trigger the panic", false)

	events := collectUntilClosed(t, e, r.id, 10*time.Second)
	if !hasTerminal(events, "failed") {
		t.Fatalf("the panic backstop must still emit a terminal; events: %+v", events)
	}
	text, ok := closingTurn(events)
	if !ok {
		t.Fatalf("the panic backstop must emit a final closing turn; events: %+v", events)
	}
	if !strings.Contains(text, "Something went wrong") {
		t.Fatalf("the backstop closing turn should be the honest-failure copy, got %q", text)
	}
	if st, k := e.tasks.Get("conv-panic"); !k || st.Status != task.StatusCeiling {
		t.Fatalf("the task ledger must record the honest non-done status, got %+v (ok=%v)", st, k)
	}
}

// A message that lands in the finishing window (after the agent's last inbox
// drain, before the run is retired) must be re-dispatched as a continuation
// run — never silently stranded. clearActive owns the sweep under actMu.
func TestClearActiveRedispatchesLeftoverInbox(t *testing.T) {
	srv := answerServer(t)
	e := testEngine(t, srv.URL)
	s := e.sessions.get("conv-leftover")

	// The finishing-window interleaving, deterministically: the run is still
	// the active run (submit would route to its inbox) but its loop is done.
	r := &run{id: "neo_finishing", convID: s.id, sess: s}
	s.setActive(r)
	if id, fresh := s.submit("the late message"); fresh || id != r.id {
		t.Fatalf("a message during the finishing window must queue on the live run; got id=%q fresh=%v", id, fresh)
	}

	s.clearActive(r)

	s.actMu.Lock()
	successor := s.active
	s.actMu.Unlock()
	if successor == nil || successor == r {
		t.Fatalf("the stranded message must be re-dispatched as a continuation run")
	}
	if leftover := s.drainInput(); len(leftover) != 0 {
		t.Fatalf("the inbox must be empty after the sweep, got %v", leftover)
	}
	// The continuation is a REAL run: it drives the real agent to a real
	// terminal carrying the late message as its objective.
	events := collectUntilClosed(t, e, successor.id, 10*time.Second)
	if !hasTerminal(events, "completed") {
		t.Fatalf("the continuation run must reach a terminal; events: %+v", events)
	}
	if st, ok := e.tasks.Get(s.id); !ok || st.Objective != "the late message" {
		t.Fatalf("the continuation must carry the late message as its objective, got %+v (ok=%v)", st, ok)
	}
}

// An explicitly STOPPED run abandons its queued mid-task messages (the user
// superseded the task); the sweep must not resurrect them as a surprise run.
func TestClearActiveDiscardsInboxOnStoppedRun(t *testing.T) {
	srv := answerServer(t)
	e := testEngine(t, srv.URL)
	s := e.sessions.get("conv-stopped-discard")

	r := &run{id: "neo_stopped", convID: s.id, sess: s}
	r.stopped.Store(true)
	s.setActive(r)
	s.enqueueInput("a message from before the stop")

	s.clearActive(r)

	s.actMu.Lock()
	successor := s.active
	s.actMu.Unlock()
	if successor != nil {
		t.Fatalf("a stopped run's queued messages must be abandoned, not re-dispatched")
	}
	if leftover := s.drainInput(); len(leftover) != 0 {
		t.Fatalf("the inbox must still be cleared, got %v", leftover)
	}
}

// dropRunNarration filters a respawn seed: the current run's own assistant
// narration is removed, while user turns (including mid-run messages) and
// prior runs' turns all survive.
func TestDropRunNarration(t *testing.T) {
	r := &run{id: "neo_current"}
	turns := []conversation.Turn{
		{Role: "user", Text: "original ask", IntentID: "neo_current"},
		{Role: "assistant", Text: "earlier thread answer", IntentID: "neo_prior"},
		{Role: "assistant", Text: "Still on it — that pass hit a snag, so I'm taking another run at it.", IntentID: "neo_current"},
		{Role: "user", Text: "mid-run steer", IntentID: "neo_current"},
		{Role: "assistant", Text: "narration from this run", IntentID: "neo_current"},
	}

	got := dropRunNarration(turns, r)
	want := []string{"original ask", "earlier thread answer", "mid-run steer"}
	if len(got) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Text != w {
			t.Fatalf("turn %d: got %q want %q", i, got[i].Text, w)
		}
	}
	if out := dropRunNarration(turns, nil); len(out) != len(turns) {
		t.Fatalf("a nil run must filter nothing, got %d of %d", len(out), len(turns))
	}
}

// The respawn seed round-trip on the REAL conversation store: turns persisted
// by the current run's narration never re-enter the successor's seed, so a
// respawned agent is not primed to re-emit its own prior narration.
func TestRespawnSeedExcludesOwnNarration(t *testing.T) {
	store := conversation.Open(t.TempDir())
	convID := "conv-seed"
	r := &run{id: "neo_task_run"}

	store.AppendUser(convID, r.id, "build the thing")
	store.AppendAssistant(convID, "neo_old_run", "an answer from last week")
	store.AppendAssistant(convID, r.id, "Still on it — that pass hit a snag, so I'm taking another run at it.")
	store.AppendUser(convID, r.id, "also make it blue")

	turns := store.Recent(convID, conversation.DefaultRecallTurns)
	seeded := seedMessages(dropRunNarration(turns, r))

	for _, m := range seeded {
		if strings.Contains(m.Content, "Still on it") {
			t.Fatalf("the run's own narration leaked into the respawn seed: %+v", seeded)
		}
	}
	var texts []string
	for _, m := range seeded {
		texts = append(texts, m.Content)
	}
	joined := strings.Join(texts, " | ")
	for _, want := range []string{"build the thing", "an answer from last week", "also make it blue"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("seed lost a turn it must keep (%q): %s", want, joined)
		}
	}
}
