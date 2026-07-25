// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"matrix/neo/internal/dojo"
	"matrix/neo/internal/llm"
)

// DOJO (req 1): the disposable desktop's session lifecycle manager lives in
// neo/internal/dojo; this file is the engine seam — the durable dojo.* event
// emitter and the reaper hook. The actuation lane (tools/desktop, wave 2) and
// the client surface (wave 3) build on these events.

// publishDojo emits a dojo.* lifecycle event onto the conversation's run topic
// so it reaches live subscribers AND (being whitelisted in
// traceWorkspaceTypes) the durable trace, so reopen rebuilds the last honest
// desktop state.
func (e *Engine) publishDojo(convID, typ string, fields map[string]interface{}) {
	runID := e.activeRunForConv(convID)
	if runID == "" {
		runID = e.latestRunForConv(convID)
	}
	if runID == "" {
		runID = convID
	}
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["intent_id"] = runID
	fields["conversation_id"] = convID
	e.broker.ensure(runID)
	e.broker.publish(runID, typ, "neo", fields)
}

// Dojo exposes the session manager (nil when not configured); the wave-2
// desktop lane and wave-3 routes resolve sessions through it.
func (e *Engine) Dojo() *dojo.Manager {
	return e.dojo
}

// dojoBootWait bounds how long an agent desktop call waits for a booting
// desktop before returning the typed desktop_booting result (well under the
// bridge's 60s call timeout; a warm boot resolves inside it, a cold ~2 GiB
// image pull does not and the model is told to retry).
const dojoBootWait = 20 * time.Second

// desktopBootingMessage is the model-facing typed result for a desktop that is
// still turning on. It names the retry contract explicitly.
const desktopBootingMessage = "the desktop is booting — the computer is turning on, which can take a few minutes on a cold start. The user sees the boot screen. Retry the desktop action shortly (or do other useful work first); do not give up on the task because of this."

// activeConvID reports the conversation of a currently executing run — the
// binding target for a desktop session the agent implicitly boots mid-run.
// Each daemon is single-tenant, so any live run is the user's own; with more
// than one live the most recently registered wins.
func (e *Engine) activeConvID() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	conv := ""
	for _, r := range e.runs {
		if r != nil && r.convID != "" {
			conv = r.convID
		}
	}
	return conv
}

// dojoBoot is the wave-3 provisioning trigger: it ensures the active
// conversation has a desktop session (starting a BACKGROUND boot when none
// exists — the client renders it as the computer turning on) and waits briefly
// for readiness. nil means the desktop is ready to drive; dojo.ErrBooting means
// it is still provisioning; any other error is a typed lifecycle failure.
func (e *Engine) dojoBoot(ctx context.Context) error {
	conv := e.activeConvID()
	if conv == "" {
		return errors.New("no desktop is running and there is no active conversation to boot one for")
	}
	if _, err := e.dojo.EnsureSession(conv); err != nil {
		return err
	}
	snap, err := e.dojo.WaitReady(ctx, conv, dojoBootWait)
	if errors.Is(err, dojo.ErrNotFound) {
		return errors.New("the desktop failed to boot — the boot screen shows why; retrying will turn on a fresh desktop")
	}
	if snap.State == dojo.StateProvisioning {
		return dojo.ErrBooting
	}
	return nil
}

// StartDojoReaper runs the idle-timeout / hard-lifetime reaper until ctx ends
// (no-op when dojo is not configured).
func (e *Engine) StartDojoReaper(ctx context.Context) {
	if e.dojo != nil {
		go e.dojo.StartReaper(ctx)
	}
}

