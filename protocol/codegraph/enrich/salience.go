package enrich

import (
	"math"

	"centra/protocol/codegraph/model"
)

// salienceEMARate mirrors cortex/salience.EMARate (§8.3): the exponential
// moving-average alpha used to fold a fresh observation into the running
// salience so the signal tracks structure without whipsawing. Kept as a local
// constant so the codegraph module stays free of the cortex store/Pebble
// dependency that the salience package pulls in; the value is intentionally
// identical to the substrate's.
const salienceEMARate = 0.05

// salienceEdges are the incoming edge types whose fan-in defines a node's graph
// salience: how many symbols call it, reference it, or (for an interface)
// implement it. High fan-in ⇒ central ⇒ salient. This mirrors the impact/blast-
// radius edge set.
var salienceEdges = []model.EdgeType{model.EdgeCalls, model.EdgeReferences, model.EdgeImplements}

// computeSalience returns a normalized [0,1] graph salience for every
// enrichable node, derived from log-scaled reverse-edge fan-in over
// salienceEdges and normalized by the graph's maximum. Deterministic: a pure
// function of the index's edges.
func computeSalience(ix *model.Index) map[string]float64 {
	raw := map[string]float64{}
	var max float64
	for _, n := range ix.Nodes() {
		if !Enrichable(n) {
			continue
		}
		deg := 0
		for _, t := range salienceEdges {
			deg += len(ix.Reverse(n.Id, t))
		}
		r := math.Log1p(float64(deg))
		raw[n.Id] = r
		if r > max {
			max = r
		}
	}
	if max == 0 {
		return raw // all zero; leave as-is
	}
	for id, r := range raw {
		raw[id] = r / max
	}
	return raw
}

// blendSalience folds an observed salience into a prior via the EMA. A zero
// prior (never scored) adopts the observation outright so the first pass is not
// damped toward zero.
func blendSalience(prior, observed float64) float64 {
	if prior == 0 {
		return observed
	}
	return prior + salienceEMARate*(observed-prior)
}
