// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Tests for the F3 "durable workspace timeline" contract at the server seam:
//
//   1. The broker tap routes workspace events to the sidecar trace store, and
//      GET /conversations/{id}/trace serves them back after the run settles —
//      so a reopened thread rebuilds "Neo's Computer" instead of an empty
//      computer. The final answer + transient channels are NOT in the trace
//      (the conversation store owns the answer).
//   2. The tap is non-blocking: a burst of publishes never stalls the publish
//      path even though each event is also persisted.
//
// Both drive the REAL Engine + Server + trace.Store (no fakes).

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTraceTestEngine builds an Engine with BOTH the durable conversation store
// and the workspace trace enabled (each in its own temp dir), with the broker
// tap wired exactly as NewEngine does in production.
func newTraceTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	if !e.trace.Enabled() {
		t.Fatal("trace store must be enabled when TraceDir is set")
	}
	return e
}

// TestTrace_ServedOnReopen is the recorded F3 walkthrough equivalent: a
// multi-tool run publishes workspace events, settles, and a later
// GET /conversations/{id}/trace returns the full prior workspace (not an empty
// thread), with the answer/transient frames correctly excluded.
func TestTrace_ServedOnReopen(t *testing.T) {
	e := newTraceTestEngine(t)
	srv := newLiveRunTestServer(t, e)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const convID = "conv_trace"
	const intentID = "neo_trace_run"

	// Dispatch equivalent: register the run, open its topic, persist the user
	// turn stamped with the run id (the F1 contract the trace handler resolves
	// the latest run from).
	r := &run{id: intentID, convID: convID}
	e.registerRun(r)
	e.broker.ensure(intentID)
	e.conv.AppendUser(convID, intentID, "build me a small tool")

	// The live workspace stream — exactly what surfaceTool / the narrator emit.
	e.broker.publish(intentID, "tool.step", "neo", map[string]interface{}{"id": "s1", "surface": "terminal", "title": "Terminal", "running": false, "command": "go build ./..."})
	e.broker.publish(intentID, "tool.search", "neo", map[string]interface{}{"tool": "web_search", "query": "go embed fs", "results": []map[string]interface{}{{"title": "docs", "url": "https://go.dev"}}})
	e.broker.publish(intentID, "tool.media", "neo", map[string]interface{}{"kind": "image", "url": "/media/diagram.png"})
	e.broker.publish(intentID, "chat.assistant", "neo", map[string]interface{}{"text": "Let me check the build first.", "intent_id": intentID})            // non-final narration → kept
	e.broker.publish(intentID, "chat.delta", "neo", map[string]interface{}{"channel": "content", "text": "typing…"})                                       // transient → dropped
	e.broker.publish(intentID, "chat.assistant", "neo", map[string]interface{}{"text": "Done — here is your tool.", "final": true, "intent_id": intentID}) // final answer → NOT in trace (conversation store owns it)

	// Async persistence: drain the writer before settling/reading.
	e.trace.Flush()

	// Settle the run (the answer is persisted to the conversation store, the
	// broker topic is closed + scheduled for reclaim).
	e.conv.AppendAssistant(convID, intentID, "Done — here is your tool.")
	e.broker.closeRun(intentID)
	e.unregisterRun(intentID)

	// Reopen: GET the durable workspace trace.
	body := mustGetJSON(t, &http.Client{Timeout: 5 * time.Second}, ts.URL+"/conversations/"+convID+"/trace")

	if body["intent_id"] != intentID {
		t.Errorf("trace intent_id: want %q got %v", intentID, body["intent_id"])
	}
	if v, ok := body["live_run"]; !ok || v != "" {
		t.Errorf("settled run: want live_run=\"\" got %v (ok=%v)", v, ok)
	}
	rawEvents, ok := body["events"].([]interface{})
	if !ok {
		t.Fatalf("events must be a list, got %T (%v)", body["events"], body["events"])
	}
	var types []string
	for _, re := range rawEvents {
		ev := re.(map[string]interface{})
		types = append(types, ev["type"].(string))
	}
	// The workspace is rebuilt: step + search + media + the non-final
	// narration. The final answer and the transient delta are excluded.
	want := map[string]int{"tool.step": 1, "tool.search": 1, "tool.media": 1, "chat.assistant": 1}
	got := map[string]int{}
	for _, ty := range types {
		got[ty]++
	}
	for ty, n := range want {
		if got[ty] != n {
			t.Errorf("workspace trace: want %d %q events, got %d (all types=%v)", n, ty, got[ty], types)
		}
	}
	if got["chat.delta"] != 0 {
		t.Errorf("transient chat.delta must NOT be persisted to the trace, got %d", got["chat.delta"])
	}
	// The final answer text must live in the conversation store, never the
	// trace, so it isn't double-rendered as a narration step on reopen.
	for _, re := range rawEvents {
		ev := re.(map[string]interface{})
		if ev["type"] == "chat.assistant" {
			f, _ := ev["fields"].(map[string]interface{})
			if f != nil && f["final"] == true {
				t.Error("final answer must NOT be in the workspace trace (conversation store owns it)")
			}
		}
	}
}

// TestTrace_DoesNotBlockBrokerPublish confirms the F3 trace tap never stalls
// the publish hot path (m5/speed): a large burst of publishes through a broker
// with the trace tap installed completes well within a generous deadline. A
// synchronous per-event fsync — or a blocking enqueue — would blow it.
func TestTrace_DoesNotBlockBrokerPublish(t *testing.T) {
	e := newTraceTestEngine(t)
	const intentID = "neo_bench"
	e.broker.ensure(intentID)

	const n = 5000
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			e.broker.publish(intentID, "tool.step", "neo", map[string]interface{}{"id": "s", "n": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("publishing %d events with the trace tap installed exceeded 3s — the tap must be non-blocking (off the publish path)", n)
	}

	// And the events did get persisted (the tap isn't a no-op): after a flush,
	// the run's trace is non-empty (bounded by the retained cap).
	e.trace.Flush()
	if got := e.trace.Load(intentID); len(got) == 0 {
		t.Error("trace tap installed but nothing persisted")
	}
}

// TestParseTracePath pins the route matcher so /conversations/{id}/trace is
// recognised and ordinary conversation paths are not mistaken for it.
func TestParseTracePath(t *testing.T) {
	cases := []struct {
		path   string
		wantID string
		wantOK bool
	}{
		{"/conversations/conv_abc/trace", "conv_abc", true},
		{"/conversations/conv_abc", "", false},
		{"/conversations", "", false},
		{"/conversations/conv_abc/trace/extra", "", false},
		{"/intents/x/stop", "", false},
	}
	for _, c := range cases {
		id, ok := parseTracePath(c.path)
		if ok != c.wantOK || id != c.wantID {
			t.Errorf("parseTracePath(%q) = (%q,%v), want (%q,%v)", c.path, id, ok, c.wantID, c.wantOK)
		}
	}
}
