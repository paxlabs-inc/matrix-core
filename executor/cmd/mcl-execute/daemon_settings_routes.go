// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_settings_routes.go — /me, /version, /diag/*, /settings/*
// route surface (sess#27).
//
// Routes:
//
//   GET    /me              current actor identity + daemon defaults
//   GET    /version         daemon version + build metadata
//   GET    /diag/embedder   embedder running flag + provider model
//   GET    /diag/mcp        per-alias MCP server health
//   GET    /diag/bridge     bridge layer status
//   GET    /settings        read-only daemon settings (CLI-set flags)
//
// /settings PATCH is reserved for v1.1 — the v1 daemon's settings are
// CLI-flag-only (no runtime mutation surface). The frontend reads
// these read-only values to populate the settings UI without writes.

import (
	"net/http"
	"runtime"
)

// handleMe serves GET /me.
func (d *daemonState) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	embEnabled := false
	if d.infra != nil {
		embEnabled = d.infra.hasEmb
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"actor":              d.actor.UserURI,
		"agent":              d.actor.AgentURI,
		"did":                d.actor.DID,
		"default_skill":      d.defaultSkillURI,
		"compiler_model":     d.compilerModel,
		"executor_model":     d.executorModel,
		"workspace_root":     d.workspaceRoot,
		"allow_sub_dispatch": d.allowSubDispatch,
		"embedder_enabled":   embEnabled,
	})
}

// handleVersion serves GET /version. Public route (no auth required).
func (d *daemonState) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"daemon":    "mcl-execute",
		"build":     daemonBuildVersion,
		"go":        runtime.Version(),
		"sess":      "sess#27",
		"surface_v": daemonAPIVersion,
	})
}

// handleDiagEmbedder serves GET /diag/embedder.
func (d *daemonState) handleDiagEmbedder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	enabled := false
	if d.infra != nil {
		enabled = d.infra.hasEmb
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": enabled,
	})
}

// handleDiagMCP serves GET /diag/mcp.
func (d *daemonState) handleDiagMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.manager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp manager not loaded"})
		return
	}
	type srvStat struct {
		Alias     string `json:"alias"`
		Running   bool   `json:"running"`
		ToolCount int    `json:"tool_count"`
	}
	out := make([]srvStat, 0, 4)
	for _, alias := range d.infra.manager.Aliases() {
		out = append(out, srvStat{
			Alias:     alias,
			Running:   d.infra.manager.Client(alias) != nil,
			ToolCount: len(d.infra.manager.Tools(alias)),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items": out,
	})
}

// handleDiagBridge serves GET /diag/bridge.
func (d *daemonState) handleDiagBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	cortexOK := d.infra != nil && d.infra.cortex != nil
	embOK := d.infra != nil && d.infra.hasEmb
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cortex_open":      cortexOK,
		"embedder_running": embOK,
	})
}

// handleSettings serves GET /settings.
func (d *daemonState) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"compiler_model":     d.compilerModel,
		"executor_model":     d.executorModel,
		"default_skill":      d.defaultSkillURI,
		"workspace_root":     d.workspaceRoot,
		"allow_sub_dispatch": d.allowSubDispatch,
		"max_retry":          d.maxRetry,
		"seed":               d.seed,
	})
}

// daemonBuildVersion is filled in via ldflags in production builds.
// Local dev builds carry the placeholder string.
var daemonBuildVersion = "dev"

// daemonAPIVersion pins the wire-format version the frontend can
// branch on. Bumped when the route surface changes incompatibly.
const daemonAPIVersion = "1.0"

// Copyright © 2026 Paxlabs Inc. All rights reserved.
