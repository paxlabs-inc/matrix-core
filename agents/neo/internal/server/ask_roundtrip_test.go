// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Integration test for the Construct Ask back-channel round-trip (task 7.2,
// Property 6: Ask liveness + safety; Requirements 5.4, 5.5, 5.6, 17.6).
//
// This drives the REAL components end-to-end — no fakes:
//
//   • the real tools.Manager Ask path (construct_render(kind=ask) →
//     dispatchAsk), so the parked agent receives the answer as the recorded
//     INPUT (backchannel.Summarize result),
//   • the real engine back-channel (respondAsk: registers a waiter, emits the
//     Ask surface + ask.awaiting, parks on the buffered(1) channel, on answer
//     patches the Ask to its settled state via backchannel.Answered →
//     construct.surface.patch and returns the response),
//   • the real session waiter store (registerAsk / pendingAsk / answerAsk —
//     deletes the waiter under lock, sends on a buffered(1) channel for an
//     exactly-once resume),
//   • the real HTTP receiver (POST /intents/{id}/asks/{ask_id}/answer →
//     handleAskAnswer: 400 off-contract via backchannel.ValidateResponse,
//     404 unknown ask_id, 409 duplicate/no-waiter),
//   • the real backchannel contract (ValidateResponse / Summarize / Answered).
//
// The asserted properties:
//   1. A run parks on an Ask (a waiter is registered; the Ask surface is
//      emitted and ask.awaiting is announced).                       — R5.4
//   2. Answering once with a VALID response resumes the parked run EXACTLY
//      once, the agent receives the answer as the recorded INPUT
//      (Summarize), and the Ask surface is patched to its settled/answered
//      state (Answered → construct.surface.patch).             — R5.4, R5.6
//   3. A DUPLICATE answer for the same ask_id is rejected and does not
//      double-resume.                                                — R5.5
//   4. A MALFORMED / off-contract response is rejected (400) and leaves the
//      run parked with the waiter intact.                            — R5.5
//   5. An EXPIRED/cancelled or UNKNOWN ask_id is rejected and leaves the run
//      parked (or, for expiry, never resumes it with an answer).     — R5.5
//
// Together these are the end-to-end liveness + safety check the MVP requires
// before it is considered complete.                                  — R17.6

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"centra/packages/construct/backchannel"
	"centra/packages/construct/projection"
	"centra/packages/construct/schema"
	"centra/packages/construct/schema/primitives"
	"centra/agents/neo/internal/tools"
)

// --- fixture -----------------------------------------------------------------

// askResult captures one return of the construct_render(ask) tool call (the
// recorded INPUT the parked agent reads to resume).
type askResult struct {
	content string
	isErr   bool
	err     error
}

// eventCollector accumulates every event the run publishes so the test can
// assert the emitted Ask surface, the ask.awaiting park, and the settled patch.
type eventCollector struct {
	mu  sync.Mutex
	evs []Event
}

func (c *eventCollector) add(ev Event) {
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
}

func (c *eventCollector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.evs))
	copy(out, c.evs)
	return out
}

