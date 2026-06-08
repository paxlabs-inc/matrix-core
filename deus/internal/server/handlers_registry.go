package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/registry"
	"github.com/paxlabs-inc/deus/pkg/types"
)

func (s *Server) mountRegistryRoutes(r chi.Router) {
	r.Route("/v1/services", func(r chi.Router) {
		r.Get("/{id}", s.handleGetService)
		r.Group(func(r chi.Router) {
			r.Use(DevDeveloperAuth(s.deps.DevMode))
			r.Post("/", s.handleCreateService)
			r.Post("/{id}/publish", s.handlePublishService)
			r.Post("/{id}/artifacts", s.handleUploadArtifact)
			r.Post("/{id}/deploy", s.handleDeployService)
			r.Get("/{id}/deployments/{deployment_id}", s.handleGetDeployment)
		})
	})
}

func (s *Server) handleCreateService(w http.ResponseWriter, r *http.Request) {
	var body types.CreateServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json body", nil)
		return
	}
	raw, err := json.Marshal(body.Manifest)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid manifest", nil)
		return
	}
	res, err := s.deps.Registry.Create(r.Context(), registry.CreateInput{
		Manifest: raw,
		Owner:    DeveloperWalletFromContext(r.Context()),
	})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, types.CreateServiceResponse{
		ID:           res.ID,
		Slug:         res.Slug,
		Status:       res.Status,
		ManifestHash: res.ManifestHash,
		Validation: types.ValidationResult{
			OK:       res.Validation.OK,
			Warnings: res.Validation.Warnings,
		},
	})
}

func (s *Server) handleGetService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	row, err := s.deps.Registry.Get(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "service not found", nil)
		return
	}
	var manifest map[string]any
	_ = json.Unmarshal(row.Manifest, &manifest)
	writeJSON(w, http.StatusOK, types.ServiceResponse{
		ID:           row.ID,
		Slug:         row.Slug,
		Status:       row.Status,
		Kind:         row.Kind,
		Mode:         row.Mode,
		DisplayName:  row.DisplayName,
		Summary:      row.Summary,
		ManifestHash: row.ManifestHash,
		ChainID:      row.ChainID,
		Manifest:     manifest,
	})
}

func (s *Server) handlePublishService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	key := strings.TrimSpace(os.Getenv("DEUS_PUBLISH_PRIVATE_KEY"))
	if key == "" {
		key = s.deps.PublishPrivateKey
	}
	if key == "" {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "publish key not configured", nil)
		return
	}
	res, err := s.deps.Registry.Publish(r.Context(), registry.PublishInput{
		ServiceID:     id,
		Owner:         DeveloperWalletFromContext(r.Context()),
		PrivateKeyHex: key,
	})
	if err != nil {
		s.writeRegistryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, types.PublishServiceResponse{
		ID:           res.ID,
		ChainID:      res.ChainID,
		Status:       res.Status,
		ManifestHash: res.ManifestHash,
		TxHash:       res.TxHash,
	})
}

func (s *Server) writeRegistryErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		writeAPIError(w, http.StatusNotFound, "not_found", msg, nil)
	case strings.Contains(msg, "forbidden"), strings.Contains(msg, "owner mismatch"):
		writeAPIError(w, http.StatusForbidden, "forbidden", msg, nil)
	case strings.Contains(msg, "invalid"):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", msg, nil)
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal_error", msg, nil)
	}
}
