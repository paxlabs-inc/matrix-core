// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	profileStore "centra/agents/neo/internal/runtime/profile"
)

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if s.engine.profileStore == nil {
		message := "encrypted profile store is unavailable"
		if s.engine.profileStoreErr != nil {
			message = s.engine.profileStoreErr.Error()
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message})
		return
	}
	switch r.Method {
	case http.MethodGet:
		profile, err := s.engine.profileStore.Get(r.Context(), s.engine.readLegacyProfile)
		if errors.Is(err, profileStore.ErrAbsent) {
			writeJSON(w, http.StatusOK, profileStore.Profile{AgentName: "Neo", ExpertiseDomains: []string{}})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.engine.cacheProfile(profile)
		writeJSON(w, http.StatusOK, profile)
	case http.MethodPut:
		var input profileStore.Profile
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
			return
		}
		input.PreferredPersonName = strings.TrimSpace(input.PreferredPersonName)
		input.AgentName = strings.TrimSpace(input.AgentName)
		stored, err := s.engine.profileStore.Put(r.Context(), input)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.engine.cacheProfile(stored)
		writeJSON(w, http.StatusOK, stored)
	case http.MethodDelete:
		if err := s.engine.profileStore.Delete(r.Context()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.engine.profileMu.Lock()
		s.engine.userAgentName = "Neo"
		s.engine.userPreferredName = ""
		s.engine.userExpertiseDomains = nil
		s.engine.profileFetchedAt = time.Now()
		s.engine.profileMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}
