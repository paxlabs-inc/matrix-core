// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	neomemory "matrix/neo/internal/memory"
)

func (s *Server) handleMemoryRecent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	spec := neomemory.TimelineQuery{Limit: memoryLimit(r.URL.Query().Get("limit"), 100)}
	s.writeTimeline(w, spec)
}

func (s *Server) handleMemoryTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	items, err := s.engine.pager.TimelineTypes()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var body struct {
		Near  string   `json:"near"`
		Types []string `json:"type"`
		AsOf  string   `json:"as_of"`
		Limit int      `json:"limit"`
		Form  string   `json:"form"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
		return
	}
	spec := neomemory.TimelineQuery{
		Near:  body.Near,
		Types: body.Types,
		Limit: memoryLimit(strconv.Itoa(body.Limit), 100),
	}
	if strings.TrimSpace(body.AsOf) != "" {
		asOf, err := time.Parse(time.RFC3339, body.AsOf)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "as_of must be RFC3339"})
			return
		}
		spec.AsOf = &asOf
	}
	s.writeTimeline(w, spec)
}

func (s *Server) handleMemoryMutate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	var req neomemory.MutationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
		return
	}
	result, err := s.engine.pager.Mutate(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) writeTimeline(w http.ResponseWriter, spec neomemory.TimelineQuery) {
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	items, total, err := s.engine.pager.Timeline(spec)
	if err != nil {
		if strings.Contains(err.Error(), "unknown memory type") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":          items,
		"total":          total,
		"total_estimate": total,
	})
}

func memoryLimit(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	if n > 200 {
		return 200
	}
	return n
}
