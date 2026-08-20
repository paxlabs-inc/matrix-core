// Package salience computes graph salience scores from reverse fan-in.
// Salience measures how "important" a node is based on how many other nodes
// depend on it via calls, references, and implements edges.
package salience

import (
	"math"
	"sort"

	"centra/protocol/codegraph-ultra/internal/model"
	"centra/protocol/codegraph-ultra/internal/store"
)

// ImpactEdges are the edge types used for salience computation.
var ImpactEdges = []model.EdgeType{
	model.EdgeCalls,
	model.EdgeReferences,
	model.EdgeImplements,
}

// Scorer computes and stores salience scores for all symbol-level nodes.
type Scorer struct {
	db *store.DB
}

// New creates a new salience scorer.
func New(db *store.DB) *Scorer {
	return &Scorer{db: db}
}

// Compute calculates salience for all enrichable node kinds and stores them.
// Salience is based on reverse fan-in (how many nodes depend on this one).
// The score is normalized to [0,1] and blended with any prior salience via EMA.
func (s *Scorer) Compute() (int, error) {
	ix := s.db.LoadIndex()

	// Compute raw reverse fan-in for each node over impact edges.
	rawScores := make(map[string]float64)
	for id := range ix.Nodes {
		degree := 0
		for _, et := range ImpactEdges {
			degree += len(ix.Reverse[id][et])
		}
		if degree > 0 {
			rawScores[id] = math.Log1p(float64(degree))
		}
	}

	// Normalize to [0,1].
	maxScore := 0.0
	for _, v := range rawScores {
		if v > maxScore {
			maxScore = v
		}
	}
	if maxScore > 0 {
		for id := range rawScores {
			rawScores[id] /= maxScore
		}
	}

	// Sort IDs for deterministic output.
	ids := make([]string, 0, len(rawScores))
	for id := range rawScores {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Update nodes with blended salience (EMA alpha=0.05).
	const alpha = 0.05
	updated := 0
	for _, id := range ids {
		newScore := rawScores[id]
		n := s.db.GetNode(id)
		if n == nil {
			continue
		}

		var blended float64
		if n.Enrich.Salience > 0 {
			blended = alpha*newScore + (1-alpha)*n.Enrich.Salience
		} else {
			blended = newScore
		}

		n.Enrich.Salience = blended
		if err := s.db.UpsertNode(n); err != nil {
			continue
		}
		updated++
	}

	return updated, nil
}

// ComputeForIndex computes salience scores for an in-memory index.
// Returns the salience map without persisting.
func ComputeForIndex(ix *model.Index) map[string]float64 {
	rawScores := make(map[string]float64)
	for id := range ix.Nodes {
		degree := 0
		for _, et := range ImpactEdges {
			degree += len(ix.Reverse[id][et])
		}
		if degree > 0 {
			rawScores[id] = math.Log1p(float64(degree))
		}
	}

	maxScore := 0.0
	for _, v := range rawScores {
		if v > maxScore {
			maxScore = v
		}
	}
	if maxScore > 0 {
		for id := range rawScores {
			rawScores[id] /= maxScore
		}
	}
	return rawScores
}