// desktopLook is the tools.DesktopLookFunc seam (DOJO req 3.2): it captures the
// live desktop through bytebotd, invokes the stateless MiMo v2.5 omni vision
// client with the frozen single-pass grounding prompt, and returns structured
// text — interactive elements + a best click point, coordinates already
// rescaled from omni's 0–1000 convention to screen pixels. Omni output enters
// the window ONLY as this tool result (planner_integrity); the session model is
// never swapped.
func (e *Engine) desktopLook(ctx context.Context, target string) (string, error) {
	if e.dojo == nil {
		return "", errors.New("no disposable desktop is configured")
	}
	if e.omni == nil {
		return "", errors.New("desktop vision grounding is not configured")
	}
	shot, err := e.dojo.CallBytebotd(ctx, []byte(`{"action":"screenshot"}`))
	if errors.Is(err, dojo.ErrNoSession) || errors.Is(err, dojo.ErrBooting) {
		if berr := e.dojoBoot(ctx); berr != nil {
			if errors.Is(berr, dojo.ErrBooting) {
				return "", errors.New(desktopBootingMessage)
			}
			return "", berr
		}
		shot, err = e.dojo.CallBytebotd(ctx, []byte(`{"action":"screenshot"}`))
	}
	if err != nil {
		return "", err
	}
	var wrap struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(shot, &wrap); err != nil || wrap.Image == "" {
		return "", fmt.Errorf("desktop screenshot was not an image: %w", err)
	}
	png, err := base64.StdEncoding.DecodeString(wrap.Image)
	if err != nil {
		return "", fmt.Errorf("desktop screenshot base64: %w", err)
	}
	width, height, err := dojo.PNGDimensions(png)
	if err != nil {
		return "", err
	}
	dataURL := "data:image/png;base64," + wrap.Image
	req := llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(dojo.GroundingSystemPrompt),
		llm.UserImageMessage(dojo.GroundingInstruction(target), dataURL),
	}}
	// One retry on an empty completion: omni can spend its whole budget on
	// reasoning and return no content (wave-1 gotcha). The client is built with
	// a generous max_tokens so the second pass has room.
	var raw string
	for attempt := 0; attempt < 2 && raw == ""; attempt++ {
		res, cerr := e.omni.Chat(ctx, req)
		if cerr != nil {
			return "", fmt.Errorf("omni grounding call: %w", cerr)
		}
		raw = strings.TrimSpace(res.Message.Content)
	}
	if raw == "" {
		return "", errors.New("omni returned no grounding after retry")
	}
	g, err := dojo.ParseGrounding(raw, width, height)
	if err != nil {
		return "", err
	}
	return dojo.FormatGrounding(g), nil
}

// desktopA11y is the tools.DesktopA11yFunc seam (DOJO req 3.3): the
// deterministic middle rung — an AT-SPI accessibility-tree dump of the live
// desktop, rendered as text with screen-pixel coordinates and no vision model.
func (e *Engine) desktopA11y(ctx context.Context) (string, error) {
	if e.dojo == nil {
		return "", errors.New("no disposable desktop is configured")
	}
	tree, err := e.dojo.A11yDump(ctx)
	if errors.Is(err, dojo.ErrNoSession) || errors.Is(err, dojo.ErrBooting) {
		if berr := e.dojoBoot(ctx); berr != nil {
			if errors.Is(berr, dojo.ErrBooting) {
				return "", errors.New(desktopBootingMessage)
			}
			return "", berr
		}
		tree, err = e.dojo.A11yDump(ctx)
	}
	if err != nil {
		return "", err
	}
	return dojo.FormatA11yTree(tree), nil
}

// handleDojoSession serves GET /dojo/session?conversation={id}: the live dojo
// session snapshot for a conversation (or null), so a freshly opened client can
// seed its desktop panel without replaying the whole trace.
func (s *Server) handleDojoSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e := s.engine
	if e.dojo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "the disposable desktop is not configured"})
		return
	}
	conv := strings.TrimSpace(r.URL.Query().Get("conversation"))
	if conv == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation is required"})
		return
	}
	snap, ok := e.dojo.SessionForConv(conv)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"session": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"session": snap})
}

