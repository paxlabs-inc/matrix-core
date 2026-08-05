// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// detstopModelServer scripts the brand-kit incident's shape against the REAL
// agent loop: the model emits answer-shaped working text alongside one tool
// call (suppressed while execution is in flight), then keeps returning
// EMPTY turns until the unified unproductive-attempt cap escalates — the
// unproductive-cap death that used to be delivered as a false "permission or
// limit" banner with the already-shown answer pasted in again.
func detstopModelServer(t *testing.T, narration string, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	frame := func(w http.ResponseWriter, delta map[string]any, finish string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{choice}})
		fmt.Fprintf(w, "data: %s\n", b)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			frame(w, map[string]any{"role": "assistant", "content": narration}, "")
			tc := map[string]any{"index": 0, "id": "call_read", "type": "function",
				"function": map[string]any{"name": "fs__read_file", "arguments": `{"path":"/tmp/probe.txt"}`}}
			frame(w, map[string]any{"tool_calls": []any{tc}}, "")
			frame(w, map[string]any{}, "tool_calls")
		} else {
			// An empty bare turn: no content, no tool calls — each one draws the
			// "continue or answer" guidance nudge until the cap escalates.
			frame(w, map[string]any{"role": "assistant", "content": ""}, "stop")
		}
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeterministicStopIsHonestAndNeverRepastes drives a REAL run to the
// unproductive-cap death and pins the fixed closing turn:
//
//  1. the copy says "I need your direction" (taken this as far as I can) —
//     never the false "permission, limit" blocker language, and
//  2. the suppressed working text may appear once in the honest closing turn,
//     never as repeated progress narration.
func TestDeterministicStopIsHonestAndNeverRepastes(t *testing.T) {
	const narration = "The brand-kit site looks complete already. Which part should I pick back up?"
	var (
		calls int
		mu    sync.Mutex
	)
	srv := detstopModelServer(t, narration, &calls, &mu)
	e := testEngine(t, srv.URL)
	// Isolate the seam under test: no epistemic side-channel model calls
	// perturbing the scripted call sequence.
	e.cfg.EpistemicPremises = false
	e.cfg.EpistemicPredictions = false
	s := e.sessions.get("conv-detstop")

	r := s.dispatch("pick back up on the brand kit site", false)
	events := collectUntilClosed(t, e, r.id, 20*time.Second)

	if !hasTerminal(events, "failed") {
		t.Fatalf("the deterministic stop must still terminate the stream; events: %+v", events)
	}
	text, ok := closingTurn(events)
	if !ok {
		t.Fatalf("the deterministic stop must emit a final closing turn; events: %+v", events)
	}
	if strings.Contains(text, "permission") || strings.Contains(text, "couldn't complete") {
		t.Fatalf("an unproductive-cap death must not read as a permission/limit blocker, got %q", text)
	}
	if !strings.Contains(text, "as far as I can") &&
		!strings.Contains(text, "couldn't fully verify") {
		t.Fatalf("the closing turn should own the stop honestly (need your direction), got %q", text)
	}
	if !strings.Contains(text, narration) {
		t.Fatalf("the honest closing turn lost the suppressed best-effort result:\n%q", text)
	}

	// The working text is public exactly once in the terminal honest partial,
	// never as in-flight narration and then again at close.
	seen := 0
	for _, ev := range events {
		if ev.Type != "chat.assistant" {
			continue
		}
		if s, _ := ev.Fields["text"].(string); strings.Contains(s, narration) {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the narration must appear exactly once in the stream, saw it %d times", seen)
	}
}
