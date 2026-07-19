// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"matrix/construct/backchannel"
	"matrix/construct/schema/primitives"
	cxself "matrix/cortex/self"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/runrecord"
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
	s := &Server{engine: engine, backend: u, proxy: rp}
	// Epistemic-core req.2.1: persist the derived capability surface into the
	// durable self-model at boot, so the resident capability section every
	// agent carries reflects what this process actually serves.
	s.recordSurface()
	return s, nil
}

// routeFact is one Neo-owned route: the mux pattern, its handler, and a
// one-line EXTERNAL semantic. The table drives BOTH mux registration
// (Handler) and the derived capability surface (SurfaceFacts), so the
// self-model's "what can reach me" facts cannot drift from what the process
// actually serves (epistemic-core req.2.1/2.2). semantic "" keeps a route out
// of the external surface (internal plumbing).
type routeFact struct {
	pattern  string
	semantic string
	handler  http.HandlerFunc
}

// routes is the single source of Neo-owned route registrations. Comments about
// each route's ownership rationale live with its handler.
func (s *Server) routes() []routeFact {
	return []routeFact{
		{"/chat", "POST /chat — send a user message; the reply streams back over the SSE event stream (this is the ONLY way to talk to you)", s.handleChat},
		{"/events", "GET /events — the SSE event stream carrying your replies, tool steps, and workspace events", s.handleEvents},
		{"/events/replay/", "GET /events/replay/{conversation} — replay a conversation's durable event trace", s.handleReplay},
		{"/messages/async/", "", s.handleAsyncPoll},
		{"/intents/", "", s.handleIntents},
		// Global kill switch: interrupts EVERY live Neo run on this
		// (single-tenant) daemon — the "Stop all" control.
		{"/halt", "POST /halt — interrupt every live run (the user's stop-all control)", s.handleHalt},
		// Neo owns conversation history (it persists every Neo turn); serve
		// list/detail from Neo's own durable store instead of proxying to the
		// daemon, which never saw a Neo conversation.
		{"/conversations", "GET /conversations — list conversation history from your durable store", s.handleConversations},
		{"/conversations/", "", s.handleConversations},
		// Memory reads must come from Neo's pager. The co-located daemon owns a
		// separate Cortex actor and therefore cannot see Neo's learned memories.
		{"/memory/recent", "GET /memory/recent — list your learned memories from Neo's durable store", s.handleMemoryRecent},
		{"/memory/types", "GET /memory/types — count your learned memories by type", s.handleMemoryTypes},
		{"/memory/search", "POST /memory/search — search your learned memories", s.handleMemorySearch},
		{"/memory/mutate", "POST /memory/mutate — create, update, replace, or delete your learned memories through a typed bounded request", s.handleMemoryMutate},
		// Privacy controls (PRIV-01): default-off durable-memory consent,
		// full export, and receipt-backed delete-all (fail-closed until ORACLE
		// erasure lands). Backed by Neo's own pager.
		{"/memory/consent", "GET/PUT /memory/consent — read or set your default-off durable-memory opt-in", s.handleMemoryConsent},
		{"/memory/export", "GET /memory/export — export every current learned memory as JSON", s.handleMemoryExport},
		{"/memory/delete-all", "DELETE /memory/delete-all — request receipt-backed erasure of all your memory", s.handleMemoryDeleteAll},
		// Personalization profile (PRIV-01/ORACLE req 13) on Neo's own actor.
		{"/personalization", "GET/PUT/DELETE /personalization — read, save, or delete your single personalization profile", s.handlePersonalization},
		{"/personalization/", "", s.handlePersonalization},
		// Media plane: generated + uploaded images/video/audio live on the
		// agent's machine volume.
		{"/media/", "GET /media/{id} — serve a generated or uploaded media artifact from your machine volume", s.handleMedia},
		{"/upload", "POST /upload — receive a user file onto your machine volume", s.handleUpload},
		{"/voice/session/", "", s.handleVoiceSession},
		// Automatrix control surface: per-user opt-in toggle, opportunity
		// queue, completion inbox.
		{"/automatrix/", "GET/POST /automatrix/* — the proactive-task (Automatrix) control surface", s.handleAutomatrix},
		// Morning-brief control surface: opt-in toggle, schedule (time/tz/days/
		// length/sections), and pause for the ORACLE personalized brief.
		{"/brief/", "GET/PUT /brief/settings — the morning-brief schedule + opt-in control surface", s.handleBrief},
		{"/integrations/telegram", "GET/PUT/DELETE /integrations/telegram — connect a private Telegram chat to this user's Neo", s.handleTelegram},
		// Personalization interview entry (ORACLE task 5.3): mint an interview-
		// flagged conversation; the interview itself runs in the normal chat.
		{"/interview/", "POST /interview/start — start (or repeat) the guided personalization interview", s.handleInterview},
		// Coding workbench environment surface: project-scoped workspace
		// tree / file read / atomic write / diff / bounded exec.
		{"/workspace/", "GET/POST /workspace/* — the coding workbench surface (tree, file read/write, raw download, upload, diff, exec)", s.handleWorkspace},
		// Workbench project registry: projects are workspace subdirectories.
		{"/projects", "GET/POST /projects — the workbench project registry", s.handleProjects},
		{"/projects/", "", s.handleProject},
		// Self-model observability (self-model req.13): read-only inspection
		// of the resident self-summary + failure patterns.
		{"/diag/self-model", "GET /diag/self-model — read-only inspection of your current self-model", s.handleDiagSelfModel},
	}
}

