// Package discovery implements semantic search with graceful degradation (docs/07-discovery.md).
package discovery

import (
	"context"
	"encoding/json"
	"math/big"
	"sort"

	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/types"
)

// Service provides discovery queries.
type Service struct {
	store   *store.Store
	embed   Embedder
	weights RankingWeights
}

// Option configures discovery Service.
type Option func(*Service)

// WithEmbedder sets the embedding backend.
func WithEmbedder(e Embedder) Option {
	return func(s *Service) { s.embed = e }
}

// WithRankingWeights sets blend weights.
func WithRankingWeights(w RankingWeights) Option {
	return func(s *Service) { s.weights = w }
}

// New returns a discovery Service.
func New(st *store.Store, opts ...Option) *Service {
	s := &Service{
		store:   st,
		embed:   NewHashEmbedder(),
		weights: DefaultRankingWeights(),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SearchRequest mirrors POST /v1/discover (docs/05-api.md §5.4).
type SearchRequest struct {
	Query   string         `json:"query"`
	Filters map[string]any `json:"filters"`
	Limit   int            `json:"limit"`
}

// Search runs constraint extraction → embed → vector + lexical union → blended rank.
func (s *Service) Search(ctx context.Context, req SearchRequest) (types.DiscoverResponse, error) {
	limit := req.Limit
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	extracted := ExtractConstraints(req.Query, req.Filters)
	kind, _ := extracted.Filters["kind"].(string)
	if kind == "" {
		kind = filterString(extracted.Filters, "kind")
	}
	maxPrice := filterString(extracted.Filters, "max_price_wei")
	minUptime := toIntAny(extracted.Filters["min_uptime_bps"])

	candidates := map[string]store.DiscoverCandidate{}
	merge := func(rows []store.DiscoverCandidate) {
		for i := range rows {
			row := rows[i]
			prev, ok := candidates[row.ID]
			if !ok {
				candidates[row.ID] = row
				continue
			}
			if row.SemanticSim > prev.SemanticSim {
				prev.SemanticSim = row.SemanticSim
			}
			if row.LexicalRank > prev.LexicalRank {
				prev.LexicalRank = row.LexicalRank
			}
			candidates[row.ID] = prev
		}
	}

	semanticQ := extracted.SemanticQuery
	if semanticQ != "" && s.embed != nil && s.embed.Semantic() {
		vec, err := s.embed.Embed(ctx, semanticQ)
		if err == nil {
			vectorHits, err := s.store.VectorSearchDiscover(ctx, vec, kind, limit*3)
			if err == nil {
				merge(vectorHits)
			}
		}
		// Graceful degradation: embed/vector failures fall through to lexical.
	}
	if semanticQ != "" {
		lexHits, err := s.store.LexicalSearchDiscover(ctx, semanticQ, kind, limit*3)
		if err == nil {
			merge(lexHits)
		}
	}
	if len(candidates) == 0 {
		browse, err := s.store.ListDiscoverCandidates(ctx, kind, limit*3)
		if err != nil {
			return types.DiscoverResponse{}, err
		}
		merge(browse)
	}

	type scored struct {
		row   store.DiscoverCandidate
		score float64
	}
	scoredRows := make([]scored, 0, len(candidates))
	for id := range candidates {
		c := candidates[id]
		if minUptime > 0 && (c.UptimeBPS == nil || *c.UptimeBPS < minUptime) {
			continue
		}
		minPrice, _ := s.store.MinPriceWeiForService(ctx, c.ID)
		if maxPrice != "" && minPrice != "" {
			if priceAbove(maxPrice, minPrice) {
				continue
			}
		}
		sc := BlendScoreWithPrice(s.weights, c, maxPrice, minPrice)
		scoredRows = append(scoredRows, scored{row: c, score: sc})
	}
	sort.Slice(scoredRows, func(i, j int) bool {
		return scoredRows[i].score > scoredRows[j].score
	})
	if len(scoredRows) > limit {
		scoredRows = scoredRows[:limit]
	}

	results := make([]types.DiscoverResult, 0, len(scoredRows))
	for i := range scoredRows {
		row := scoredRows[i].row
		ops := pricingOps(row.Manifest)
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
			Score:        scoredRows[i].score,
			Operations:   ops,
		})
	}
	return types.DiscoverResponse{Results: results}, nil
}

func pricingOps(manifest json.RawMessage) []types.DiscoverOperation {
	if len(manifest) == 0 {
		return nil
	}
	ops := make([]types.DiscoverOperation, 0, 4)
	var m struct {
		Pricing []struct {
			Operation string `json:"operation"`
			PriceWei  string `json:"price_wei"`
			Unit      string `json:"unit"`
		} `json:"pricing"`
	}
	_ = json.Unmarshal(manifest, &m)
	for _, pr := range m.Pricing {
		ops = append(ops, types.DiscoverOperation{
			Name: pr.Operation, PriceWei: pr.PriceWei, Unit: pr.Unit,
		})
	}
	return ops
}

func priceAbove(maxPriceWei, minPriceWei string) bool {
	maxP, ok1 := new(big.Int).SetString(maxPriceWei, 10)
	minP, ok2 := new(big.Int).SetString(minPriceWei, 10)
	if !ok1 || !ok2 {
		return false
	}
	return minP.Cmp(maxP) > 0
}
