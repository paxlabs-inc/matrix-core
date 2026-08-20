// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	neomemory "centra/agents/neo/internal/memory"
)

func (s *Server) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/knowledge"), "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.engine.pager.KnowledgeSnapshot())
	case path == "export" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.engine.pager.ExportKnowledge())
	case path == "search" && r.Method == http.MethodPost:
		var request struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if !decodeKnowledgeJSON(w, r, &request, 1<<20) {
			return
		}
		hits, err := s.engine.pager.SearchKnowledge(request.Query, request.Limit)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": hits, "total": len(hits)})
	case path == "import" && r.Method == http.MethodPost:
		var request neomemory.KnowledgeImportRequest
		if !decodeKnowledgeJSON(w, r, &request, (2<<20)+(256<<10)) {
			return
		}
		document, err := s.engine.pager.ImportKnowledge(r.Context(), request)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, document)
	case path == "topics" && r.Method == http.MethodPost:
		var request struct {
			Name     string `json:"name"`
			ParentID string `json:"parent_id"`
		}
		if !decodeKnowledgeJSON(w, r, &request, 1<<20) {
			return
		}
		topic, err := s.engine.pager.CreateKnowledgeTopic(r.Context(), request.Name, request.ParentID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, topic)
	case strings.HasPrefix(path, "topics/") && r.Method == http.MethodPatch:
		var request struct {
			Name     string `json:"name"`
			ParentID string `json:"parent_id"`
			Archived *bool  `json:"archived"`
		}
		if !decodeKnowledgeJSON(w, r, &request, 1<<20) {
			return
		}
		topic, err := s.engine.pager.UpdateKnowledgeTopic(r.Context(), strings.TrimPrefix(path, "topics/"), request.Name, request.ParentID, request.Archived)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, topic)
	case strings.HasPrefix(path, "documents/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "documents/")
		document, ok := s.engine.pager.KnowledgeSnapshot().Documents[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "knowledge document not found"})
			return
		}
		writeJSON(w, http.StatusOK, document)
	case strings.HasPrefix(path, "documents/") && r.Method == http.MethodPatch:
		var request neomemory.KnowledgeDocumentUpdate
		if !decodeKnowledgeJSON(w, r, &request, (2<<20)+(256<<10)) {
			return
		}
		document, err := s.engine.pager.UpdateKnowledgeDocument(r.Context(), strings.TrimPrefix(path, "documents/"), request)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, document)
	default:
		w.Header().Set("Allow", strings.Join([]string{http.MethodGet, http.MethodPost, http.MethodPatch}, ", "))
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "unsupported knowledge operation", "path": path, "method": r.Method, "limit": strconv.Itoa(2 << 20)})
	}
}

func decodeKnowledgeJSON(w http.ResponseWriter, r *http.Request, target any, maximum int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maximum))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
		return false
	}
	return true
}
