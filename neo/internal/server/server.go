// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"matrix/construct/backchannel"
	"matrix/construct/schema/primitives"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/trace"
)

// Server is Neo's HTTP front. It owns the conversational surface (POST /chat +
// the SSE event stream) and reverse-proxies every other route to the
// co-located MCL daemon, so the matrix-router and existing clients address it
// exactly like the daemon — Neo simply becomes the default agent behind /chat.
type Server struct {
	engine  *Engine
	backend *url.URL
	proxy   *httputil.ReverseProxy
}

// New builds the server. backendURL is the co-located daemon (e.g.
// http://127.0.0.1:8081) that handles core_execute and all non-conversational
// routes.
func New(engine *Engine, backendURL string) (*Server, error) {
	u, err := url.Parse(strings.TrimRight(backendURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("neo/server: bad backend url %q: %w", backendURL, err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	// FlushInterval -1 streams proxied SSE/long-poll responses immediately
	// (matches the matrix-router posture for /events passthrough).
	rp.FlushInterval = -1
	return &Server{engine: engine, backend: u, proxy: rp}, nil
}

// Handler returns the routed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", s.handleChat)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/events/replay/", s.handleReplay)
	mux.HandleFunc("/messages/async/", s.handleAsyncPoll)
	mux.HandleFunc("/intents/", s.handleIntents)
	// Neo owns conversation history now (it persists every Neo turn); serve the
	// list/detail from Neo's own durable store instead of proxying to the
	// daemon, which never saw a Neo conversation. Falls through to the proxy
	// when persistence is disabled (dev/CLI) so the daemon's store still works.
	mux.HandleFunc("/conversations", s.handleConversations)
	mux.HandleFunc("/conversations/", s.handleConversations)
	// Media plane: generated + uploaded images/video/audio live on the agent's
	// machine volume. These are Neo-owned routes (the daemon has never heard of
	// them), registered before the catch-all proxy.
	mux.HandleFunc("/media/", s.handleMedia)
	mux.HandleFunc("/upload", s.handleUpload)
	// Automatrix control surface (task 6.1): the per-user opt-in toggle (which
	// creates/cancels the AUTOMATRIX alarm), the opportunity management queue,
	// and the completion inbox. Neo-owned routes registered before the catch-all
	// proxy (the daemon has never heard of them).
	mux.HandleFunc("/automatrix/", s.handleAutomatrix)
	mux.HandleFunc("/", s.proxy.ServeHTTP) // healthz, /messages, /memory, /tools, … → daemon
	return mux
}

// chatRequest mirrors the daemon's POST /chat body (only the fields Neo needs).
type chatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	convID := req.ConversationID
	if convID == "" {
		convID = synthConvID(msg)
	}
	// Chronos AUTOMATRIX idle-wake: NOT a normal user turn. The engine wake
	// handler re-reads the per-user opt-in, defers on a busy session, enforces
	// the per-day cap, picks one eligible opportunity (handed to the supervised
	// restricted-surface run), and reschedules the next jittered wake. It
	// produces no live conversational run of its own, so respond with an
	// accepted, run-less envelope rather than dispatching a turn.
	if s.engine.MaybeHandleAutomatrixWake(r.Context(), convID, msg) {
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"conversation_id": convID,
			"kind":            "automatrix_wake",
		})
		return
	}
	// Mint/resume the session FIRST (it seeds from durable history that does
	// NOT yet include this message), then SUBMIT the message. F5: submit does
	// not interrupt — if a turn is already in flight it QUEUES the message into
	// the live agent's inbox (delivered at the agent's next tool-call boundary)
	// and returns the SAME active run id, so the client keeps watching one live
	// stream instead of cancelling the work. Otherwise it dispatches a fresh
	// run. Either way we persist the user turn STAMPED WITH that run id (F1): a
	// reload/relogin during a still-live run reads the trailing user turn's
	// intent_id (and the `live_run` field on GET /conversations/{id}) to decide
	// whether to subscribe(replay:true) and reattach — no more "thread looks
	// done while agent is still working".
	sess := s.engine.sessions.get(convID)
	runID, _ := sess.submit(msg)
	s.engine.conv.AppendUser(convID, runID, msg)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"conversation_id": convID,
		"kind":            "dispatch",
		"intent_id":       runID,
		"events_url":      "/events?intent_id=" + runID,
		"poll_url":        "/messages/async/" + runID,
	})
}

