// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"centra/agents/neo/internal/capabilityhub"
	"centra/agents/neo/internal/memory"
	neotools "centra/agents/neo/internal/tools"
)

const maxCapabilityRequestBytes = 1 << 20

type capabilityActionRequest struct {
	Version     string   `json:"version"`
	Permissions []string `json:"permissions,omitempty"`
	Pinned      *bool    `json:"pinned,omitempty"`
}

type capabilityImportRequest struct {
	SourceType capabilityhub.SourceType `json:"source_type"`
	Source     string                   `json:"source"`
	Manifest   string                   `json:"manifest,omitempty"`
	Prose      string                   `json:"prose,omitempty"`
	Provenance string                   `json:"provenance,omitempty"`
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil || s.engine.capabilities == nil {
		message := "capability hub disabled"
		if s.engine != nil && s.engine.capabilityErr != nil {
			message = "capability hub unavailable"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message})
		return
	}
	// Projection is best-effort and replayable: every lifecycle mutation first
	// commits to the Hub audit table, then this bounded drain mirrors pending
	// typed provenance to Neocortex. A Neocortex outage cannot fail this route;
	// unprojected events remain pending across restart.
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		s.engine.projectCapabilityProvenance(ctx)
	}()
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/capabilities"), "/")
	if path == "" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		state := capabilityhub.State(strings.TrimSpace(r.URL.Query().Get("state")))
		items, err := s.engine.capabilities.List(r.Context(), capabilityhub.Query{Search: r.URL.Query().Get("q"), State: state})
		if err != nil {
			s.writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"capabilities": items})
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) == 1 && parts[0] == "catalog" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if s.engine.capabilityLibrary == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "capability library unavailable"})
			return
		}
		items, err := capabilityhub.DiscoverLibrary(r.Context(), s.engine.capabilityLibrary, r.URL.Query().Get("q"), 50)
		if err != nil {
			s.writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"capabilities": items})
		return
	}
	if len(parts) == 1 && parts[0] == "import" {
		s.handleCapabilityImport(w, r)
		return
	}
	if len(parts) == 0 || len(parts) > 2 || strings.TrimSpace(parts[0]) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	slug := parts[0]
	if len(parts) == 1 {
		s.handleCapabilityVersions(w, r, slug)
		return
	}
	s.handleCapabilityAction(w, r, slug, parts[1])
}

func (e *Engine) projectCapabilityProvenance(ctx context.Context) {
	if e == nil || e.capabilities == nil || e.pager == nil {
		return
	}
	events, err := e.capabilities.PendingProvenance(ctx, 50)
	if err != nil {
		return
	}
	for _, event := range events {
		if _, err := e.pager.WriteCapabilityProvenance(ctx, memory.CapabilityProvenance{
			AuditID: event.ID, Slug: event.Slug, Version: event.Version,
			Action: event.Action, Detail: event.Detail, OccurredAt: event.CreatedAt,
		}); err != nil {
			return
		}
		if err := e.capabilities.MarkProvenanceProjected(ctx, event.ID); err != nil {
			return
		}
	}
}

func (e *Engine) snapshotCapabilities(ctx context.Context) ([]neotools.CapabilitySnapshotItem, error) {
	if e == nil || e.capabilities == nil {
		return nil, fmt.Errorf("capability hub unavailable")
	}
	active, err := e.capabilities.List(ctx, capabilityhub.Query{State: capabilityhub.StateActive})
	if err != nil {
		return nil, err
	}
	if len(active) > 64 {
		active = active[:64]
	}
	snapshot := make([]neotools.CapabilitySnapshotItem, 0, len(active))
	for _, item := range active {
		capability, instructions, err := e.capabilities.ActiveInstructions(ctx, item.Slug)
		if err != nil {
			return nil, err
		}
		snapshot = append(snapshot, neotools.CapabilitySnapshotItem{
			Slug: capability.Slug, Version: capability.Version, Display: capability.Display,
			Description: capability.Description, Digest: capability.Digest, Instructions: instructions,
		})
	}
	return snapshot, nil
}

func (e *Engine) createCapabilityCandidate(ctx context.Context, manifest, prose, provenance string) (string, error) {
	if e == nil || e.capabilities == nil {
		return "", fmt.Errorf("capability hub unavailable")
	}
	capability, err := e.capabilities.ImportAuthored(ctx, capabilityhub.AuthoredRequest{
		Manifest: manifest, Prose: prose, SourceRef: provenance,
	})
	if err != nil {
		return "", err
	}
	e.projectCapabilityProvenance(ctx)
	body, err := json.Marshal(struct {
		Capability capabilityhub.Capability `json:"capability"`
		Next       string                   `json:"next"`
	}{Capability: capability, Next: "The candidate is quarantined. The user must review permissions, verify, and activate it in Capability Hub."})
	return string(body), err
}