// waitForEvent polls the collected events until one of type typ appears or the
// deadline expires.
func (c *eventCollector) waitForEvent(typ string, timeout time.Duration) (Event, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, ev := range c.snapshot() {
			if ev.Type == typ {
				return ev, true
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return Event{}, false
}

// parkedAsk is a fully wired, REAL Ask round-trip parked and waiting for an
// answer. The construct_render(ask) tool call runs on a background goroutine
// (exactly as the agent loop would invoke it) and delivers its single result
// onto resultCh once answered / cancelled / expired.
type parkedAsk struct {
	e        *Engine
	ts       *httptest.Server
	sess     *session
	run      *run
	intentID string
	askID    string
	ask      *primitives.Ask
	cancel   context.CancelFunc
	resultCh chan askResult
	events   *eventCollector
}

// askArgs builds the construct_render arguments for a `choose` Ask with two
// options. ParseRender turns these into a validated schema.Surface, exactly as
// the model's tool call would.
func askArgs(askID string) map[string]interface{} {
	return map[string]interface{}{
		"kind": "ask",
		"id":   askID,
		"payload": map[string]interface{}{
			"ask_kind": "choose",
			"prompt":   "Where should I deploy the build?",
			"options": []map[string]interface{}{
				{"id": "staging", "label": "Staging"},
				{"id": "prod", "label": "Production"},
			},
			"required": true,
		},
	}
}

// newParkedAsk wires a real Engine + Server + session + run, drives the real
// Ask tool path on a goroutine, and blocks until the run is genuinely parked
// (a waiter registered and ask.awaiting announced). It registers cleanup so the
// parked goroutine is always released at test end.
func newParkedAsk(t *testing.T, intentID, askID, convID string) *parkedAsk {
	t.Helper()

	e := &Engine{broker: newBroker(), runs: map[string]*run{}}
	sess := &session{id: convID, engine: e, asks: map[string]*askWaiter{}}
	r := &run{id: intentID, convID: convID, sess: sess}
	e.registerRun(r)
	e.broker.ensure(intentID)

	srv, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Derive the Ask the way the tool call does, so the expected recorded INPUT
	// (Summarize) is computed from the SAME surface the agent emits.
	surface, err := projection.ParseRender(askArgs(askID))
	if err != nil {
		t.Fatalf("ParseRender: %v", err)
	}
	if surface.Kind != schema.KindAsk || surface.Ask == nil {
		t.Fatalf("expected an ask surface, got kind=%q", surface.Kind)
	}

	// Capture every event the run publishes (the emitted surface, the
	// ask.awaiting park, the settled patch, ask.answered).
	collector := &eventCollector{}
	_, ch, subCancel := e.broker.subscribe(intentID, 0)
	t.Cleanup(subCancel)
	go func() {
		for ev := range ch {
			collector.add(ev)
		}
	}()

	// Drive the REAL Ask tool path: construct_render(kind=ask) → dispatchAsk →
	// respondAsk. A zero-value Manager is sufficient because Dispatch routes
	// construct_render before touching the MCP registry, and dispatchAsk only
	// needs the surface emitter + ask responder we wire here.
	m := &tools.Manager{}
	m.SetSurfaceEmitter(e.emitConstructSurface)
	m.SetAskResponder(e.respondAsk)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	resultCh := make(chan askResult, 1)
	go func() {
		content, isErr, derr := m.Dispatch(withRun(ctx, r), tools.ConstructRenderTool, askArgs(askID))
		resultCh <- askResult{content: content, isErr: isErr, err: derr}
	}()

	// Block until the run is genuinely parked: the waiter is registered AND the
	// park has been announced. Both are the real liveness signal (R5.4).
	waitParked(t, sess, askID)
	if _, ok := collector.waitForEvent("ask.awaiting", 2*time.Second); !ok {
		t.Fatal("run never announced ask.awaiting — it did not park on the Ask")
	}
	if _, ok := collector.waitForEvent("construct.surface", 2*time.Second); !ok {
		t.Fatal("the Ask surface was never emitted")
	}

	return &parkedAsk{
		e:        e,
		ts:       ts,
		sess:     sess,
		run:      r,
		intentID: intentID,
		askID:    askID,
		ask:      surface.Ask,
		cancel:   cancel,
		resultCh: resultCh,
		events:   collector,
	}
}

// waitParked blocks until a waiter for askID is registered, or fails the test.
func waitParked(t *testing.T, sess *session, askID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := sess.pendingAsk(askID); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the Ask waiter to register (run never parked)")
}

// postAnswer POSTs a typed AskResponse to the back-channel and returns the HTTP
// status code.
func postAnswer(t *testing.T, base, intentID, askID string, body map[string]interface{}) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal answer: %v", err)
	}
	url := base + "/intents/" + intentID + "/asks/" + askID + "/answer"
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// --- tests -------------------------------------------------------------------

// TestAskRoundTrip_AnswerOnceResumesExactlyOnce is the core liveness + safety
// walkthrough: park on an Ask, answer once with a valid response, and assert
// the run resumes EXACTLY once with the answer as the recorded INPUT, the Ask
// surface is patched to its settled state, and a duplicate answer is rejected
// without a second resume. (R5.4, R5.5, R5.6, R17.6.)
func TestAskRoundTrip_AnswerOnceResumesExactlyOnce(t *testing.T) {
	pa := newParkedAsk(t, "neo_ask_run", "ask_choose_1", "conv_ask")

	// Answer ONCE with a valid choose response (passes ValidateResponse).
	if code := postAnswer(t, pa.ts.URL, pa.intentID, pa.askID, map[string]interface{}{"choice": "prod"}); code != http.StatusOK {
		t.Fatalf("valid answer: want 200, got %d", code)
	}

	// The parked run resumes exactly once and receives the answer as the
	// recorded INPUT — the backchannel.Summarize of the SAME ask + response.
	var got askResult
	select {
	case got = <-pa.resultCh:
	case <-time.After(3 * time.Second):
		t.Fatal("parked run did not resume after a valid answer")
	}
	if got.err != nil {
		t.Fatalf("tool dispatch returned a transport error: %v", got.err)
	}
	if got.isErr {
		t.Fatalf("a valid answer must resume cleanly, got in-band error: %q", got.content)
	}
	wantInput := backchannel.Summarize(pa.ask, &primitives.AskResponse{Choice: "prod"})
	if got.content != wantInput {
		t.Errorf("recorded INPUT mismatch:\n  want %q\n  got  %q", wantInput, got.content)
	}

	// The Ask surface is patched to its settled/answered state: a
	// construct.surface.patch carrying the answered Ask (Response set), plus
	// the ask.answered announcement. (R5.6.)
	patch, ok := pa.events.waitForEvent("construct.surface.patch", 3*time.Second)
	if !ok {
		t.Fatal("the Ask surface was never patched to its settled state")
	}
	assertSettledPatch(t, patch, pa.askID)
	if _, ok := pa.events.waitForEvent("ask.answered", 2*time.Second); !ok {
		t.Error("the run never announced ask.answered")
	}

	// Exactly-once: the waiter is consumed, so a second/duplicate answer for
	// the same ask_id is rejected and produces NO second resume. (R5.5.)
	if _, stillPending := pa.sess.pendingAsk(pa.askID); stillPending {
		t.Error("the Ask waiter must be retired after the first answer (exactly-once)")
	}
	dupCode := postAnswer(t, pa.ts.URL, pa.intentID, pa.askID, map[string]interface{}{"choice": "staging"})
	if dupCode == http.StatusOK {
		t.Errorf("a duplicate answer must be rejected, got 200")
	}
	if dupCode != http.StatusNotFound && dupCode != http.StatusConflict {
		t.Errorf("duplicate answer: want 404 or 409, got %d", dupCode)
	}
	select {
	case second := <-pa.resultCh:
		t.Fatalf("duplicate answer caused a SECOND resume (content=%q) — exactly-once violated", second.content)
	case <-time.After(250 * time.Millisecond):
		// No second resume — correct.
	}
}

// TestAskRoundTrip_SafetyRejections is the table-driven safety half of Property
// 6: every off-contract, unknown, or expired answer is rejected and leaves the
// run parked (the waiter intact and the tool call still blocked), with NO
// resume carrying that bad answer. (R5.5.)
func TestAskRoundTrip_SafetyRejections(t *testing.T) {
	cases := []struct {
		name     string
		askID    string // the ask_id to POST to ("" → use the real one)
		body     map[string]interface{}
		wantCode int
		expire   bool // cancel the run first, simulating an expired/closed ask
	}{
		{
			name:     "malformed empty choice is off-contract",
			body:     map[string]interface{}{"choice": ""},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "choice not among offered options is off-contract",
			body:     map[string]interface{}{"choice": "moon"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "wrong-shape response (input value for a choose) is off-contract",
			body:     map[string]interface{}{"value": "prod"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "unknown ask_id is rejected",
			askID:    "ask_does_not_exist",
			body:     map[string]interface{}{"choice": "prod"},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "expired/cancelled ask_id is rejected",
			body:     map[string]interface{}{"choice": "prod"},
			wantCode: http.StatusNotFound,
			expire:   true,
		},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			intentID := "neo_safety_run"
			realAskID := "ask_safety"
			pa := newParkedAsk(t, intentID, realAskID, "conv_safety")

			postAskID := c.askID
			if postAskID == "" {
				postAskID = realAskID
			}

			if c.expire {
				// Simulate expiry/cancellation: the run's context is cancelled,
				// so respondAsk unwinds and retires the waiter — exactly the
				// state an expired Ask reaches. The tool call returns an in-band
				// "didn't answer" result, never a resume with a posted answer.
				pa.cancel()
				select {
				case res := <-pa.resultCh:
					if !res.isErr {
						t.Fatalf("a cancelled/expired ask must NOT resume with an answer; got clean content %q", res.content)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("cancelled ask never unwound the parked tool call")
				}
				// The waiter must be gone after expiry.
				if _, ok := pa.sess.pendingAsk(realAskID); ok {
					t.Fatal("an expired ask must retire its waiter")
				}
			}

			code := postAnswer(t, pa.ts.URL, intentID, postAskID, c.body)
			if code != c.wantCode {
				t.Fatalf("[%d] %s: want status %d, got %d", i, c.name, c.wantCode, code)
			}

			if !c.expire {
				// The run stays parked: the waiter is intact and the tool call
				// is still blocked (no resume slipped through).
				if _, ok := pa.sess.pendingAsk(realAskID); !ok {
					t.Error("a rejected answer must leave the Ask waiter parked")
				}
				select {
				case res := <-pa.resultCh:
					t.Fatalf("a rejected answer must NOT resume the run; got content=%q isErr=%v", res.content, res.isErr)
				case <-time.After(200 * time.Millisecond):
					// Still parked — correct.
				}
			}
		})
	}
}

// assertSettledPatch verifies a construct.surface.patch carries the answered
// Ask (its Response folded on) for the target ask id — the settled state the
// renderer flips to. Mirrors backchannel.Answered.
func assertSettledPatch(t *testing.T, patch Event, askID string) {
	t.Helper()
	if id, _ := patch.Fields["id"].(string); id != askID {
		t.Errorf("settled patch target id: want %q, got %v", askID, patch.Fields["id"])
	}
	raw, ok := patch.Fields["surface"]
	if !ok {
		t.Fatal("settled patch carries no surface payload")
	}
	s, ok := raw.(*schema.Surface)
	if !ok {
		t.Fatalf("settled patch surface has unexpected type %T", raw)
	}
	if s.Kind != schema.KindAsk || s.Ask == nil {
		t.Fatalf("settled patch must be an Ask surface, got kind=%q", s.Kind)
	}
	if s.Ask.Response == nil {
		t.Error("settled patch Ask must carry the human Response (answered state)")
	}
}
