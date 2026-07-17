// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Privacy controls surface (PRIV-01, launch-readiness req 8). These routes are
// Neo-owned and back onto Engine.pager — Neo's OWN Cortex actor — because the
// co-located daemon owns a SEPARATE actor and cannot see Neo's learned
// memories. The daemon's /personalization routes address the daemon actor's
// record; the authenticated product must reach Neo's, so these routes exist.
//
//   GET  /memory/consent          read the durable, auditable opt-in state
//   PUT  /memory/consent          set opt-in (default off); persisted + audited
//   GET  /memory/export           export every current user-facing memory
//   DELETE /memory/delete-all     fail-closed until ORACLE erasure lands
//   GET  /personalization          read the single profile record
//   PUT  /personalization          save/replace the single profile record
//   DELETE /personalization        tombstone the profile (single-record delete)
//   GET  /personalization/export   export the profile with version lineage

import (
	"encoding/json"
	"net/http"

	neomemory "matrix/neo/internal/memory"
)

// memoryConsentNotice is the plain-language explanation shown before opt-in
// (req 8.1). No protocol jargon; describes what durable personalization stores.
const memoryConsentNotice = "Durable memory is off by default. When you turn it on, Neo may keep useful facts, preferences, and goals from your conversations so it can remember them next time. You can inspect, edit, or delete any record, export everything, or turn memory off at any point. Turning it off stops new memory from being kept."

// handleMemoryConsent serves GET/PUT /memory/consent.
func (s *Server) handleMemoryConsent(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		state, err := s.engine.pager.MemoryConsent(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"consent": state, "notice": memoryConsentNotice})
	case http.MethodPut:
		var body struct {
			Enabled bool `json:"enabled"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
			return
		}
		state, err := s.engine.pager.SetMemoryConsent(r.Context(), body.Enabled, "user")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"consent": state, "notice": memoryConsentNotice})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleMemoryExport serves GET /memory/export — every current user-facing
// memory plus the consent state, as a downloadable JSON document.
func (s *Server) handleMemoryExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	export, err := s.engine.pager.ExportMemories(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="matrix-memory-export.json"`)
	writeJSON(w, http.StatusOK, export)
}

// handleMemoryDeleteAll serves DELETE /memory/delete-all. Complete erasure
// across primary storage, MinIO versions, caches/indexes, training stores, and
// a deletion receipt depends on ORACLE's cryptographic-erasure pipeline
// (spec/oracle task 4.1), which is not yet implemented (req 8.4). We refuse
// fail-closed rather than tombstone-all — a partial delete that leaves
// recoverable copies would be a false completion of a deletion promise.
func (s *Server) handleMemoryDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusFailedDependency, map[string]interface{}{
		"error":       "delete-all is unavailable until receipt-backed cryptographic erasure ships",
		"dependency":  "ORACLE task 4.1 (cryptographic erasure + deletion receipt)",
		"available":   false,
		"alternative": "you can delete individual records now; full erasure with a receipt will be enabled once the prerequisite lands",
	})
}

// handlePersonalization serves GET/PUT/DELETE /personalization against Neo's
// own pager (the authenticated product surface, req 8.3).
func (s *Server) handlePersonalization(w http.ResponseWriter, r *http.Request) {
	// GET /personalization/export is routed separately; guard against the
	// prefix dispatcher forwarding the export path here.
	if r.URL.Path == "/personalization/export" {
		s.handlePersonalizationExport(w, r)
		return
	}
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		prof, uri, _, err := s.engine.pager.PersonalizationProfile(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"profile": prof, "uri": uri})
	case http.MethodPut:
		var prof neomemory.PersonalizationProfile
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&prof); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
			return
		}
		uri, err := s.engine.pager.SavePersonalizationProfile(r.Context(), prof)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// An explicit Settings save of durable personal data is an affirmative
		// opt-in (req 8.1), mirroring the interview confirmation gate.
		if _, cerr := s.engine.pager.SetMemoryConsent(r.Context(), true, "user"); cerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": cerr.Error()})
			return
		}
		saved, _, _, _ := s.engine.pager.PersonalizationProfile(r.Context())
		writeJSON(w, http.StatusOK, map[string]interface{}{"profile": saved, "uri": uri})
	case http.MethodDelete:
		deleted, err := s.engine.pager.DeletePersonalization(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": deleted})
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handlePersonalizationExport serves GET /personalization/export.
func (s *Server) handlePersonalizationExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.engine == nil || s.engine.pager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory not available"})
		return
	}
	rec, err := s.engine.pager.PersonalizationExport(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="matrix-personalization-export.json"`)
	writeJSON(w, http.StatusOK, rec)
}