func (s *Server) handleCapabilityImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var request capabilityImportRequest
	if err := decodeCapabilityJSON(w, r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var (
		capability capabilityhub.Capability
		err        error
	)
	switch request.SourceType {
	case capabilityhub.SourceLibrary:
		if s.engine.capabilityLibrary == "" {
			err = fmt.Errorf("capability library unavailable")
		} else {
			capability, err = s.engine.capabilities.ImportLibrary(r.Context(), s.engine.capabilityLibrary, request.Source)
		}
	case capabilityhub.SourceURL, capabilityhub.SourceGitHub:
		capability, err = s.engine.capabilities.ImportURL(r.Context(), request.Source, request.SourceType)
	case capabilityhub.SourceAuthored:
		capability, err = s.engine.capabilities.ImportAuthored(r.Context(), capabilityhub.AuthoredRequest{
			Manifest: request.Manifest, Prose: request.Prose, SourceRef: request.Provenance,
		})
	default:
		err = fmt.Errorf("unsupported source_type")
	}
	if err != nil {
		s.writeCapabilityError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, capability)
}

func (s *Server) handleCapabilityVersions(w http.ResponseWriter, r *http.Request, slug string) {
	switch r.Method {
	case http.MethodGet:
		versions, err := s.engine.capabilities.Versions(r.Context(), slug)
		if err != nil {
			s.writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
	case http.MethodDelete:
		version := strings.TrimSpace(r.URL.Query().Get("version"))
		if version == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version is required"})
			return
		}
		if err := s.engine.capabilities.Uninstall(r.Context(), slug, version); err != nil {
			s.writeCapabilityError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleCapabilityAction(w http.ResponseWriter, r *http.Request, slug, action string) {
	if action == "audit" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		events, err := s.engine.capabilities.Audit(r.Context(), slug, atoiSafe(r.URL.Query().Get("limit")))
		if err != nil {
			s.writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var request capabilityActionRequest
	if err := decodeCapabilityJSON(w, r, &request); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var (
		capability capabilityhub.Capability
		err        error
	)
	switch action {
	case "grant":
		capability, err = s.engine.capabilities.Grant(r.Context(), slug, request.Version, request.Permissions)
	case "verify":
		available := map[string]string{}
		readOnly := map[string]bool{}
		if s.engine.tools != nil {
			for _, schema := range s.engine.tools.VerificationSchemas() {
				name := schema.Function.Name
				available[name] = name
				if separator := strings.LastIndex(name, "__"); separator >= 0 && separator+2 < len(name) {
					short := name[separator+2:]
					if previous, exists := available[short]; !exists || previous == name {
						available[short] = name
					} else {
						available[short] = ""
					}
				}
				metadata, ok := s.engine.tools.ToolEffectMetadata(name)
				readOnly[name] = ok && metadata.SideEffectClass == "read-only"
			}
		}
		verification := capabilityhub.Verification{AvailableTools: available, ReadOnlyTools: readOnly}
		if s.engine.tools != nil {
			verification.RunTool = func(name string, arguments map[string]any) (capabilityhub.ToolTestResult, error) {
				ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
				defer cancel()
				result, callErr := s.engine.tools.CallDirect(ctx, name, arguments)
				return capabilityhub.ToolTestResult{Content: result.Content, IsError: result.IsError}, callErr
			}
		}
		capability, err = s.engine.capabilities.Verify(r.Context(), slug, request.Version, verification)
	case "activate":
		capability, err = s.engine.capabilities.Activate(r.Context(), slug, request.Version)
	case "disable":
		err = s.engine.capabilities.Disable(r.Context(), slug)
	case "pin":
		if request.Pinned == nil {
			err = fmt.Errorf("pinned is required")
		} else {
			capability, err = s.engine.capabilities.Pin(r.Context(), slug, request.Version, *request.Pinned)
		}
	case "rollback":
		capability, err = s.engine.capabilities.Rollback(r.Context(), slug)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown capability action"})
		return
	}
	if err != nil {
		s.writeCapabilityError(w, err)
		return
	}
	if action == "disable" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, capability)
}

func decodeCapabilityJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCapabilityRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request must contain one JSON object")
		}
		return err
	}
	return nil
}

func (s *Server) writeCapabilityError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, capabilityhub.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, capabilityhub.ErrVersionConflict):
		status = http.StatusConflict
	case errors.Is(err, capabilityhub.ErrInvalidTransition):
		status = http.StatusConflict
	case errors.Is(err, capabilityhub.ErrGrantRequired), errors.Is(err, capabilityhub.ErrToolUnavailable), errors.Is(err, capabilityhub.ErrVerificationRequired):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, capabilityhub.ErrUnsafePackage):
		status = http.StatusBadRequest
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "unsupported") || strings.Contains(err.Error(), "unavailable"):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
