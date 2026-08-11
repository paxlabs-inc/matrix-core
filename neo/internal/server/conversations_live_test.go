// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Tests for the F1 "Durable live-run resume" contract:
//
//   1. engine.activeRunForConv reports the in-flight run id (or "") per
//      conversation — keyed off the authoritative runs registry, not the
//      broker (the broker lingers 2 minutes post-settlement so late
//      reconnects can replay).
//   2. GET /conversations/{id} carries the new `live_run` envelope field,
//      so a reload/relogin can immediately subscribe(replay:true) without
//      waiting for the /messages/async/<id> poll.
//   3. After settlement (registerRun + unregisterRun), `live_run` is "" even
//      while the broker topic still exists (no false-live flag from the
//      replay-grace linger).
//
// Plus an end-to-end walkthrough that exercises the SAME server-side wire
// contract a browser reload/hard-refresh/wipe-cookies-and-relogin depends on,
// via two independent HTTP clients sharing no state.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/conversation"
)

// newLiveRunTestEngine constructs the minimal Engine slice the live-run tests
// need: durable conversation store, broker, and the runs registry. No LLM /
// tool manager / pager wired (these tests do not drive the agent loop — they
// test the resume signal contract).
func newLiveRunTestEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{
		conv:   conversation.Open(t.TempDir()),
		broker: newBroker(),
		runs:   map[string]*run{},
	}
}

// newLiveRunTestServer wraps the engine in a Server. The backend URL never
// fires for the routes exercised here (handleConversations short-circuits to
// the durable store when conv is enabled), so any valid URL works.
func newLiveRunTestServer(t *testing.T, e *Engine) *Server {
	t.Helper()
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return s
}

// TestActiveRunForConv_ReturnsInflightRun pins the unit contract: in-flight
// only iff registerRun has fired and unregisterRun has not, scoped by convID.
func TestActiveRunForConv_ReturnsInflightRun(t *testing.T) {
	e := &Engine{runs: map[string]*run{}, broker: newBroker()}

	if got := e.activeRunForConv("conv_A"); got != "" {
		t.Errorf("no runs registered: want \"\", got %q", got)
	}
	if got := e.activeRunForConv(""); got != "" {
		t.Errorf("empty convID is a no-op: want \"\", got %q", got)
	}

	r1 := &run{id: "neo_alpha", convID: "conv_A"}
	e.registerRun(r1)
	e.broker.ensure(r1.id) // mirror dispatch — topic exists for the duration of the run

	if got := e.activeRunForConv("conv_A"); got != "neo_alpha" {
		t.Errorf("conv_A live: want \"neo_alpha\", got %q", got)
	}
	if got := e.activeRunForConv("conv_B"); got != "" {
		t.Errorf("conv_B is unrelated: want \"\", got %q", got)
	}

	// Settle. The broker topic LINGERS for 2 minutes (replay grace), but
	// activeRunForConv must NOT consult the broker — only the runs registry is
	// authoritative for "in flight".
	e.unregisterRun("neo_alpha")
	if !e.broker.has("neo_alpha") {
		t.Fatal("precondition: broker topic must linger post-unregister so the live-vs-broker distinction is meaningful")
	}
	if got := e.activeRunForConv("conv_A"); got != "" {
		t.Errorf("after unregister: want \"\", got %q", got)
	}
}