// handleDojoFrame serves GET /dojo/frame?conversation={id}: one live-view
// frame of the conversation's desktop, JPEG-encoded for the wire. This is the
// wave-3 live-view transport: the client polls it while the panel is visible —
// plain request/response over the existing exec-curl carrier, no held stream
// to keepalive (the 2026-07-11 browser-heartbeat failure class cannot occur).
// Frames are passive: they never bump the session's idle clock.
func (s *Server) handleDojoFrame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e := s.engine
	if e.dojo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "the disposable desktop is not configured"})
		return
	}
	conv := strings.TrimSpace(r.URL.Query().Get("conversation"))
	if conv == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation is required"})
		return
	}
	png, err := e.dojo.Frame(r.Context(), conv)
	switch {
	case errors.Is(err, dojo.ErrBooting):
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "desktop_booting"})
		return
	case errors.Is(err, dojo.ErrNoSession), errors.Is(err, dojo.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "no desktop is running"})
		return
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if jpg, jerr := reencodeJPEG(png); jerr == nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jpg)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// handleDojoBoot serves POST /dojo/boot {conversation_id}: the USER turns the
// disposable desktop on — the power button, fully independent of the agent
// (req 4.2: the human's control of the shared computer includes its power).
// Reattaches the live session when one exists; otherwise registers a fresh one
// and boots it in the background while the panel renders the turning-on state.
func (s *Server) handleDojoBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e := s.engine
	if e.dojo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "the disposable desktop is not configured"})
		return
	}
	var req struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.ConversationID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation_id is required"})
		return
	}
	snap, err := e.dojo.EnsureSession(strings.TrimSpace(req.ConversationID))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"session": snap})
}

// handleDojoShutdown serves POST /dojo/shutdown {conversation_id}: the USER
// turns the desktop off. This is the normal teardown pipeline — work ships
// home first (req 5.1); it is NOT a discard. Idempotent: shutting down an
// already-off desktop answers 200 with a null session.
func (s *Server) handleDojoShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e := s.engine
	if e.dojo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "the disposable desktop is not configured"})
		return
	}
	var req struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.ConversationID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation_id is required"})
		return
	}
	snap, ok := e.dojo.SessionForConv(strings.TrimSpace(req.ConversationID))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"session": nil})
		return
	}
	if err := e.dojo.Teardown(r.Context(), snap.ID, "user_shutdown"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"session": nil})
}

// handleDojoTakeover serves POST /dojo/takeover {conversation_id}: the human
// takes the control lease (req 4.2). While held, agent desktop calls are
// rejected with takeover_active; the human's input passes through /dojo/input.
func (s *Server) handleDojoTakeover(w http.ResponseWriter, r *http.Request) {
	s.handleDojoLease(w, r, func(convID string) (dojo.Session, error) {
		return s.engine.dojo.TakeoverConv(convID)
	})
}

// handleDojoHandback serves POST /dojo/handback {conversation_id}: the human
// returns the controls (req 4.3). The manager arms the re-observation gate so
// the agent looks before it acts.
func (s *Server) handleDojoHandback(w http.ResponseWriter, r *http.Request) {
	s.handleDojoLease(w, r, func(convID string) (dojo.Session, error) {
		return s.engine.dojo.HandbackConv(convID)
	})
}

func (s *Server) handleDojoLease(w http.ResponseWriter, r *http.Request, move func(string) (dojo.Session, error)) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine.dojo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "the disposable desktop is not configured"})
		return
	}
	var req struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.ConversationID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation_id is required"})
		return
	}
	snap, err := move(strings.TrimSpace(req.ConversationID))
	switch {
	case errors.Is(err, dojo.ErrNoSession), errors.Is(err, dojo.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "no desktop is running"})
		return
	case errors.Is(err, dojo.ErrBooting):
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "desktop_booting"})
		return
	case errors.Is(err, dojo.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"session": snap})
}

