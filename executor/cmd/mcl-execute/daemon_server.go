// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_server.go — HTTP+SSE routes for the daemon.
//
// Routes:
//
//   GET  /healthz       liveness + actor + overall_root + uptime + broker stats
//   GET  /events        SSE stream of every transcript event (live web feed)
//   POST /messages      run one message end-to-end through runMessage
//   GET  /intents/{id}  list signed envelope chain for an intent
//   POST /shutdown      graceful drain (admin token)
//
// Auth: Bearer token via Authorization header, matched against
// MATRIX_DAEMON_TOKEN. Empty token disables auth (local-dev only).
//
// Single-flight: at most one /messages in flight at a time so cortex
// stays single-writer; concurrent requests get 409 Busy.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// newDaemonMux assembles the HTTP routes for the daemon.
//
// sess#27 surface (in route order):
//
//	Public:
//	  GET  /healthz
//	  GET  /version
//
//	User-facing (authAny — router headers OR legacy bearer):
//	  GET  /me
//	  POST /messages                 sync (legacy)
//	  POST /messages/async           returns 202 + intent_id
//	  GET  /messages/async/<id>      poll terminal state
//	  GET  /events                   live SSE firehose w/ filters
//	  GET  /events/replay/<id>       backfill historical events
//	  GET  /intents                  list + filters
//	  GET  /intents/<id>...          summary, lifecycle, plan, ...
//	  POST /intents/<id>/cancel
//	  POST /intents/<id>/correct
//	  POST /intents/<id>/gates/<nid>/answer
//	  GET  /memory                   cortex.Find
//	  POST /memory/search            cortex.Find (body)
//	  GET  /memory/<uri>             cortex.Resolve
//	  GET  /memory/types             distinct type counts
//	  GET  /memory/recent            newest-first
//	  GET  /memory/salience/top      top-N
//	  GET  /skills                   list + filters
//	  GET  /skills/<slug@v>          detail
//	  POST /skills/suggest           ranking by prose
//	  GET  /tools                    list
//	  GET  /tools/<alias>/<name>     detail
//	  GET  /servers                  MCP status
//	  GET  /agents/manifest          agent manifest
//	  POST /classify                 D9 materiality
//	  GET  /cortex/snapshot          overall_root
//	  GET  /cortex/stats             totals + per-type counts
//	  GET  /cortex/replay            replay validation
//	  GET  /snapshots                snapshot manager status
//	  POST /snapshots/push           manual push
//	  GET  /diag/embedder|/mcp|/bridge
//	  GET  /settings
//
//	Admin (legacy bearer required):
//	  POST /shutdown
func newDaemonMux(d *daemonState, t *transcript) http.Handler {
	mux := http.NewServeMux()

	// Public + ops.
	mux.HandleFunc("/healthz", d.handleHealthz)
	mux.HandleFunc("/version", d.handleVersion)
	mux.HandleFunc("/metrics", d.handleMetrics(t))

	// Identity / settings / diag.
	mux.HandleFunc("/me", d.handleMe)
	mux.HandleFunc("/settings", d.handleSettings)
	mux.HandleFunc("/diag/embedder", d.handleDiagEmbedder)
	mux.HandleFunc("/diag/mcp", d.handleDiagMCP)
	mux.HandleFunc("/diag/bridge", d.handleDiagBridge)

	// Chat: the Liaison front door (triage → reply or dispatch+narrate).
	mux.HandleFunc("/chat", d.handleChat(t))

	// Messages: sync (legacy) + async.
	mux.HandleFunc("/messages", d.handleMessages(t))
	mux.HandleFunc("/messages/async", d.handleMessagesAsyncStart(t))
	mux.HandleFunc("/messages/async/", d.handleMessagesAsyncPoll)

	// Events: live + replay.
	mux.HandleFunc("/events", d.handleEventsFiltered)
	mux.HandleFunc("/events/replay/", d.handleEventsReplay)

	// Intents: list at /intents, sub-routes via the router.
	mux.HandleFunc("/intents", d.handleIntentsRouter(t))
	mux.HandleFunc("/intents/", d.handleIntentsRouter(t))

	// Memory: cortex.Find / Resolve / metadata / Write.
	mux.HandleFunc("/memory", d.handleMemoryRoot)
	mux.HandleFunc("/memory/", d.handleMemoryRouter)
	mux.HandleFunc("/memory/search", d.handleMemorySearch)
	mux.HandleFunc("/memory/types", d.handleMemoryTypes)
	mux.HandleFunc("/memory/recent", d.handleMemoryRecent)
	mux.HandleFunc("/memory/salience/top", d.handleMemorySalienceTop)

	// Skills: corpus catalog.
	mux.HandleFunc("/skills", d.handleSkillsRouter)
	mux.HandleFunc("/skills/", d.handleSkillsRouter)

	// Tools / servers / agents.
	mux.HandleFunc("/tools", d.handleToolsRouter)
	mux.HandleFunc("/tools/", d.handleToolsRouter)
	mux.HandleFunc("/servers", d.handleServersList)
	mux.HandleFunc("/agents/manifest", d.handleAgentsManifest)

	// Classify (D9 materiality).
	mux.HandleFunc("/classify", d.handleClassify)

	// Cortex introspection.
	mux.HandleFunc("/cortex/snapshot", d.handleCortexSnapshot)
	mux.HandleFunc("/cortex/stats", d.handleCortexStats)
	mux.HandleFunc("/cortex/replay", d.handleCortexReplay)

	// Snapshots.
	mux.HandleFunc("/snapshots", d.handleSnapshotsList)
	mux.HandleFunc("/snapshots/push", d.handleSnapshotsPush)

	// sess#34 / Forge Phase 1: filesystem HTTP surface for the Forge
	// SPA. Routes 404 when d.forgeFS is nil (i.e. daemon not booted
	// with -forge-mode).
	mux.HandleFunc("/fs/tree", d.handleForgeFSRouter)
	mux.HandleFunc("/fs/read", d.handleForgeFSRouter)
	mux.HandleFunc("/fs/write", d.handleForgeFSRouter)

	// sess#36 / Forge Phase 3: git HTTP surface for the diff viewer +
	// branch/merge ops. Routes 404 when d.gitOps is nil.
	mux.HandleFunc("/git/status", d.handleForgeGitRouter)
	mux.HandleFunc("/git/diff", d.handleForgeGitRouter)
	mux.HandleFunc("/git/branch", d.handleForgeGitRouter)
	mux.HandleFunc("/git/merge", d.handleForgeGitRouter)

	// sess#36 / Forge Phase 3: PTY shell over WebSocket for xterm.js.
	// Routes 404 when d.shellCfg is nil.
	mux.HandleFunc("/shell/exec", d.handleForgeShellExec)

	// Gideon fleet telemetry: read-only proxy of the admin-dashboard live
	// fleet summary (GIDEON_FLEET_URL + /api/summary). Returns 404 when the
	// daemon is not in -gideon-mode, mirroring the forge /fs + /git surfaces.
	mux.HandleFunc("/gideon/fleet", d.handleGideonFleet)

	// Admin.
	mux.HandleFunc("/shutdown", d.handleShutdown(t))

	return logMiddleware(t, mux)
}

