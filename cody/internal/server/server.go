// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server exposes codyd's HTTP surface. It sits behind the router (which owns
// the JWT and strips Authorization before forwarding), so codyd itself trusts
// the X-Matrix-User header the router injects — exactly the Neo posture.
type Server struct {
	engine *Engine
}

// New builds the HTTP front for an engine.
func New(engine *Engine) *Server { return &Server{engine: engine} }

// Handler returns the route mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/chat", s.handleChat)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/events/replay/", s.handleReplay)
	mux.HandleFunc("/messages/async/", s.handlePoll)
	mux.HandleFunc("/intents/", s.handleIntents)
	mux.HandleFunc("/conversations/", s.handleConversations)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "service": "codyd"})
}

// handleChat dispatches (or attaches to) a plan run and returns 202 with the
// stream/poll locations — the Neo envelope, so the client reuses one flow.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Message        string `json:"message"`
		ConversationID string `json:"conversation_id"`
		Mode           string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "message is required"})
		return
	}
	convID := strings.TrimSpace(req.ConversationID)
	if convID == "" {
		convID = "conv-" + newRunID()
	}
	runID, fresh, err := s.engine.Submit(convID, req.Message, req.Mode)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"conversation_id": convID,
		"kind":            "dispatch",
		"fresh":           fresh,
		"intent_id":       runID,
		"events_url":      "/events?intent_id=" + runID,
		"poll_url":        "/messages/async/" + runID,
	})
}

// handleEvents streams a run topic: replay past since_seq, then live, with a
// heartbeat comment every 15s.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("intent_id")
	if id == "" || !s.engine.broker.has(id) {
		http.Error(w, "unknown intent", http.StatusNotFound)
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since_seq"))
	s.streamSSE(w, r, id, since, true)
}

// handleReplay dumps the replay buffer then closes (finite).
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/events/replay/")
	if id == "" || !s.engine.broker.has(id) {
		http.Error(w, "unknown intent", http.StatusNotFound)
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since_seq"))
	s.streamSSE(w, r, id, since, false)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/messages/async/")
	run := s.engine.lookupRun(id)
	if run == nil {
		http.Error(w, "unknown intent", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"intent_id": id, "status": run.getStatus(),
	})
}

// handleIntents routes POST /intents/{id}/stop.
func (s *Server) handleIntents(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/intents/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		if !s.engine.Stop(parts[0]) {
			http.Error(w, "unknown intent", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"stopped": true})
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// handleConversations serves GET /conversations/{id}/trace — the durable
// workspace timeline the client replays through the live reducer on reopen.
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/conversations/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "trace" || r.Method != http.MethodGet {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	convID := parts[0]
	led, err := s.engine.readLedger(convID)
	if err != nil {
		http.Error(w, "unknown conversation", http.StatusNotFound)
		return
	}
	events, err := s.engine.trace.load(led.RunID)
	if err != nil {
		http.Error(w, "trace unavailable", http.StatusInternalServerError)
		return
	}
	live := ""
	if run := s.engine.lookupRun(led.RunID); run != nil && run.getStatus() == "running" {
		live = led.RunID
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"intent_id": led.RunID,
		"live_run":  live,
		"status":    led.Status,
		"mode":      led.Mode,
		"events":    events,
	})
}

// streamSSE writes replay-then-follow SSE frames.
func (s *Server) streamSSE(w http.ResponseWriter, r *http.Request, id string, since int, follow bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	replay, live, cancel := s.engine.broker.subscribe(id, since)
	defer cancel()
	for _, ev := range replay {
		writeEvent(w, ev)
	}
	flusher.Flush()
	if !follow || live == nil {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev, ok := <-live:
			if !ok {
				return
			}
			writeEvent(w, ev)
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
