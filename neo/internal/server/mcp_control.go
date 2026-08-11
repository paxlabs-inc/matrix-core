// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"errors"
	"net/http"
	"strings"

	"matrix/neo/internal/mcpcontrol"
)

type mcpClassificationRequest struct {
	Tools []mcpcontrol.Classification `json:"tools"`
}

func (s *Server) handleMCPControl(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil || s.engine.mcpControl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP control plane unavailable"})
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/integrations/mcp"), "/")
	if path == "" {
		s.handleMCPRoot(w, r)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[0] == "tools" && parts[1] == "search" {
		if r.Method != http.MethodGet {
			s.writeMCPError(w, errors.New("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		items, err := s.engine.mcpControl.SearchTools(r.Context(), r.URL.Query().Get("q"), atoiSafe(r.URL.Query().Get("limit")))
		if err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tools": items})
		return
	}
	if len(parts) == 2 && parts[0] == "oauth" && parts[1] == "callback" {
		if r.Method != http.MethodGet {
			s.writeMCPError(w, errors.New("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		server, err := s.engine.mcpControl.FinishOAuth(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("code"))
		if err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		writeJSON(w, http.StatusOK, server)
		return
	}
	if len(parts) < 1 || len(parts) > 2 || strings.TrimSpace(parts[0]) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	alias := parts[0]
	if len(parts) == 1 {
		s.handleMCPServer(w, r, alias)
		return
	}
	s.handleMCPAction(w, r, alias, parts[1])
}

func (s *Server) handleMCPRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		servers, err := s.engine.mcpControl.List(r.Context())
		if err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"servers": servers, "runtime_error": boundedControlError(s.engine.mcpControlErr)})
	case http.MethodPost:
		var request mcpcontrol.CreateRequest
		if err := decodeCapabilityJSON(w, r, &request); err != nil {
			s.writeMCPError(w, err, http.StatusBadRequest)
			return
		}
		server, err := s.engine.mcpControl.Put(r.Context(), request)
		if err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		writeJSON(w, http.StatusCreated, server)
	default:
		s.writeMCPError(w, errors.New("method not allowed"), http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPServer(w http.ResponseWriter, r *http.Request, alias string) {
	switch r.Method {
	case http.MethodGet:
		server, err := s.engine.mcpControl.Get(r.Context(), alias)
		if err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		writeJSON(w, http.StatusOK, server)
	case http.MethodDelete:
		if err := s.engine.mcpControl.Delete(r.Context(), alias); err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		applied, err := s.engine.applyMCPForced(r.Context())
		if err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": alias, "applied": applied})
	default:
		s.writeMCPError(w, errors.New("method not allowed"), http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPAction(w http.ResponseWriter, r *http.Request, alias, action string) {
	if action == "audit" {
		if r.Method != http.MethodGet {
			s.writeMCPError(w, errors.New("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		events, err := s.engine.mcpControl.Audit(r.Context(), alias, atoiSafe(r.URL.Query().Get("limit")))
		if err != nil {
			s.writeMCPError(w, err, 0)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events})
		return
	}
	if r.Method != http.MethodPost {
		s.writeMCPError(w, errors.New("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	var (
		server  mcpcontrol.Server
		applied bool
		err     error
	)
	switch action {
	case "probe":
		server, err = s.engine.mcpControl.Probe(r.Context(), alias)
	case "classify":
		var request mcpClassificationRequest
		if decodeErr := decodeCapabilityJSON(w, r, &request); decodeErr != nil {
			err = decodeErr
			break
		}
		server, err = s.engine.mcpControl.Classify(r.Context(), alias, request.Tools)
	case "enable":
		server, err = s.engine.mcpControl.Enable(r.Context(), alias, true)
		if err == nil {
			applied, err = s.engine.applyMCPForced(r.Context())
			if refreshed, getErr := s.engine.mcpControl.Get(r.Context(), alias); getErr == nil {
				server = refreshed
			}
		}
	case "disable":
		server, err = s.engine.mcpControl.Enable(r.Context(), alias, false)
		if err == nil {
			applied, err = s.engine.applyMCPForced(r.Context())
			if refreshed, getErr := s.engine.mcpControl.Get(r.Context(), alias); getErr == nil {
				server = refreshed
			}
		}
	case "rollback":
		server, err = s.engine.mcpControl.Rollback(r.Context(), alias)
		if err == nil {
			applied, err = s.engine.applyMCPForced(r.Context())
			if refreshed, getErr := s.engine.mcpControl.Get(r.Context(), alias); getErr == nil {
				server = refreshed
			}
		}
	case "oauth-start":
		var start mcpcontrol.OAuthStart
		start, err = s.engine.mcpControl.StartOAuth(r.Context(), alias)
		if err == nil {
			writeJSON(w, http.StatusOK, start)
			return
		}
	case "oauth-revoke":
		err = s.engine.mcpControl.ClearOAuth(r.Context(), alias)
		if err == nil {
			server, err = s.engine.mcpControl.Get(r.Context(), alias)
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		s.writeMCPError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": server, "applied": applied})
}

func (s *Server) writeMCPError(w http.ResponseWriter, err error, status int) {
	if status == 0 {
		switch {
		case errors.Is(err, mcpcontrol.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, mcpcontrol.ErrConflict):
			status = http.StatusConflict
		case errors.Is(err, mcpcontrol.ErrEncryptionRequired):
			status = http.StatusServiceUnavailable
		case errors.Is(err, mcpcontrol.ErrUnclassified), errors.Is(err, mcpcontrol.ErrUnhealthy), errors.Is(err, mcpcontrol.ErrOAuthState):
			status = http.StatusUnprocessableEntity
		default:
			status = http.StatusBadRequest
		}
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 1000 {
		message = message[:1000]
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func boundedControlError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}