// TestHandleConversations_ReportsLiveRunWhileInFlight exercises the
// GET /conversations/{id} envelope: `live_run` must equal the in-flight
// intent_id, and the existing record fields (conversation_id, turns, updated)
// must continue to deserialise unchanged for older clients.
func TestHandleConversations_ReportsLiveRunWhileInFlight(t *testing.T) {
	e := newLiveRunTestEngine(t)
	srv := newLiveRunTestServer(t, e)

	const convID = "conv_live"
	const intentID = "neo_inflight"

	// Durable user turn stamped with the run id — the F1 persistence contract.
	e.conv.AppendUser(convID, intentID, "kick off a long task")

	// Register the live run (the same step session.dispatch performs after
	// minting the id; we register manually here because we are not driving the
	// agent loop in this unit test).
	r := &run{id: intentID, convID: convID}
	e.registerRun(r)
	t.Cleanup(func() { e.unregisterRun(intentID) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/conversations/"+convID, nil)
	srv.handleConversations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got["conversation_id"] != convID {
		t.Errorf("conversation_id: want %q got %v", convID, got["conversation_id"])
	}
	if got["live_run"] != intentID {
		t.Errorf("live_run: want %q got %v", intentID, got["live_run"])
	}
	if _, ok := got["updated"]; !ok {
		t.Error("envelope must still carry updated (older clients)")
	}
	turns, ok := got["turns"].([]interface{})
	if !ok || len(turns) != 1 {
		t.Fatalf("turns: want 1 got %v", got["turns"])
	}
	first := turns[0].(map[string]interface{})
	if first["intent_id"] != intentID {
		t.Errorf("user turn intent_id (durable backstop): want %q got %v", intentID, first["intent_id"])
	}
	if first["role"] != "user" {
		t.Errorf("first turn role: want \"user\" got %v", first["role"])
	}
}

// TestHandleConversations_NoLiveRunOnceSettled pins the inverse: a settled run
// (registerRun → unregisterRun) reports live_run="" immediately, even though
// the broker topic lingers 2 minutes for replay. The runs registry is the
// authoritative live signal.
func TestHandleConversations_NoLiveRunOnceSettled(t *testing.T) {
	e := newLiveRunTestEngine(t)
	srv := newLiveRunTestServer(t, e)

	const convID = "conv_settled"
	const intentID = "neo_done"
	e.conv.AppendUser(convID, intentID, "ask")
	e.conv.AppendAssistant(convID, intentID, "answer")

	r := &run{id: intentID, convID: convID}
	e.registerRun(r)
	e.broker.ensure(intentID) // mirror dispatch — topic exists for the run
	e.unregisterRun(intentID)

	// Precondition: the broker topic lingers post-settlement (the replay
	// grace). If activeRunForConv consulted the broker, this test would
	// erroneously fail — pinning the right authority.
	if !e.broker.has(intentID) {
		t.Fatal("precondition: broker topic must linger so the false-live trap is exercised")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/conversations/"+convID, nil)
	srv.handleConversations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := got["live_run"]; !ok {
		t.Error("envelope must always carry live_run (the resume contract)")
	} else if v != "" {
		t.Errorf("settled conversation: want live_run=\"\", got %v", v)
	}
}

// TestF1_ResumeWalkthrough_Integration is the recorded server-side walkthrough
// the F1 acceptance criteria require. A real httptest.NewServer hosts the real
// Server + real Engine + real conversation.Store. We simulate the three resume
// scenarios — refresh, hard-refresh, wipe-cookies-and-relogin — by:
//
//   - opening a SECOND, independent net/http client (no shared cookies / state)
//     after each "user action" and reissuing the same GET /conversations/{id}
//   - GET /events?intent_id=... requests the browser would emit. The Neo
//     daemon's /events route has no auth on the daemon side (Neo runs
//     per-daemon, the matrix-router fronts auth), so a clean client reattaches
//     identically to a refreshed one — this matches reality.
//
// Without an LLM available in tests we cannot drive the agent loop end-to-end;
// the test instead drives the SAME server-side wire contract the browser flow
// depends on: durable user turn with intent_id, live_run on
// GET /conversations/{id} while in flight, SSE replay across reconnect, and
// live_run="" after settlement.
func TestF1_ResumeWalkthrough_Integration(t *testing.T) {
	e := newLiveRunTestEngine(t)
	srv := newLiveRunTestServer(t, e)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const convID = "conv_walkthrough"
	const intentID = "neo_walkthrough"

	// Step 1 — POST /chat equivalent: the same two writes handleChat performs
	// AFTER the F1 reorder (start the run → AppendUser stamped with run.id).
	// We register manually because we are not driving the LLM here.
	r := &run{id: intentID, convID: convID}
	e.registerRun(r)
	e.broker.ensure(intentID)
	e.conv.AppendUser(convID, intentID, "do something long")
	t.Logf("step 1: registered run %s on conversation %s", intentID, convID)

	// Step 2 — GET /conversations/{id} reports live_run while in flight.
	{
		client := &http.Client{Timeout: 5 * time.Second}
		body := mustGetJSON(t, client, ts.URL+"/conversations/"+convID)
		if got := body["live_run"]; got != intentID {
			t.Fatalf("step 2: live_run mismatch — want %q got %v", intentID, got)
		}
		t.Logf("step 2: live_run=%q (in-flight)", body["live_run"])
	}

	// Publish a couple of agent events as the "still working" stream.
	e.broker.publish(intentID, "chat.assistant", "neo", map[string]interface{}{"text": "thinking..."})
	e.broker.publish(intentID, "tool.step", "neo", map[string]interface{}{"title": "fetching"})

	// Step 3 — simulate a "refresh" by opening an independent http.Client with
	// no shared headers/cookies and re-subscribing via SSE replay.
	{
		freshClient := &http.Client{Timeout: 5 * time.Second}
		events := readSSEFrames(t, freshClient, ts.URL+"/events/replay/"+intentID, 2, 3*time.Second)
		if len(events) < 2 {
			t.Fatalf("step 3 (refresh): want >=2 replayed events, got %d", len(events))
		}
		t.Logf("step 3 (refresh, replay): received %d events; types=%v", len(events), eventTypes(events))
	}

	// Step 4 — simulate "wipe cookies + relogin" with yet another fresh
	// http.Client (no shared transport / cookies). Neo's /events has no
	// per-conversation auth on the daemon side, so the reconnect lands
	// identically.
	{
		reloginClient := &http.Client{Timeout: 5 * time.Second}
		events := readSSEFrames(t, reloginClient, ts.URL+"/events/replay/"+intentID, 2, 3*time.Second)
		if len(events) < 2 {
			t.Fatalf("step 4 (wipe+relogin): want >=2 replayed events, got %d", len(events))
		}
		t.Logf("step 4 (wipe+relogin, fresh client, replay): received %d events; types=%v", len(events), eventTypes(events))

		body := mustGetJSON(t, reloginClient, ts.URL+"/conversations/"+convID)
		if got := body["live_run"]; got != intentID {
			t.Fatalf("step 4: live_run on relogin must still be the in-flight id; got %v", got)
		}
	}

	// Step 5 — settle the run; live_run must transition to "" immediately
	// (independent of the 2-minute broker linger).
	e.broker.publish(intentID, "message.complete", "neo", map[string]interface{}{"status": "completed"})
	e.broker.publish(intentID, "chat.assistant", "neo", map[string]interface{}{"text": "done"})
	e.conv.AppendAssistant(convID, intentID, "done")
	e.broker.closeRun(intentID)
	e.unregisterRun(intentID)

	{
		client := &http.Client{Timeout: 5 * time.Second}
		body := mustGetJSON(t, client, ts.URL+"/conversations/"+convID)
		if v, ok := body["live_run"]; !ok || v != "" {
			t.Fatalf("step 5: settled live_run must be \"\", got %v (ok=%v)", v, ok)
		}
		t.Logf("step 5: live_run=\"\" (settled)")
	}
}

// mustGetJSON GETs a URL and decodes the JSON body. Test fatal on any error.
func mustGetJSON(t *testing.T, c *http.Client, url string) map[string]interface{} {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d body=%s", url, resp.StatusCode, string(body))
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

// readSSEFrames opens an SSE connection and reads up to want frames or until
// the connection closes / the deadline expires. Used to verify replay
// reattachment across independent clients.
func readSSEFrames(t *testing.T, c *http.Client, url string, want int, deadline time.Duration) []Event {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d body=%s", url, resp.StatusCode, string(body))
	}

	done := time.After(deadline)
	out := make([]Event, 0, want)
	type frame struct {
		ev  Event
		err error
	}
	frames := make(chan frame, want)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				frames <- frame{err: err}
				return
			}
			frames <- frame{ev: ev}
		}
	}()
	for {
		select {
		case <-done:
			return out
		case f, ok := <-frames:
			if !ok {
				return out
			}
			if f.err != nil {
				t.Fatalf("decode SSE frame: %v", f.err)
			}
			out = append(out, f.ev)
			if len(out) >= want {
				return out
			}
		}
	}
}

func eventTypes(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}
