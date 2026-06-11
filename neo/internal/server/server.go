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

	"matrix/neo/internal/conversation"
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
	// Mint/resume the session FIRST (it seeds from durable history that does
	// NOT yet include this message), THEN persist the user turn so a reload or
	// restart can list and reopen the thread.
	sess := s.engine.sessions.get(convID)
	s.engine.conv.AppendUser(convID, msg)
	run := sess.start(msg)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"conversation_id": convID,
		"kind":            "dispatch",
		"intent_id":       run.id,
		"events_url":      "/events?intent_id=" + run.id,
		"poll_url":        "/messages/async/" + run.id,
	})
}

// handleConversations serves GET /conversations (list) and
// GET /conversations/<id> (full turn log) from Neo's durable store. The shape
// mirrors the daemon's routes (and the web client's expectations) exactly:
// list → {"items":[summary...]}, detail → the record. When persistence is
// disabled it proxies to the daemon so the legacy store still answers.
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, rec)
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

// handleIntents intercepts only the gate-answer for a live Neo run; every other
// /intents/* route proxies to the daemon.
func (s *Server) handleIntents(w http.ResponseWriter, r *http.Request) {
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