// surfaceIs / surfaceIsNot are the architectural is / is-not facts of the
// serving surface, declared HERE — beside the route table they qualify, owned
// by the package that IS the surface — and written into the durable self-model
// (epistemic-core req.2.1): the arena incident's false premise ("I can wire my
// own API up directly / I have an OpenAI-compatible endpoint") must collide
// with these resident facts at the moment it would form.
var (
	surfaceIs = []string{
		"You are the conversational agent behind the Matrix daemon's HTTP front: clients send POST /chat and read your streamed replies over SSE GET /events.",
		"Every route not on your list reverse-proxies to the co-located MCL daemon (healthz, /messages, /tools, the core_execute plane).",
	}
	surfaceIsNot = []string{
		"You have NO OpenAI-compatible endpoint: /v1/chat/completions, /v1/completions, and /v1/models are NOT served. Nothing can integrate with you as an OpenAI-style API.",
		"You cannot be wired into another system as a raw LLM/completions backend — external integration happens ONLY via POST /chat + the SSE /events stream.",
	}
)

// SurfaceFacts derives the capability-surface facts from the SAME route table
// Handler registers, plus the declared is/is-not facts. Derived, never
// hand-written into the prompt, so the resident section cannot drift from the
// live serving surface (req.2.2).
func (s *Server) SurfaceFacts() cxself.SurfaceFacts {
	facts := cxself.SurfaceFacts{
		Is:    append([]string(nil), surfaceIs...),
		IsNot: append([]string(nil), surfaceIsNot...),
	}
	for _, rt := range s.routes() {
		if rt.semantic != "" {
			facts.API = append(facts.API, rt.semantic)
		}
	}
	return facts
}

// recordSurface persists the derived surface facts into the durable structural
// self-model (best-effort: a fresh install without a cortex pager simply keeps
// the honest "unknown" rendering).
func (s *Server) recordSurface() {
	if s.engine == nil || s.engine.pager == nil {
		return
	}
	_, _ = s.engine.pager.WriteSurfaceFacts(context.Background(), s.SurfaceFacts())
}

// Handler returns the routed mux, registered from the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, rt.handler)
	}
	mux.HandleFunc("/", s.proxy.ServeHTTP) // healthz, /messages, /memory, /tools, … → daemon
	// Q2 warm-on-open: the FIRST inbound request (whatever the app hits on
	// open) triggers a one-shot, non-blocking warm of the embedder + HNSW
	// substrate so the first message's relevance recall isn't on the cold path.
	// Engine.Warm is idempotent + async, so this never adds latency to a request.
	return s.observeHTTP(s.warmOnFirstRequest(mux))
}

// warmOnFirstRequest wraps next so every inbound request first pokes
// Engine.Warm (a no-op after the one-shot fires), then serves normally.
func (s *Server) warmOnFirstRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.engine.Warm()
		next.ServeHTTP(w, r)
	})
}

// selfModelDiag is the read-only inspection payload: what the agent currently
// believes about ITSELF — its resident structural summary (how it is built) and
// its active failure-pattern memories (how it tends to fail) — plus the scope
// and context limit that back them. It is a pure projection of the shared cortex
// self-model; it never mutates it.
type selfModelDiag struct {
	Identity        string               `json:"identity"`
	ResidentSummary string               `json:"resident_summary"`
	Scope           []string             `json:"scope,omitempty"`
	ContextLimit    int                  `json:"context_limit,omitempty"`
	StructuralURI   string               `json:"structural_uri,omitempty"`
	FailurePatterns []failurePatternDiag `json:"failure_patterns"`
}