// handleConversations serves GET /conversations (list) and
// GET /conversations/<id> (full turn log) from Neo's durable store. The shape
// mirrors the daemon's routes (and the web client's expectations) exactly:
// list → {"items":[summary...]}, detail → the record. When persistence is
// disabled it proxies to the daemon so the legacy store still answers.
//
// F1 wire shape (additive, byte-compatible): the detail response carries a
// sibling `live_run` field — the in-flight intent_id for this conversation, or
// "" once the run has settled. The existing record fields
// (conversation_id, title, turns, updated) are emitted unchanged so older
// clients keep deserialising. New clients use `live_run` to subscribe to the
// SSE stream with replay on reopen/relogin without waiting for the
// /messages/async/<id> poll — the durable resume signal that closes the
// "thread looks done while agent is still working" defect.
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	// F3: GET /conversations/{id}/trace serves the durable workspace timeline
	// ("Neo's Computer") so a reopened thread rebuilds its tool steps / search
	// cards / media / surfaces / swarm windows — not just the text turns. This
	// is a Neo-owned route the daemon never had, so it is intercepted before
	// the conv.Enabled proxy fallback (a trace-disabled store returns an empty
	// timeline rather than a daemon 404).
	if id, ok := parseTracePath(r.URL.Path); ok {
		s.handleTrace(w, r, id)
		return
	}
	if !s.engine.conv.Enabled() {
		s.proxy.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/conversations"), "/")
	if id == "" {
		items := s.engine.conv.List()
		if items == nil {
			items = []conversation.Summary{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
		return
	}
	if strings.ContainsAny(id, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conversation id required"})
		return
	}
	rec := s.engine.conv.Get(id)
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	writeJSON(w, http.StatusOK, conversationDetailEnvelope(rec, s.engine.activeRunForConv(id)))
}

// conversationDetail is the GET /conversations/<id> wire envelope. The
// embedded *conversation.Record promotes the existing record fields
// (conversation_id, title, turns, updated) verbatim so older clients
// deserialise identically; LiveRun is a sibling addition older clients ignore
// safely. LiveRun is the in-flight intent_id for the conversation, or "" once
// the run has settled (the durable resume signal).
type conversationDetail struct {
	*conversation.Record
	LiveRun string `json:"live_run"`
}

func conversationDetailEnvelope(rec *conversation.Record, liveRun string) conversationDetail {
	return conversationDetail{Record: rec, LiveRun: liveRun}
}

// conversationTrace is the GET /conversations/{id}/trace wire envelope (F3).
// IntentID is the run whose workspace this is (the in-flight run, else the
// conversation's most recent run); LiveRun mirrors the detail envelope's signal
// so the client can decide whether to also subscribe(replay) for live updates;
// Events are the persisted workspace SSE frames, oldest-first, replayed through
// the client's existing reducer to rebuild "Neo's Computer".
type conversationTrace struct {
	IntentID string        `json:"intent_id"`
	LiveRun  string        `json:"live_run"`
	Events   []trace.Event `json:"events"`
}

// handleTrace serves the durable workspace timeline for a conversation's most
// recent run. A trace-disabled store, an unknown conversation, or a run with no
// persisted workspace all return 200 with an empty events list so the client
// degrades to a text-only thread rather than erroring.
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" || strings.ContainsAny(id, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conversation id required"})
		return
	}
	runID := s.engine.latestRunForConv(id)
	events := s.engine.trace.Load(runID)
	if events == nil {
		events = []trace.Event{}
	}
	writeJSON(w, http.StatusOK, conversationTrace{
		IntentID: runID,
		LiveRun:  s.engine.activeRunForConv(id),
		Events:   events,
	})
}

// handleEvents serves the live SSE stream for a Neo run, or proxies to the
// daemon when the intent_id belongs to a daemon-side run (dashboard dispatch).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("intent_id")
	if id == "" || !s.engine.broker.has(id) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	since := atoiSafe(r.URL.Query().Get("since_seq"))
	s.streamSSE(w, r, id, since, true)
}

// handleReplay dumps a Neo run's buffered events and closes (finite), so the
// client can reconnect: replay history, then open the live stream with
// since_seq. Daemon intents fall through to the proxy.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/events/replay/")
	if id == "" || !s.engine.broker.has(id) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	since := atoiSafe(r.URL.Query().Get("since_seq"))
	s.streamSSE(w, r, id, since, false)
}

