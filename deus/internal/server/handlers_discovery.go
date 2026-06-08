package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/discovery"
	"github.com/paxlabs-inc/deus/pkg/types"
)

func (s *Server) mountDiscoveryRoutes(r chi.Router) {
	r.Get("/v1/discover", s.handleDiscoverGET)
	r.Post("/v1/discover", s.handleDiscoverPOST)
}

func (s *Server) handleDiscoverGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	kind := r.URL.Query().Get("kind")
	res, err := s.deps.Discovery.Search(r.Context(), discovery.SearchRequest{
		Query: q,
		Filters: map[string]any{
			"kind": kind,
		},
		Limit: 10,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDiscoverPOST(w http.ResponseWriter, r *http.Request) {
	var body types.DiscoverRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json body", nil)
		return
	}
	filters := map[string]any{}
	for k, v := range body.Filters {
		filters[k] = v
	}
	res, err := s.deps.Discovery.Search(r.Context(), discovery.SearchRequest{
		Query:   body.Query,
		Filters: filters,
		Limit:   body.Limit,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
