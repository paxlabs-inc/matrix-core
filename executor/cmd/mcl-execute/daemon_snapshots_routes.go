// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_snapshots_routes.go — /snapshots/* route surface (sess#27).
//
// Routes:
//
//   GET  /snapshots          list S3-stored snapshots (timestamps)
//   POST /snapshots/push     manual on-demand push
//
// Backed by executor/internal/snapshot.Manager which the daemon
// constructs at boot when -snapshot-endpoint is supplied. All routes
// 503 when the manager is nil (snapshot-disabled local-dev mode).

import (
	"context"
	"net/http"
	"time"
)

// handleSnapshotsList serves GET /snapshots.
//
// v1: returns the most-recent push timestamp (when known) and the
// configured S3 layout. Full enumeration of users/<id>/snapshots/*
// would require an mc ls call which costs network IO; the frontend
// uses /snapshots/push to take a fresh one and the backend timestamp
// is sufficient for the "last known" UI.
func (d *daemonState) handleSnapshotsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.snapMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "snapshot manager not enabled (-snapshot-endpoint flag absent)",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":     true,
		"seeded":      d.snapMgr.IsSeeded(),
		"seeded_path": d.snapMgr.SeededPath(),
		"actor":       d.actor.UserURI,
		"note":        "v1: full S3 listing via /snapshots/list lands when mc ls is wired",
	})
}

// handleSnapshotsPush serves POST /snapshots/push.
//
// Synchronous: the response is held until the tar+upload completes
// (typically 5-30s). The frontend should show a spinner.
func (d *daemonState) handleSnapshotsPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.snapMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "snapshot manager not enabled",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	key, err := d.snapMgr.Push(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "snapshot push: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "pushed",
		"key":    key,
		"at":     time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