// handleMemoryRoot dispatches the bare /memory path by method:
//
//	GET  /memory   →  handleMemoryFind (existing list+filter surface)
//	POST /memory   →  handleMemoryWrite (sess#29; writes typed memory)
//
// Method-not-allowed for everything else.
func (d *daemonState) handleMemoryRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.handleMemoryFind(w, r)
	case http.MethodPost:
		d.handleMemoryWrite(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleMemoryRouter dispatches /memory/<URI-encoded> | /memory/search
// | /memory/types | /memory/recent | /memory/salience/top to the
// correct handler. ServeMux picks the longest matching prefix, but
// some sub-paths (search, types, recent, salience/top) are explicit
// registered above; this catch-all handles arbitrary URI-encoded
// paths under /memory/.
func (d *daemonState) handleMemoryRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/memory/")
	switch path {
	case "search":
		d.handleMemorySearch(w, r)
	case "types":
		d.handleMemoryTypes(w, r)
	case "recent":
		d.handleMemoryRecent(w, r)
	case "salience/top":
		d.handleMemorySalienceTop(w, r)
	default:
		d.handleMemoryResolve(w, r)
	}
}

// --- middleware -----------------------------------------------------

// logMiddleware emits one transcript event per HTTP request with status
// + duration so the audit feed includes wire-level activity.
func logMiddleware(t *transcript, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		t.Event("http.request", "http", map[string]interface{}{
			"method":   r.Method,
			"path":     r.URL.Path,
			"status":   rec.status,
			"remote":   r.RemoteAddr,
			"duration": time.Since(start).Milliseconds(),
		})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(c int) {
	if !s.wrote {
		s.status = c
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(c)
}

// Flush forwards to the underlying ResponseWriter when it implements
// http.Flusher. Required so SSE on /events streams live; without this
// the type assertion `w.(http.Flusher)` in handleEvents fails since
// statusRecorder satisfies http.ResponseWriter via embedding but the
// embedded interface method set does NOT include Flush.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter so Go 1.20+
// http.NewResponseController and ReverseProxy can reach hijack /
// flush / push capabilities through the wrapper.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Hijack lets WebSocket upgraders (gorilla/websocket on /shell/exec)
// take over the underlying connection. Without this method the
// upgrader's `w.(http.Hijacker)` type assertion fails on the
// statusRecorder wrapper and returns 500 before our handler runs.
//
// Sess#36 / Forge Phase 3.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("statusRecorder: underlying ResponseWriter is not Hijacker")
	}
	// Mark the request as hijacked at status 101 so logMiddleware
	// emits a useful row for the WS handshake instead of "0".
	if !s.wrote {
		s.status = http.StatusSwitchingProtocols
		s.wrote = true
	}
	return hj.Hijack()
}

