// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func fastClient(base string, ap Approver) *Client {
	return New(Options{
		BaseURL:      base,
		Approver:     ap,
		PollInterval: time.Millisecond,
		MaxWait:      5 * time.Second,
		Timeout:      2 * time.Second,
	})
}

func TestRunHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "i1"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed",
				"result": map[string]string{"answer": "all done"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := fastClient(srv.URL, nil).Run(context.Background(), "do x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "all done" {
		t.Errorf("answer = %q, want 'all done'", out)
	}
}

func TestRunServicesApprovalGateInline(t *testing.T) {
	var mu sync.Mutex
	answered := false
	var askedQuestion string
	var askedOptions []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		done := answered
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "i2"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []map[string]any{
				{"node_id": "n1", "question": "Approve spend of 5 PAX?", "options": []string{"yes", "no"}},
			}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gates/") && strings.HasSuffix(r.URL.Path, "/answer"):
			mu.Lock()
			answered = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": map[string]string{"answer": "settled"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "working"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	approver := func(ctx context.Context, nodeID, q string, opts []string) (bool, string) {
		mu.Lock()
		askedQuestion = q
		askedOptions = opts
		mu.Unlock()
		return true, ""
	}

	out, err := fastClient(srv.URL, approver).Run(context.Background(), "send 5 PAX")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "settled" {
		t.Errorf("answer = %q, want 'settled'", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(askedQuestion, "Approve spend") {
		t.Errorf("approver was not asked the gate question, got %q", askedQuestion)
	}
	if len(askedOptions) != 2 {
		t.Errorf("approver should receive the gate options, got %v", askedOptions)
	}
}

func TestRunPropagatesPipelineFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "i3"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "failed", "error": "compile blew up"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, err := fastClient(srv.URL, nil).Run(context.Background(), "do x")
	if err == nil || !strings.Contains(err.Error(), "compile blew up") {
		t.Errorf("expected the pipeline error to propagate, got %v", err)
	}
}

// conflictBody is the daemon's real 409 body for a deterministic-id collision.
func conflictBody(intentID string) string {
	return `{"error":"intent \"` + intentID + `\" already exists in async registry"}`
}