type failurePatternDiag struct {
	Statement string `json:"statement"`
	URI       string `json:"uri,omitempty"`
}

// handleDiagSelfModel serves GET /diag/self-model: the current resident
// self-summary and the active failure-pattern memories (self-model req.13.1). It
// is read-only and side-effect free (req.13.2) — it resolves the shared
// self-model through the pager's read path and never signs, mutates the model,
// or touches plan/walk.
func (s *Server) handleDiagSelfModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil || s.engine.pager == nil {
		http.Error(w, "self-model unavailable (no cortex pager)", http.StatusServiceUnavailable)
		return
	}
	model, err := s.engine.pager.SelfModel(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("self-model read failed: %v", err), http.StatusInternalServerError)
		return
	}
	resp := selfModelDiag{
		Identity:        model.Identity,
		ResidentSummary: model.Structural.Summary,
		Scope:           model.Structural.Scope,
		ContextLimit:    model.Structural.ContextLimit,
		StructuralURI:   model.StructuralURI,
		FailurePatterns: make([]failurePatternDiag, 0, len(model.FailurePatterns)),
	}
	for _, fp := range model.FailurePatterns {
		resp.FailurePatterns = append(resp.FailurePatterns, failurePatternDiag{Statement: fp.Statement, URI: fp.URI})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// chatRequest mirrors the daemon's POST /chat body (only the fields Neo
// needs). Project is the workbench project tag: a coding-surface chat names
// its project so History/Workspace scope per project (additive; absent for
// dashboard chats).
type chatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
	Project        string `json:"project,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
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
	dispatchMsg, audio, voiceTurn, voiceErr := s.engine.prepareVoiceInput(r.Context(), msg)
	if voiceErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": voiceErr.Error()})
		return
	}
	msg = dispatchMsg
	if s.engine.MaybeHandleEpisodicSweep(msg) {
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"conversation_id": convID,
			"kind":            "episodic_sweep",
		})
		return
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
	// Chronos MORNING_BRIEF idle-wake (ORACLE task 5.4): NOT a normal user
	// turn either. The engine wake handler re-reads the durable schedule, does
	// zero network when disabled/paused, enforces the day + single-per-local-
	// date guards, and dispatches the supervised restricted-surface brief run
	// (delivered out of band as a durable turn + inbox record). It produces no
	// live conversational run, so respond with an accepted, run-less envelope.
	if s.engine.MaybeHandleMorningBriefWake(r.Context(), convID, msg) {
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"conversation_id": convID,
			"kind":            "morning_brief_wake",
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
	// Tag the conversation with its workbench project BEFORE the session is
	// minted: rebuildAgent reads the tag to inject the coding-workspace
	// context into the agent's system prompt, so the very first turn must
	// already see it.
	if p := strings.TrimSpace(req.Project); p != "" {
		s.engine.conv.SetProject(convID, p)
	}
	sess := s.engine.sessions.get(convID)
	var runID string
	duplicate := false
	if audio != nil {
		runID, _ = sess.submitAudio(msg, audio)
	} else {
		var submitErr error
		runID, _, duplicate, submitErr = sess.submitIdempotent(msg, req.IdempotencyKey, nil)
		if submitErr != nil {
			status := http.StatusInternalServerError
			if errors.Is(submitErr, runrecord.ErrIdempotencyConflict) {
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]string{"error": submitErr.Error()})
			return
		}
		if !duplicate && (!voiceTurn || s.engine.cfg.VoiceMode == "asr_first") {
			s.engine.conv.AppendUser(convID, runID, msg)
		}
	}
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
	// CHAT-01: POST /conversations/{id}/fork creates a new conversation from a
	// chronological prefix of an existing one. Intercepted before the proxy
	// fallback (a Neo-owned route the daemon never had).
	if id, ok := parseForkPath(r.URL.Path); ok {
		s.handleConversationFork(w, r, id)
		return
	}
	if !s.engine.conv.Enabled() {
		s.proxy.ServeHTTP(w, r)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/conversations"), "/")
	// CHAT-01: PATCH (rename/archive) and DELETE mutate a single conversation.
	switch r.Method {
	case http.MethodGet:
		// fall through to the read path below
	case http.MethodPatch:
		s.handleConversationPatch(w, r, id)
		return
	case http.MethodDelete:
		s.handleConversationDelete(w, r, id)
		return
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		items := s.engine.conv.List()
		// ?project= scopes the list to one workbench project (server-backed
		// per-project history; the History page's query).
		if project := strings.TrimSpace(r.URL.Query().Get("project")); project != "" {
			filtered := make([]conversation.Summary, 0, len(items))
			for _, it := range items {
				if it.Project == project {
					filtered = append(filtered, it)
				}
			}
			items = filtered
		}
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

// handleConversationPatch renames or archives one conversation (CHAT-01).
// Body: {"title":"…"} to rename (empty clears the override) and/or
// {"archived":true|false} to archive/unarchive. A missing/deleted conversation
// yields an explicit 404 rather than a silent create. Returns the refreshed
// summary so an optimistic client can reconcile.
func (s *Server) handleConversationPatch(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conversation id required"})
		return
	}
	var body struct {
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.Title == nil && body.Archived == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to update: provide title and/or archived"})
		return
	}
	if !s.engine.conv.Exists(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if !s.engine.conv.UpdateMeta(id, body.Title, body.Archived) {
		if !s.engine.conv.Exists(id) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "conversation update failed"})
		}
		return
	}
	writeJSON(w, http.StatusOK, s.conversationSummary(id))
}

// handleConversationDelete permanently removes one conversation (CHAT-01). It
// refuses while a run is still in flight (409) so an active task's state is not
// torn out from under it; the client must stop the run first. On success it
// removes the durable turn logs and every associated run's workspace trace, and
// returns a cleanup disclosure of what was purged.
func (s *Server) handleConversationDelete(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conversation id required"})
		return
	}
	if !s.engine.conv.Exists(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if live := s.engine.activeRunForConv(id); live != "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":     "conversation has an active run; stop it before deleting",
			"live_run":  live,
			"remediate": "POST the stop action for this run, then retry the delete",
		})
		return
	}
	// Collect the distinct run ids across the full history so each run's durable
	// workspace trace is purged alongside the turns.
	runIDs := map[string]struct{}{}
	for _, t := range s.engine.conv.History(id) {
		if t.IntentID != "" {
			runIDs[t.IntentID] = struct{}{}
		}
	}
	if !s.engine.conv.Delete(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	tracesRemoved := 0
	if s.engine.trace != nil {
		for rid := range runIDs {
			if s.engine.trace.Remove(rid) {
				tracesRemoved++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"conversation_id": id,
		"cleanup": map[string]interface{}{
			"turns":  "deleted",
			"traces": tracesRemoved,
			// Learned memories, generated media, and derived indexes are governed
			// by the memory-consent controls (PRIV-01) and ORACLE's deletion
			// pipeline; deleting a thread does not force-erase durable memories.
			"memories": "retained (manage under memory controls)",
			"media":    "retained (manage under memory controls)",
		},
	})
}

// handleConversationFork creates a new conversation from the chronological
// prefix of an existing one up to a selected turn (CHAT-01). Body:
// {"up_to_turn": N} — copy the first N turns of the source's full history into a
// fresh conversation with parent/turn provenance and an independent future. A
// missing/deleted parent yields an explicit 404; an out-of-range turn a 400.
func (s *Server) handleConversationFork(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.engine.conv.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "conversation persistence disabled"})
		return
	}
	if id == "" || strings.ContainsAny(id, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conversation id required"})
		return
	}
	var body struct {
		UpToTurn int `json:"up_to_turn"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	hist := s.engine.conv.History(id)
	if len(hist) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if live := s.engine.activeRunForConv(id); live != "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":    "conversation has an active run; wait for it to finish or stop it before forking",
			"live_run": live,
		})
		return
	}
	upTo := body.UpToTurn
	if upTo <= 0 || upTo > len(hist) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "up_to_turn out of range"})
		return
	}
	newID := synthConvID(id)
	if !s.engine.conv.Fork(id, newID, upTo) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fork failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"conversation_id": newID,
		"forked_from":     id,
		"forked_at_turn":  upTo,
		"summary":         s.conversationSummary(newID),
	})
}

