// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_gideon_fleet.go — Gideon fleet telemetry proxy route.
//
// Route:
//
//	GET /gideon/fleet   proxy the live Paxeer admin-dashboard fleet summary
//
// This is Gideon's authoritative real-time node_status source. The route is
// a thin read-only proxy of the upstream REST summary
// (GIDEON_FLEET_URL + /api/summary, default
// https://admin-dashboard.rpcpaxeer.online) so the SPA avoids cross-origin
// fetches and the scheduler/policy can consume the same feed server-side.
//
// Gating: always registered, but returns 404 when the daemon is NOT booted
// with -gideon-mode — matching the forge /fs and /git surfaces, which 404
// unmounted when their mode is off. Additive: forge mode is unaffected.
//
// Auth: existing bearer-token requireAuth path (MATRIX_DAEMON_TOKEN).

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// gideonFleetBase resolves the upstream fleet telemetry base URL from
// GIDEON_FLEET_URL, defaulting to the production admin dashboard. The same
// default + env contract is used by tools/gideon/rpc-bridge.mjs so the MCP
// tool and this proxy always agree on the source.
func gideonFleetBase() string {
	base := strings.TrimSpace(os.Getenv("GIDEON_FLEET_URL"))
	if base == "" {
		base = "https://admin-dashboard.rpcpaxeer.online"
	}
	return strings.TrimRight(base, "/")
}

// handleGideonFleet proxies GET /gideon/fleet → <GIDEON_FLEET_URL>/api/summary.
// The upstream JSON body is streamed back verbatim with a 15s timeout. On
// upstream transport/HTTP failure the route returns 502 with a clear error
// so the panel can surface "telemetry source unreachable" distinctly from a
// daemon fault.
func (d *daemonState) handleGideonFleet(w http.ResponseWriter, r *http.Request) {
	if !d.gideonMode {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "gideon fleet route disabled; restart daemon with -gideon-mode",
		})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !d.requireAuth(w, r) {
		return
	}

	url := gideonFleetBase() + "/api/summary"
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "gideon fleet: build upstream request: " + err.Error(),
		})
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":     "gideon fleet: upstream unreachable: " + err.Error(),
			"fleet_url": url,
		})
		return
	}
	defer resp.Body.Close()

	// Cap the proxied body so a misbehaving upstream can't stream unbounded
	// data through the daemon. The summary is ~10-20KB; 1MB is generous.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "gideon fleet: read upstream body: " + err.Error(),
		})
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":         "gideon fleet: upstream returned non-200",
			"upstream_code": resp.Status,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
