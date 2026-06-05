// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_tools_routes.go — /tools/*, /servers, /agents/manifest
// route surface (sess#27).
//
// Routes:
//
//   GET  /tools                       flat list of every registered tool
//   GET  /tools/:alias/:name          tool detail (input_schema + side_effect)
//   GET  /servers                     MCP server status per alias
//   GET  /agents/manifest             current agent manifest body
//
// All routes read from the live mcp.Manager + tool.Registry that the
// daemon booted with. Manifest reload is admin-only (POST is
// reserved; not implemented in v1).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"matrix/executor/mcp"
	"matrix/executor/tool"
)

// toolDTO is the wire shape for one tool.
type toolDTO struct {
	URI             string          `json:"uri"`
	Server          string          `json:"server"`
	Name            string          `json:"name"`
	Version         string          `json:"version"`
	Description     string          `json:"description,omitempty"`
	SideEffectClass string          `json:"side_effect_class"`
	InputSchema     json.RawMessage `json:"input_schema,omitempty"`
}

// serverDTO is the wire shape for one MCP server.
type serverDTO struct {
	Alias         string    `json:"alias"`
	Transport     string    `json:"transport"`
	Version       string    `json:"version"`
	PackageDigest string    `json:"package_digest,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
	ToolCount     int       `json:"tool_count"`
	Tools         []toolDTO `json:"tools"`
	Running       bool      `json:"running"`
}

// handleToolsList serves GET /tools.
func (d *daemonState) handleToolsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.manifest == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "manifest not loaded"})
		return
	}
	out := buildToolListing(d.infra.manifest, d.infra.manager)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":          out,
		"total_estimate": len(out),
	})
}

// handleToolDetail serves GET /tools/:alias/:name.
func (d *daemonState) handleToolDetail(w http.ResponseWriter, r *http.Request, alias, name string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.manifest == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "manifest not loaded"})
		return
	}
	for _, srv := range d.infra.manifest.Servers {
		if srv.Alias != alias {
			continue
		}
		schemaByName := liveToolSchemas(d.infra.manager, alias)
		for _, te := range srv.Tools {
			if te.Name != name {
				continue
			}
			dto := toolDTO{
				URI:             toolURI(srv.Alias, te.Name, srv.Version),
				Server:          srv.Alias,
				Name:            te.Name,
				Version:         srv.Version,
				Description:     te.Description,
				SideEffectClass: te.SideEffectClass,
				InputSchema:     schemaByName[te.Name],
			}
			writeJSON(w, http.StatusOK, dto)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": fmt.Sprintf("tool %s/%s not found", alias, name),
	})
}

// handleServersList serves GET /servers.
func (d *daemonState) handleServersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.manifest == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "manifest not loaded"})
		return
	}
	out := make([]serverDTO, 0, len(d.infra.manifest.Servers))
	for _, srv := range d.infra.manifest.Servers {
		schemaByName := liveToolSchemas(d.infra.manager, srv.Alias)
		tools := make([]toolDTO, 0, len(srv.Tools))
		for _, te := range srv.Tools {
			tools = append(tools, toolDTO{
				URI:             toolURI(srv.Alias, te.Name, srv.Version),
				Server:          srv.Alias,
				Name:            te.Name,
				Version:         srv.Version,
				Description:     te.Description,
				SideEffectClass: te.SideEffectClass,
				InputSchema:     schemaByName[te.Name],
			})
		}
		dto := serverDTO{
			Alias:         srv.Alias,
			Transport:     srv.Transport,
			Version:       srv.Version,
			PackageDigest: srv.PackageDigest,
			Endpoint:      srv.Endpoint,
			ToolCount:     len(srv.Tools),
			Tools:         tools,
			Running:       d.infra.manager != nil && d.infra.manager.Client(srv.Alias) != nil,
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}

// handleAgentsManifest serves GET /agents/manifest.
func (d *daemonState) handleAgentsManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.manifest == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "manifest not loaded"})
		return
	}
	writeJSON(w, http.StatusOK, d.infra.manifest)
}

// handleToolsRouter dispatches /tools/* (with sub-path detail).
func (d *daemonState) handleToolsRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/tools")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		d.handleToolsList(w, r)
		return
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "tool detail requires /tools/:alias/:name",
		})
		return
	}
	d.handleToolDetail(w, r, parts[0], parts[1])
}

// ---- helpers ----

// buildToolListing flattens an AgentManifest into the wire-shape list.
func buildToolListing(m *tool.AgentManifest, mgr *mcp.Manager) []toolDTO {
	out := make([]toolDTO, 0, 32)
	for _, srv := range m.Servers {
		schemaByName := liveToolSchemas(mgr, srv.Alias)
		for _, te := range srv.Tools {
			out = append(out, toolDTO{
				URI:             toolURI(srv.Alias, te.Name, srv.Version),
				Server:          srv.Alias,
				Name:            te.Name,
				Version:         srv.Version,
				Description:     te.Description,
				SideEffectClass: te.SideEffectClass,
				InputSchema:     schemaByName[te.Name],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

// liveToolSchemas returns name→inputSchema for one alias, pulled from
// the live mcp.Manager. Returns an empty map when the manager doesn't
// have that alias loaded yet.
func liveToolSchemas(mgr *mcp.Manager, alias string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if mgr == nil {
		return out
	}
	for _, t := range mgr.Tools(alias) {
		if len(t.InputSchema) > 0 {
			out[t.Name] = t.InputSchema
		}
	}
	return out
}

// toolURI renders matrix://tool/mcp/<alias>/<name>@<version>.
func toolURI(alias, name, version string) string {
	return fmt.Sprintf("matrix://tool/mcp/%s/%s@%s", alias, name, version)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