// handleAsyncPoll answers the poll_url that POST /chat advertises for a Neo run
// (GET /messages/async/<id>). The web client polls it on reload to decide
// whether to reconnect a still-live run; without this it would be proxied to
// the daemon, which has never heard of a neo_ intent and returns 404. Daemon
// async jobs (non-Neo intents) fall through to the proxy unchanged.
func (s *Server) handleAsyncPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/messages/async/")
	if id == "" || !s.engine.broker.has(id) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	// lookupRun is non-nil only while the run is in flight; once it settles the
	// run is unregistered but its topic lingers (replay grace), so a poll in
	// that window reports completed rather than a misleading "running".
	status := "completed"
	if s.engine.lookupRun(id) != nil {
		status = "running"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"intent_id":  id,
		"status":     status,
		"request":    map[string]string{"prose": ""},
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// handleIntents intercepts the gate-answer and the Construct Ask-answer for a
// live Neo run; every other /intents/* route proxies to the daemon.
func (s *Server) handleIntents(w http.ResponseWriter, r *http.Request) {
	// Explicit interrupt: POST /intents/{id}/stop cancels a live Neo turn (the
	// "stop" button). Barge-in via a new message is automatic; this is the
	// deliberate path to stop without sending one.
	if id, ok := parseStopPath(r.URL.Path); ok && r.Method == http.MethodPost {
		run := s.engine.lookupRun(id)
		if run == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live run for that intent"})
			return
		}
		run.sess.interrupt(run)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "intent_id": id, "status": "interrupting"})
		return
	}
	// Construct Ask back-channel: POST /intents/{id}/asks/{ask_id}/answer.
	if id, askID, ok := parseAskAnswerPath(r.URL.Path); ok && r.Method == http.MethodPost && s.engine.lookupRun(id) != nil {
		s.handleAskAnswer(w, r, id, askID)
		return
	}
	id, nodeID, ok := parseGateAnswerPath(r.URL.Path)
	if !ok || r.Method != http.MethodPost || s.engine.lookupRun(id) == nil {
		s.proxy.ServeHTTP(w, r)
		return
	}
	var body struct {
		Approved bool   `json:"approved"`
		Answer   string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	run := s.engine.lookupRun(id)
	if run == nil || !run.sess.answerGate(nodeID, gateAnswer{approved: body.Approved, answer: body.Answer}) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending gate for that node"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "node_id": nodeID, "approved": body.Approved})
}

// handleAskAnswer delivers a typed human response to a parked Construct Ask
// (the back-channel of the construct_render(kind=ask) tool call). It validates
// the posted AskResponse against the Ask it answers BEFORE delivering it, so a
// malformed or off-contract answer is rejected (the run stays parked) rather
// than resuming the agent with garbage. A stale/duplicate/unknown ask_id is a
// 404. The caller has already confirmed the run is live.
func (s *Server) handleAskAnswer(w http.ResponseWriter, r *http.Request, id, askID string) {
	run := s.engine.lookupRun(id)
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live run for that intent"})
		return
	}
	ask, ok := run.sess.pendingAsk(askID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending ask for that id"})
		return
	}
	var resp primitives.AskResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	if err := backchannel.ValidateResponse(ask, &resp); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !run.sess.answerAsk(askID, &resp) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "ask already answered or expired"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "intent_id": id, "ask_id": askID})
}

// streamSSE writes the run's events as text/event-stream. When live is true it
// replays buffered events (seq>since) then follows live until the run closes or
// the client disconnects; when false it writes the buffer and returns.
func (s *Server) streamSSE(w http.ResponseWriter, r *http.Request, id string, since int, live bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	replay, ch, cancel := s.engine.broker.subscribe(id, since)
	defer cancel()

	for _, ev := range replay {
		if !writeEvent(w, ev) {
			return
		}
	}
	flusher.Flush()

	if !live || ch == nil {
		return
	}
	ctx := r.Context()
	// Comment-ping every 15s so the client's heartbeat watchdog (30s in
	// lib/realtime/sse.ts) doesn't abort+reconnect during long tool/model gaps
	// — a research turn can run many seconds between visible events. Matches
	// the daemon's documented 15s ping cadence.
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			if !writeEvent(w, ev) {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false
	}
	return true
}

// parseGateAnswerPath matches /intents/{id}/gates/{nid}/answer.
func parseGateAnswerPath(p string) (id, nodeID string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 5 || parts[0] != "intents" || parts[2] != "gates" || parts[4] != "answer" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// parseAskAnswerPath matches /intents/{id}/asks/{ask_id}/answer.
func parseAskAnswerPath(p string) (id, askID string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 5 || parts[0] != "intents" || parts[2] != "asks" || parts[4] != "answer" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// parseStopPath matches /intents/{id}/stop.
func parseStopPath(p string) (id string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 3 || parts[0] != "intents" || parts[2] != "stop" {
		return "", false
	}
	return parts[1], true
}

// parseTracePath matches /conversations/{id}/trace (F3 durable workspace).
func parseTracePath(p string) (id string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 3 || parts[0] != "conversations" || parts[2] != "trace" {
		return "", false
	}
	return parts[1], true
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
