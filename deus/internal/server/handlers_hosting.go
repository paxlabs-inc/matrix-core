package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/hosting"
	"github.com/paxlabs-inc/deus/pkg/types"
)

func (s *Server) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosting == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "hosting not configured", nil)
		return
	}
	serviceID := chi.URLParam(r, "id")
	if err := r.ParseMultipartForm(12 << 20); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "multipart required", nil)
		return
	}
	file, header, err := r.FormFile("artifact")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "artifact file required", nil)
		return
	}
	defer file.Close()
	name := header.Filename
	if v := strings.TrimSpace(r.FormValue("filename")); v != "" {
		name = v
	}
	key, err := s.deps.Hosting.UploadArtifact(r.Context(), serviceID, name, file, header.Size)
	if err != nil {
		s.writeHostingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, types.UploadArtifactResponse{
		ArtifactKey: key,
		URL:         s.deps.BlobURL(key),
	})
}

func (s *Server) handleDeployService(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosting == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "hosting not configured", nil)
		return
	}
	serviceID := chi.URLParam(r, "id")
	var body types.DeployServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json", nil)
		return
	}
	res, err := s.deps.Hosting.Deploy(r.Context(), hosting.DeployRequest{
		ServiceID:   serviceID,
		ArtifactKey: body.ArtifactKey,
		Runtime:     body.Runtime,
		AlwaysWarm:  body.AlwaysWarm,
		Region:      body.Region,
	})
	if err != nil {
		s.writeHostingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, types.DeployServiceResponse{
		DeploymentID: res.DeploymentID,
		Status:       res.Status,
		ExecEndpoint: res.ExecEndpoint,
		Runtime:      res.Runtime,
	})
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "store not configured", nil)
		return
	}
	depID := chi.URLParam(r, "deployment_id")
	row, err := s.deps.Store.GetDeployment(r.Context(), depID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "deployment not found", nil)
		return
	}
	var exec string
	if row.ExecEndpoint != nil {
		exec = *row.ExecEndpoint
	}
	writeJSON(w, http.StatusOK, types.DeploymentResponse{
		ID:           row.ID,
		ServiceID:    row.ServiceID,
		Status:       row.Status,
		Runtime:      row.Runtime,
		ExecEndpoint: exec,
		AlwaysWarm:   row.AlwaysWarm,
	})
}

func (s *Server) writeHostingErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		writeAPIError(w, http.StatusNotFound, "not_found", msg, nil)
	case strings.Contains(msg, "budget"), strings.Contains(msg, "kill-switch"):
		writeAPIError(w, http.StatusConflict, "hosting_budget_exceeded", msg, nil)
	case strings.Contains(msg, "not hosted"):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", msg, nil)
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal_error", msg, nil)
	}
}
