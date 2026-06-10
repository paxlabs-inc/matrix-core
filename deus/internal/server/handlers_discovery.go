package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/discovery"
	"github.com/paxlabs-inc/deus/pkg/types"
)

func (s *Server) mountDiscoveryRoutes(r chi.Router) {
	r.Get("/v1/discover", s.handleDiscoverGET)
	r.Post("/v1/discover", s.handleDiscoverPOST)
	r.Get("/v1/catalog", s.handleCatalog)
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "store not configured", nil)
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 20)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	rows, total, err := s.deps.Store.ListPublishedServices(r.Context(), limit, offset)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	items := make([]types.CatalogItem, 0, len(rows))
	for _, row := range rows {
		item := types.CatalogItem{
			ID:           row.ID,
			Slug:         row.Slug,
			Kind:         row.Kind,
			Mode:         row.Mode,
			DisplayName:  row.DisplayName,
			Summary:      row.Summary,
			Status:       row.Status,
			ManifestHash: row.ManifestHash,
		}
		if row.QualityScore != nil {
			item.QualityScore = *row.QualityScore
		}
		if row.UptimeBPS != nil {
			item.UptimeBPS = *row.UptimeBPS
		}
		enrichCatalogItem(&item, row.Manifest)
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, types.CatalogResponse{
		Services: items,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
	})
}

// enrichCatalogItem surfaces headline pricing and tags from the stored
// manifest so catalog cards can render price without a second discover call.
func enrichCatalogItem(item *types.CatalogItem, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var m struct {
		Tags    []string `json:"tags"`
		Pricing []struct {
			PriceWei string `json:"price_wei"`
			Unit     string `json:"unit"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	item.Tags = m.Tags
	if len(m.Pricing) > 0 {
		item.PriceWei = m.Pricing[0].PriceWei
		item.Unit = m.Pricing[0].Unit
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
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