// TestRunAttachesOnConflict drives the SPARK fix end-to-end against a real
// (httptest) daemon: the FIRST submission 409s ("already exists"), and the
// delegate must ATTACH to the in-flight intent — poll its status and service
// its pending gate — rather than failing or re-submitting. It asserts the
// existing gate surfaces (the approver is asked), the run reaches its terminal
// answer, and NO second submission is made.
func TestRunAttachesOnConflict(t *testing.T) {
	var mu sync.Mutex
	var submits int
	answered := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		done := answered
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			mu.Lock()
			submits++
			mu.Unlock()
			// The intent is already in flight: 409 with the existing id.
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(conflictBody("spark1")))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []map[string]any{
				{"node_id": "n1", "question": "Approve spend of 5 PAX?", "options": []string{"yes", "no"}},
			}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gates/") && strings.HasSuffix(r.URL.Path, "/answer"):
			mu.Lock()
			answered = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": map[string]string{"answer": "attached and settled"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "working"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var asked bool
	approver := func(ctx context.Context, nodeID, q string, opts []string) (bool, string) {
		mu.Lock()
		asked = true
		mu.Unlock()
		return true, ""
	}

	out, err := fastClient(srv.URL, approver).Run(context.Background(), "launch SPARK")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "attached and settled" {
		t.Errorf("answer = %q, want 'attached and settled'", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if submits != 1 {
		t.Errorf("submits = %d, want exactly 1 (the conflict; attach must NOT re-submit)", submits)
	}
	if !asked {
		t.Error("attach must surface the existing pending gate to the approver")
	}
}

// TestRunAttachToTerminalReturnsOutcome verifies that when the conflicting
// intent is ALREADY terminal at attach time, the delegate returns its outcome
// in plain language and never re-submits fresh prose.
func TestRunAttachToTerminalReturnsOutcome(t *testing.T) {
	var mu sync.Mutex
	var submits int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			mu.Lock()
			submits++
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(conflictBody("spark2")))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": map[string]string{"answer": "already settled earlier"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := fastClient(srv.URL, nil).Run(context.Background(), "launch SPARK")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "already settled earlier" {
		t.Errorf("answer = %q, want the terminal outcome", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if submits != 1 {
		t.Errorf("submits = %d, want exactly 1 (no re-submit of a terminal intent)", submits)
	}
}

// TestAttachGateClaimDedupsAcrossClients verifies the cross-attach guard
// (req 2.4): two delegate clients sharing one GateClaim guard both attach to
// the same in-flight intent, but the gate is answered exactly once.
func TestAttachGateClaimDedupsAcrossClients(t *testing.T) {
	var mu sync.Mutex
	var answers int
	answered := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		done := answered
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(conflictBody("spark3")))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []map[string]any{
				{"node_id": "n1", "question": "Approve spend of 5 PAX?", "options": []string{"yes", "no"}},
			}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gates/") && strings.HasSuffix(r.URL.Path, "/answer"):
			mu.Lock()
			answers++
			answered = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": map[string]string{"answer": "settled once"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "working"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// One shared guard handed to both clients (mirrors engine.claimGate).
	var gmu sync.Mutex
	claimed := map[string]bool{}
	guard := func(intentID, nodeID string) bool {
		gmu.Lock()
		defer gmu.Unlock()
		key := intentID + "\x00" + nodeID
		if claimed[key] {
			return false
		}
		claimed[key] = true
		return true
	}

	var apMu sync.Mutex
	var approveCalls int
	approver := func(ctx context.Context, nodeID, q string, opts []string) (bool, string) {
		apMu.Lock()
		approveCalls++
		apMu.Unlock()
		return true, ""
	}

	mk := func() *Client {
		return New(Options{
			BaseURL:      srv.URL,
			Approver:     approver,
			GateClaim:    guard,
			PollInterval: time.Millisecond,
			MaxWait:      5 * time.Second,
			Timeout:      2 * time.Second,
		})
	}

	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = mk().Run(context.Background(), "launch SPARK")
		}(i)
	}
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("client %d Run: %v", i, errs[i])
		}
		if outs[i] != "settled once" {
			t.Errorf("client %d answer = %q, want 'settled once'", i, outs[i])
		}
	}
	mu.Lock()
	gateAnswers := answers
	mu.Unlock()
	if gateAnswers != 1 {
		t.Errorf("gate answered %d times, want exactly 1 (cross-attach claim must dedup)", gateAnswers)
	}
	apMu.Lock()
	calls := approveCalls
	apMu.Unlock()
	if calls != 1 {
		t.Errorf("approver called %d times, want exactly 1 (loser attach must skip answering)", calls)
	}
}

func TestClarifyText(t *testing.T) {
	if got := clarifyText(json.RawMessage(`{"question":"which token?"}`)); got != "which token?" {
		t.Errorf("clarifyText = %q", got)
	}
	if got := clarifyText(json.RawMessage(`{"other":"x"}`)); got == "" {
		t.Error("clarifyText should fall back to the raw text")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("under-limit unchanged")
	}
	if got := truncate("abcdefghij", 4); !strings.HasSuffix(got, "…") {
		t.Errorf("truncate should add an ellipsis: %q", got)
	}
}

// sseData renders one daemon-shaped SSE event ({seq,ts,phase,type,fields}) in
// the wire format the daemon emits: a single "data: <json>\n\n" frame.
func sseData(t *testing.T, seq int, typ, phase string, fields map[string]any) []byte {
	t.Helper()
	ev := map[string]any{
		"seq":    seq,
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"phase":  phase,
		"type":   typ,
		"fields": fields,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal sse event: %v", err)
	}
	return []byte("data: " + string(b) + "\n\n")
}