// handleDojoInput serves POST /dojo/input {conversation_id, request}: one
// human input action passed through to the desktop WHILE the control lease is
// held — refused in every other state, so the passthrough exists only under an
// explicit takeover.
func (s *Server) handleDojoInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e := s.engine
	if e.dojo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "the disposable desktop is not configured"})
		return
	}
	var req struct {
		ConversationID string          `json:"conversation_id"`
		Request        json.RawMessage `json:"request"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil || strings.TrimSpace(req.ConversationID) == "" || len(req.Request) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation_id and request are required"})
		return
	}
	resp, err := e.dojo.TakeoverInput(r.Context(), strings.TrimSpace(req.ConversationID), req.Request)
	switch {
	case errors.Is(err, dojo.ErrNoSession):
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "no desktop is running"})
		return
	case errors.Is(err, dojo.ErrNoTakeover):
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "not_in_takeover", "message": "take control before sending input"})
		return
	case errors.Is(err, dojo.ErrBytebotd):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(resp)
		return
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// reencodeJPEG transcodes a PNG frame to JPEG for the client wire — a desktop
// frame shrinks roughly 10x, and the poll cadence makes that the dominant
// bandwidth cost. The sandbox-side read cost is unchanged either way.
func reencodeJPEG(png []byte) ([]byte, error) {
	img, err := pngDecode(png)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 72}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func pngDecode(b []byte) (image.Image, error) {
	return png.Decode(bytes.NewReader(b))
}

// handleDojoComputerUse is the loopback proxy the desktop bridge posts to
// (MATRIX_DOJO_URL/computer-use). It resolves the user's live disposable-desktop
// session and rides the exec-curl carrier into the appliance's localhost:9990,
// relaying the bytebotd response verbatim. It is gated on the shared bridge
// token so only the co-located bridge — never external router-forwarded
// traffic — can drive the desktop (DOJO guard_surface).
func (s *Server) handleDojoComputerUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	e := s.engine
	if e.dojoBridgeToken == "" || e.dojo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "the disposable desktop is not configured"})
		return
	}
	if !bearerEquals(r, e.dojoBridgeToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	resp, err := e.dojo.CallBytebotd(r.Context(), body)
	if errors.Is(err, dojo.ErrNoSession) || errors.Is(err, dojo.ErrBooting) {
		// Wave-3 boot trigger: the agent's first desktop action turns the
		// computer on; a warm boot resolves within this call, a cold one
		// returns the typed booting result the planner retries.
		if berr := e.dojoBoot(r.Context()); berr != nil {
			if errors.Is(berr, dojo.ErrBooting) {
				writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "desktop_booting", "message": desktopBootingMessage})
			} else {
				writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "desktop_unavailable", "message": berr.Error()})
			}
			return
		}
		resp, err = e.dojo.CallBytebotd(r.Context(), body)
	}
	switch {
	case errors.Is(err, dojo.ErrBooting):
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "desktop_booting", "message": desktopBootingMessage})
		return
	case errors.Is(err, dojo.ErrTakeoverActive):
		// The control lease is held (req 4.2): a typed result the planner
		// understands as "the human is driving — wait".
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "takeover_active", "message": "the user has taken control of the desktop and is driving it — wait for them to hand control back before acting; do not retry in a loop"})
		return
	case errors.Is(err, dojo.ErrReobserveRequired):
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "reobserve_required", "message": "the user drove the desktop and may have changed anything — take a fresh look first (desktop_a11y, desktop_look, or desktop_screenshot), then act on what you actually see"})
		return
	case errors.Is(err, dojo.ErrNoSession):
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "no desktop is running"})
		return
	case errors.Is(err, dojo.ErrBytebotd):
		// Relay bytebotd's own error body + a 4xx so the model reads and adapts.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(resp)
		return
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// bearerEquals reports whether the request's Authorization: Bearer token equals
// want.
func bearerEquals(r *http.Request, want string) bool {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.HasPrefix(h, "Bearer ") && strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) == want
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
