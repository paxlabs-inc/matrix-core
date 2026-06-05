// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_cortex_routes.go — /cortex/* introspection routes (sess#27).
//
// Routes:
//
//   GET /cortex/snapshot    overall_root + per-namespace state roots
//   GET /cortex/stats       memory totals, tombstone count, journal size
//   GET /cortex/replay      pre/post replay validation
//
// All routes are read-only; they do not perturb cortex state.

import (
	"encoding/hex"
	"net/http"

	"matrix/cortex/memory"
)

// handleCortexSnapshot serves GET /cortex/snapshot.
func (d *daemonState) handleCortexSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	root, err := d.infra.cortex.OverallRoot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "overall_root: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"overall_root": hex.EncodeToString(root[:]),
		"actor":        d.actor.UserURI,
		"agent":        d.actor.AgentURI,
	})
}

// handleCortexStats serves GET /cortex/stats.
//
// Per-type memory counts derived from cortex.ListByType (cheap key-
// scan; no body decode). Journal size is left unset in v1 since the
// backing pebble store doesn't expose a public size accessor at the
// cortex API surface.
func (d *daemonState) handleCortexStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	type entry struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	stats := make([]entry, 0, 9)
	total := 0
	for _, t := range allMemoryTypes() {
		ids, err := d.infra.cortex.ListByType(t, 0)
		if err != nil {
			continue
		}
		stats = append(stats, entry{Type: t.String(), Count: len(ids)})
		total += len(ids)
	}
	root, _ := d.infra.cortex.OverallRoot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"overall_root":   hex.EncodeToString(root[:]),
		"total_memories": total,
		"per_type":       stats,
		"actor":          d.actor.UserURI,
	})
}

// handleCortexReplay serves GET /cortex/replay.
//
// v1 stub: returns the current overall_root as the "post" root and
// notes that replay validation is the snapshot-package's domain.
// Future: pull pre-root from the most recent snapshot, post-root from
// the live store, and compare.
func (d *daemonState) handleCortexReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	root, err := d.infra.cortex.OverallRoot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"overall_root": hex.EncodeToString(root[:]),
		"replay":       "live",
		"note":         "v1: replay validation against the most recent snapshot lands when /snapshots/replay is wired",
	})
}

// _ avoids an unused-import on memory.URI in case future routes need it.
var _ = memory.URI("")

// Copyright © 2026 Paxlabs Inc. All rights reserved.