// startSSE writes the SSE response headers and returns the flusher. The caller
// then writes "data: ..." frames and flushes after each.
func startSSE(w http.ResponseWriter) (http.Flusher, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return fl, true
}

// TestDelegate_DrivenBySSEEvents verifies the SSE subscription — not the
// poller — drives gate + terminal transitions, with sub-100ms gate-to-action
// latency. The poller is stretched to 500ms so it cannot win the race; the
// terminal status is only flipped once the SSE intent.attest frame is sent, so
// a poll-only client (the pre-change behaviour) never sees "completed" and
// times out.
func TestDelegate_DrivenBySSEEvents(t *testing.T) {
	var mu sync.Mutex
	var gateSentAt, answerAt time.Time
	var answered, completed bool
	var eventsHit int
	answerSig := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "sse1"})
		case r.Method == http.MethodGet && r.URL.Path == "/events":
			mu.Lock()
			eventsHit++
			mu.Unlock()
			fl, ok := startSSE(w)
			if !ok {
				return
			}
			// Gate becomes pending shortly after subscribe.
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			gateSentAt = time.Now()
			mu.Unlock()
			_, _ = w.Write(sseData(t, 1, "gate.invoked", "walk", map[string]any{
				"intent_id": "sse1", "node_id": "n1",
				"question": "Approve spend of 5 PAX?",
				"options":  []string{"yes", "no"},
			}))
			fl.Flush()
			// Hold the stream open until the gate is answered (or the client
			// disconnects), then emit the terminal frame.
			select {
			case <-answerSig:
			case <-r.Context().Done():
				return
			}
			mu.Lock()
			completed = true
			mu.Unlock()
			_, _ = w.Write(sseData(t, 2, "intent.attest", "lifecycle", map[string]any{"intent_id": "sse1"}))
			fl.Flush()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			mu.Lock()
			done := answered
			mu.Unlock()
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []map[string]any{
				{"node_id": "n1", "question": "Approve spend of 5 PAX?", "options": []string{"yes", "no"}},
			}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gates/") && strings.HasSuffix(r.URL.Path, "/answer"):
			mu.Lock()
			answered = true
			answerAt = time.Now()
			mu.Unlock()
			select {
			case <-answerSig:
			default:
				close(answerSig)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			mu.Lock()
			done := completed
			mu.Unlock()
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "completed",
					"result": map[string]string{"answer": "sse-done"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "running"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var apMu sync.Mutex
	var approveCalls int
	approver := func(ctx context.Context, nodeID, q string, opts []string) (bool, string) {
		apMu.Lock()
		approveCalls++
		apMu.Unlock()
		return true, ""
	}

	// 500ms poller cannot beat the ~20ms SSE gate event; it only exists as the
	// safety net.
	c := New(Options{
		BaseURL:      srv.URL,
		Approver:     approver,
		PollInterval: 500 * time.Millisecond,
		MaxWait:      3 * time.Second,
		Timeout:      2 * time.Second,
	})
	out, err := c.Run(context.Background(), "send 5 PAX")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "sse-done" {
		t.Errorf("answer = %q, want 'sse-done'", out)
	}

	apMu.Lock()
	calls := approveCalls
	apMu.Unlock()
	if calls != 1 {
		t.Errorf("approver called %d times, want 1", calls)
	}
	mu.Lock()
	gSent, aRecv, hits := gateSentAt, answerAt, eventsHit
	mu.Unlock()
	if hits == 0 {
		t.Error("expected the SSE /events stream to be subscribed")
	}
	if !gSent.IsZero() && !aRecv.IsZero() {
		if d := aRecv.Sub(gSent); d >= 100*time.Millisecond {
			t.Errorf("gate-to-action latency = %s, want <100ms (SSE-driven)", d)
		}
	} else {
		t.Error("gate was never sent/answered via SSE")
	}
}

// TestDelegate_FallsBackToPollOnStreamError verifies that when the SSE stream
// is unavailable the bounded-reconnect exhausts and the retained poller still
// drives the run to a terminal state. A pre-change (poll-only) client reaches
// terminal too, but it never attempts /events — so asserting /events was hit
// makes this red before the SSE change and green after.
func TestDelegate_FallsBackToPollOnStreamError(t *testing.T) {
	var mu sync.Mutex
	var eventsHit int
	start := time.Now()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "fb1"})
		case r.Method == http.MethodGet && r.URL.Path == "/events":
			mu.Lock()
			eventsHit++
			mu.Unlock()
			http.Error(w, "stream unavailable", http.StatusInternalServerError)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			// Stay "running" until the SSE goroutine has actually attempted
			// /events (and a minimum dwell has elapsed), so the fallback
			// terminal is only declared after SSE was observed unavailable.
			mu.Lock()
			hits := eventsHit
			mu.Unlock()
			if hits == 0 || time.Since(start) < 80*time.Millisecond {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "running"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed",
				"result": map[string]string{"answer": "fb-done"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := fastClient(srv.URL, nil).Run(context.Background(), "do x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "fb-done" {
		t.Errorf("answer = %q, want 'fb-done' (fallback must reach terminal)", out)
	}
	mu.Lock()
	hits := eventsHit
	mu.Unlock()
	if hits == 0 {
		t.Error("expected SSE /events to be attempted before falling back to poll")
	}
}

// TestDelegate_NoDuplicateApproval verifies that with both the SSE event path
// and the safety-net poller active (the event+poll overlap), a single gate is
// approved exactly once. The shared answered set serialised through the main
// loop guarantees this; the test also asserts /events was active so it is red
// before the SSE change.
func TestDelegate_NoDuplicateApproval(t *testing.T) {
	var mu sync.Mutex
	var answered, completed bool
	var eventsHit int
	answerSig := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "dup1"})
		case r.Method == http.MethodGet && r.URL.Path == "/events":
			mu.Lock()
			eventsHit++
			mu.Unlock()
			fl, ok := startSSE(w)
			if !ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
			_, _ = w.Write(sseData(t, 1, "gate.invoked", "walk", map[string]any{
				"intent_id": "dup1", "node_id": "n1",
				"question": "Approve spend of 5 PAX?",
				"options":  []string{"yes", "no"},
			}))
			fl.Flush()
			select {
			case <-answerSig:
			case <-r.Context().Done():
				return
			}
			mu.Lock()
			completed = true
			mu.Unlock()
			_, _ = w.Write(sseData(t, 2, "intent.attest", "lifecycle", map[string]any{"intent_id": "dup1"}))
			fl.Flush()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			mu.Lock()
			done := answered
			mu.Unlock()
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []map[string]any{
				{"node_id": "n1", "question": "Approve spend of 5 PAX?", "options": []string{"yes", "no"}},
			}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gates/") && strings.HasSuffix(r.URL.Path, "/answer"):
			mu.Lock()
			answered = true
			mu.Unlock()
			select {
			case <-answerSig:
			default:
				close(answerSig)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			mu.Lock()
			done := completed
			mu.Unlock()
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "completed",
					"result": map[string]string{"answer": "dup-done"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "running"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var apMu sync.Mutex
	var approveCalls int
	approver := func(ctx context.Context, nodeID, q string, opts []string) (bool, string) {
		apMu.Lock()
		approveCalls++
		apMu.Unlock()
		return true, ""
	}

	// fastClient: 1ms poller + SSE both active → event+poll overlap.
	out, err := fastClient(srv.URL, approver).Run(context.Background(), "send 5 PAX")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "dup-done" {
		t.Errorf("answer = %q, want 'dup-done'", out)
	}
	apMu.Lock()
	calls := approveCalls
	apMu.Unlock()
	if calls != 1 {
		t.Errorf("approver called %d times, want exactly 1 (no duplicate approval under event+poll overlap)", calls)
	}
	mu.Lock()
	hits := eventsHit
	mu.Unlock()
	if hits == 0 {
		t.Error("expected SSE /events to be active alongside the poller")
	}
}
