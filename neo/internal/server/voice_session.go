// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type voiceSessionRequest struct {
	ConversationID string `json:"conversation_id"`
	Event          string `json:"event,omitempty"`
	State          string `json:"state,omitempty"`
	Voice          string `json:"voice,omitempty"`
	Style          string `json:"style,omitempty"`
}

func (e *Engine) publishVoiceSession(convID, event, state string) {
	typ := "voice.session." + event
	if !traceWorkspaceTypes[typ] {
		return
	}
	e.publishPreview(convID, typ, map[string]interface{}{"state": state})
}

func (s *Server) handleVoiceSession(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/voice/session/")
	if action == "state" && r.Method == http.MethodGet {
		s.forwardVoiceController(w, r, http.MethodGet, "/state", nil)
		return
	}
	if r.Method != http.MethodPost || (action != "start" && action != "stop" && action != "event") {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req voiceSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || !validVoiceConversationID(req.ConversationID) || !validVoiceSettings(req.Voice, req.Style) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "voice session is unavailable"})
		return
	}
	if action == "event" {
		if req.Event != "start" && req.Event != "state" && req.Event != "stop" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "voice session is unavailable"})
			return
		}
		s.engine.publishVoiceSession(req.ConversationID, req.Event, req.State)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
		return
	}
	body, _ := json.Marshal(req)
	status := s.forwardVoiceController(w, r, http.MethodPost, "/"+action, body)
	if status >= 200 && status < 300 {
		state := "connected"
		if action == "stop" {
			state = "stopped"
		}
		s.engine.publishVoiceSession(req.ConversationID, action, state)
	}
}

func validVoiceSettings(voice, style string) bool {
	if len(style) > 500 {
		return false
	}
	switch strings.TrimSpace(voice) {
	case "", "mimo_default", "冰糖", "茉莉", "苏打", "白桦", "Mia", "Chloe", "Milo", "Dean":
		return true
	default:
		return false
	}
}

func (s *Server) forwardVoiceController(w http.ResponseWriter, r *http.Request, method, path string, body []byte) int {
	if !s.engine.cfg.VoiceEnabled || s.engine.voiceControllerURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "voice is unavailable"})
		return http.StatusServiceUnavailable
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.engine.voiceControllerURL+path, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "voice is unavailable"})
		return http.StatusServiceUnavailable
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "voice is unavailable"})
		return http.StatusServiceUnavailable
	}
	defer resp.Body.Close()
	var payload interface{}
	if json.NewDecoder(resp.Body).Decode(&payload) != nil {
		payload = map[string]interface{}{"error": "voice is unavailable"}
	}
	writeJSON(w, resp.StatusCode, payload)
	return resp.StatusCode
}

func validVoiceConversationID(value string) bool {
	if len(value) < 2 || len(value) > 160 {
		return false
	}
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
}