// conversationSummary rebuilds the compact list-shape summary for one
// conversation (used by the PATCH/fork responses so an optimistic client
// reconciles against authoritative fields). Returns nil when it no longer
// exists.
func (s *Server) conversationSummary(id string) *conversation.Summary {
	for _, it := range s.engine.conv.List() {
		if it.ConversationID == id {
			sum := it
			return &sum
		}
	}
	return nil
}

// parseForkPath matches POST /conversations/{id}/fork (CHAT-01).
func parseForkPath(p string) (id string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 3 || parts[0] != "conversations" || parts[2] != "fork" {
		return "", false
	}
	return parts[1], true
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
	if id == "" {
		s.proxy.ServeHTTP(w, r)
		return
	}
	since := atoiSafe(r.URL.Query().Get("since_seq"))
	if !s.engine.broker.has(id) {
		if rec, ok, _ := s.engine.getRunRecord(id); ok {
			if rec.Terminal() {
				s.streamDurableTerminal(w, r, rec, since)
				return
			}
			s.engine.broker.ensure(id)
		} else {
			s.proxy.ServeHTTP(w, r)
			return
		}
	}
	s.streamSSE(w, r, id, since, true)
}

// handleReplay dumps a Neo run's buffered events and closes (finite), so the
// client can reconnect: replay history, then open the live stream with
// since_seq. Daemon intents fall through to the proxy.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/events/replay/")
	if id == "" {
		s.proxy.ServeHTTP(w, r)
		return
	}
	since := atoiSafe(r.URL.Query().Get("since_seq"))
	if !s.engine.broker.has(id) {
		if rec, ok, _ := s.engine.getRunRecord(id); ok {
			if rec.Terminal() {
				s.streamDurableTerminal(w, r, rec, since)
				return
			}
			s.engine.broker.ensure(id)
		} else {
			s.proxy.ServeHTTP(w, r)
			return
		}
	}
	s.streamSSE(w, r, id, since, false)
}