// requireAuth enforces the bearer token when configured. Returns true
// iff the request is authorised (or auth is disabled). On 401 the
// response is fully written and the caller must return.
func (d *daemonState) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if d.authToken == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	got = strings.TrimSpace(got)
	if got == "" || got != d.authToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "unauthorised: bearer token required",
		})
		return false
	}
	return true
}

// --- /healthz -------------------------------------------------------

type healthzResponse struct {
	OK            bool   `json:"ok"`
	Actor         string `json:"actor"`
	Agent         string `json:"agent"`
	OverallRoot   string `json:"overall_root,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Subscribers   int    `json:"sse_subscribers"`
	Published     uint64 `json:"sse_published"`
	Dropped       uint64 `json:"sse_dropped"`
	Busy          bool   `json:"in_flight"`
	GideonMode    bool   `json:"gideon_mode"`
}

func (d *daemonState) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	subs, pub, drop := d.broker.Stats()
	resp := healthzResponse{
		OK:            true,
		Actor:         d.actor.UserURI,
		Agent:         d.actor.AgentURI,
		OverallRoot:   captureRoot(d.infra.cortex),
		UptimeSeconds: int64(time.Since(d.startedAt).Seconds()),
		Subscribers:   subs,
		Published:     pub,
		Dropped:       drop,
		Busy:          d.tryProbeBusy(),
		GideonMode:    d.gideonMode,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleEvents is preserved for callers that import it directly
// (tests / harnesses); the production /events route uses
// handleEventsFiltered which adds query-param filtering. They share
// the same auth path so behaviour is identical when no filter is set.
func (d *daemonState) handleEvents(w http.ResponseWriter, r *http.Request) {
	d.handleEventsFiltered(w, r)
}

// tryProbeBusy returns true iff /messages is currently held. It does
// NOT attempt to acquire the lock (that's the request's job); a brief
// best-effort read using TryLock+Unlock is fine for /healthz.
func (d *daemonState) tryProbeBusy() bool {
	if d.busy.TryLock() {
		d.busy.Unlock()
		return false
	}
	return true
}

// --- /events (SSE) --------------------------------------------------
//
// Live + replay handlers live in daemon_events.go; the
// handleEventsFiltered shim above delegates to them. The legacy
// unfiltered handler that lived here in sess#26 was replaced as
// part of the sess#27 route surface expansion.

// --- /messages ------------------------------------------------------

func (d *daemonState) handleMessages(t *transcript) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !d.requireAuth(w, r) {
			return
		}

		// Single-flight: cortex is single-writer per actor.
		if !d.busy.TryLock() {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "another intent is in flight; retry after current message completes",
			})
			return
		}
		defer d.busy.Unlock()

		var req messageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("decode body: %v", err),
			})
			return
		}
		if req.Prose == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "prose is required",
			})
			return
		}

		// Tie the request lifetime to the underlying request context
		// so client disconnects cancel the in-flight pipeline. In
		// Gideon mode dispatchMessage routes to the compiler-bypass
		// runMessageDirect; otherwise the legacy runMessage runs.
		res, err := d.dispatchMessage(r.Context(), req)
		if err != nil {
			// Structured clarify-required = 422.
			if cre, ok := err.(*clarifyRequiredError); ok {
				writeJSON(w, http.StatusUnprocessableEntity, cre)
				return
			}
			t.Event("daemon.message.error", "http", map[string]interface{}{
				"error": err.Error(),
			})
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// --- /intents/{id} --------------------------------------------------

type intentEnvelopeFile struct {
	Seq      int             `json:"seq"`
	Filename string          `json:"filename"`
	Kind     string          `json:"kind,omitempty"`
	Envelope json.RawMessage `json:"envelope,omitempty"`
}

type intentResponse struct {
	IntentID  string               `json:"intent_id"`
	Path      string               `json:"path"`
	Envelopes []intentEnvelopeFile `json:"envelopes"`
}

func (d *daemonState) handleIntents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !d.requireAuth(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/intents/")
	id = strings.Trim(id, "/")
	if id == "" || strings.ContainsAny(id, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "intent id required",
		})
		return
	}
	dir := filepath.Join(d.journalDir, id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "intent not found",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("read intent dir: %v", err),
		})
		return
	}
	files := make([]intentEnvelopeFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		seq, kind := parseEnvelopeFilename(e.Name())
		fpath := filepath.Join(dir, e.Name())
		body, rerr := os.ReadFile(fpath)
		if rerr != nil {
			continue
		}
		files = append(files, intentEnvelopeFile{
			Seq:      seq,
			Filename: e.Name(),
			Kind:     kind,
			Envelope: json.RawMessage(body),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Seq < files[j].Seq })
	writeJSON(w, http.StatusOK, intentResponse{
		IntentID:  id,
		Path:      dir,
		Envelopes: files,
	})
}

// parseEnvelopeFilename extracts (seq, kind) from "0001-intent_compiled.json".
// Returns (0, name-without-ext) when the format is unexpected.
func parseEnvelopeFilename(name string) (int, string) {
	base := strings.TrimSuffix(name, ".json")
	parts := strings.SplitN(base, "-", 2)
	if len(parts) != 2 {
		return 0, base
	}
	seq := 0
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return 0, base
		}
		seq = seq*10 + int(c-'0')
	}
	return seq, parts[1]
}

// --- /shutdown ------------------------------------------------------

// shutdownRequested signals the main loop (via the listening server's
// Shutdown call below). We trigger it by calling srv.Shutdown — but
// the server reference lives in runDaemon. We use an atomic flag plus
// a context cancel passed via daemonState... but to keep this simple
// the server just closes its own listener via http.Server.Close from
// the handler in a goroutine.
//
// Implementation note: we expose a hook function shutdownFn that
// runDaemon installs via attachShutdown before serving.
var shutdownTriggered atomic.Bool
var shutdownFn func()

func attachShutdown(fn func()) { shutdownFn = fn }

func (d *daemonState) handleShutdown(t *transcript) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if !d.requireAuth(w, r) {
			return
		}
		if !shutdownTriggered.CompareAndSwap(false, true) {
			writeJSON(w, http.StatusAccepted, map[string]string{
				"status": "shutdown already in progress",
			})
			return
		}
		t.Event("daemon.shutdown.requested", "shutdown", map[string]interface{}{
			"remote": r.RemoteAddr,
		})
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "shutdown initiated",
		})
		if shutdownFn != nil {
			go shutdownFn()
		}
	}
}

// --- helpers --------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// ensureCtx is a small helper so SSE handlers can derive context with
// a deadline if needed. Currently unused but reserved for stream caps.
func ensureCtx(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
