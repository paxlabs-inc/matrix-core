// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

// The personalization-interview surface (ORACLE task 5.3, req 12).
//
//   POST /interview/start   mint a fresh interview-flagged conversation and
//                            return its id — the client opens it in the normal
//                            chat surface and Neo runs the guided interview
//                            there (five skippable groups, one at a time,
//                            summary + explicit confirmation before saving).
//
// Starting again later is the SAME call (req 12.1 "repeat/edit"): the new
// interview conversation re-enters with the saved answers, which the session
// injects from the live profile record when it mints the interview agent.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"matrix/neo/internal/memory"
)

// handleInterview is the prefix dispatcher for /interview/*.
func (s *Server) handleInterview(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/interview/start":
		s.handleInterviewStart(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown interview route"})
	}
}

// handleInterviewStart mints an interview-flagged conversation. The flag is
// durable conversation meta, so the interview posture (charter + save tool +
// writeback exclusion) survives respawns and restarts.
func (s *Server) handleInterviewStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine == nil || !s.engine.conv.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "interview not available (no durable conversation store)"})
		return
	}
	convID := synthConvID("interview")
	s.engine.conv.SetInterview(convID)
	writeJSON(w, http.StatusOK, map[string]string{"conversation_id": convID})
}

// savePersonalizationProfile is the engine-side writer behind the
// confirmation-gated save_personalization_profile tool (task 5.3): it decodes
// the confirmed profile JSON and persists it as the single versioned cortex
// record through the real pager (req 13.1).
func (e *Engine) savePersonalizationProfile(ctx context.Context, profileJSON []byte) (string, error) {
	if e.pager == nil {
		return "", fmt.Errorf("neo/personalization: no memory store")
	}
	var prof memory.PersonalizationProfile
	if err := json.Unmarshal(profileJSON, &prof); err != nil {
		return "", fmt.Errorf("neo/personalization: decode profile: %w", err)
	}
	uri, err := e.pager.SavePersonalizationProfile(ctx, prof)
	if err != nil {
		return "", err
	}
	// The guided interview reaches this writer only after its explicit
	// confirmation gate, which constitutes an affirmative opt-in to durable
	// personalization (PRIV-01 req 8.1). Enabling consent here keeps the
	// downstream extraction/writeback gate consistent with the user's
	// confirmed intent rather than leaving it default-off.
	if _, cerr := e.pager.SetMemoryConsent(ctx, true, "personalization-interview"); cerr != nil {
		return "", fmt.Errorf("neo/personalization: record consent: %w", cerr)
	}
	return uri, nil
}