// handleAsyncPoll answers the poll_url that POST /chat advertises for a Neo run
// (GET /messages/async/<id>). The web client polls it on reload to decide
// whether to reconnect a still-live run; without this it would be proxied to
// the daemon, which has never heard of a neo_ intent and returns 404. Daemon
// async jobs (non-Neo intents) fall through to the proxy unchanged.
func (s *Server) handleAsyncPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/messages/async/")
	if id == "" {
		s.proxy.ServeHTTP(w, r)
		return
	}
	if rec, ok, err := s.engine.getRunRecord(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "run record unavailable"})
		return
	} else if ok {
		body := map[string]interface{}{
			"intent_id":      rec.IntentID,
			"status":         rec.Status,
			"request":        map[string]string{"prose": rec.Request},
			"created_at":     rec.CreatedAt.Format(time.RFC3339Nano),
			"last_event_seq": rec.LastEventSeq,
		}
		if rec.StartedAt != nil {
			body["started_at"] = rec.StartedAt.Format(time.RFC3339Nano)
		}
		if rec.EndedAt != nil {
			body["ended_at"] = rec.EndedAt.Format(time.RFC3339Nano)
		}
		if rec.Result != "" {
			body["result"] = map[string]interface{}{"intent_id": rec.IntentID, "status": rec.Status, "text": rec.Result}
		}
		if rec.Error != "" {
			body["error"] = rec.Error
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	if !s.engine.broker.has(id) {
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

func (s *Server) streamDurableTerminal(w http.ResponseWriter, r *http.Request, rec runrecord.Record, since int) {
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
	last := rec.LastEventSeq
	if last < 2 {
		last = 2
	}
	ts := rec.CreatedAt.Format(time.RFC3339Nano)
	if rec.EndedAt != nil {
		ts = rec.EndedAt.Format(time.RFC3339Nano)
	}
	events := []Event{
		{Seq: last - 1, Ts: ts, Phase: "neo", Type: "message.complete", Fields: map[string]interface{}{"status": rec.Status}},
		{Seq: last, Ts: ts, Phase: "neo", Type: "chat.assistant", Fields: map[string]interface{}{"role": "assistant", "text": rec.Result, "conversation_id": rec.ConversationID, "intent_id": rec.IntentID, "final": true}},
	}
	for _, ev := range events {
		if ev.Seq > since && !writeEvent(w, ev) {
			return
		}
	}
	flusher.Flush()
}

// handleHalt is the global kill switch (POST /halt): it interrupts EVERY live
// Neo run this daemon is driving and reports how many were signalled. Each
// daemon is single-tenant, so this stops exactly the calling user's own runaway
// / parallel Neos — the deliberate "Stop all" escape hatch when the model is
// slow or a storm of respawns is underway. Idempotent: halting with nothing
// live returns halted:0.
func (s *Server) handleHalt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	n := s.engine.haltAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "halted": n})
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
		s.engine.logLifecycle("run.stop", id, run.convID, "interrupting", time.Since(run.started), nil)
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
	s.engine.logLifecycle("run.attach", id, conversationForRun(s.engine, id), "attached", 0, nil)
	defer func() {
		cancel()
		s.engine.logLifecycle("run.detach", id, conversationForRun(s.engine, id), "detached", 0, nil)
	}()

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
