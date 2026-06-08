// Package discovery implements search (Phase 1: lexical + filters; embeddings stubbed).
package discovery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/types"
)

// Service provides discovery queries.
type Service struct {
	store *store.Store
}

// New returns a discovery Service.
func New(st *store.Store) *Service {
	return &Service{store: st}
}

// SearchRequest mirrors POST /v1/discover (docs/05-api.md §5.4).
type SearchRequest struct {
	Query   string         `json:"query"`
	Filters map[string]any `json:"filters"`
	Limit   int            `json:"limit"`
}

// Search runs lexical + filter discovery (embeddings stubbed per Phase 1).
func (s *Service) Search(ctx context.Context, req SearchRequest) (types.DiscoverResponse, error) {
	kind, _ := req.Filters["kind"].(string)
	rows, err := s.store.ListDiscoverable(ctx, req.Query, kind, req.Limit)
	if err != nil {
		return types.DiscoverResponse{}, err
	}
	results := make([]types.DiscoverResult, 0, len(rows))
	for i := range rows {
		row := rows[i]
		var ops []types.DiscoverOperation
		if len(row.Manifest) > 0 {
			var m struct {
				Pricing []struct {
					Operation string `json:"operation"`
					PriceWei  string `json:"price_wei"`
					Unit      string `json:"unit"`
				} `json:"pricing"`
			}
			_ = json.Unmarshal(row.Manifest, &m)
			for _, pr := range m.Pricing {
				ops = append(ops, types.DiscoverOperation{
					Name:     pr.Operation,
					PriceWei: pr.PriceWei,
					Unit:     pr.Unit,
				})
			}
		}
		score := RankScore(row, req.Query)
		qs := ""
		if row.QualityScore != nil {
			qs = *row.QualityScore
		}
		uptime := 0
		if row.UptimeBPS != nil {
			uptime = *row.UptimeBPS
		}
		results = append(results, types.DiscoverResult{
			ID:           row.ID,
			Slug:         row.Slug,
			DisplayName:  row.DisplayName,
			Summary:      row.Summary,
			Kind:         row.Kind,
			QualityScore: qs,
			UptimeBPS:    uptime,
			Score:        score,
			Operations:   ops,
		})
	}
	return types.DiscoverResponse{Results: results}, nil
}

// RankScore is a minimal Phase 1 ranker (semantic stub = lexical match strength).
func RankScore(row store.ServiceRow, query string) float64 {
	if query == "" {
		return 0.5
	}
	q := fmt.Sprintf("%s %s %s", row.DisplayName, row.Summary, row.Slug)
	if containsFold(q, query) {
		return 0.9
	}
	return 0.3
}

func containsFold(hay, needle string) bool {
	return needle != "" && (len(hay) >= len(needle)) && (stringIndexFold(hay, needle) >= 0)
}

func stringIndexFold(s, sub string) int {
	// small helper without importing strings in hot path tests
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
