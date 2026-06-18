// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Trigger memories (memory capabilities v2, Phase 3). The structural fix for
// "Neo forgets a learned behavior" (e.g. "render a Construct surface while
// working"): a learned behavioral memory should surface when the CURRENT TURN
// matches its trigger, not only when global salience luck happens to float it
// into the top-K.
//
// v1 already pins learned guidance (LearnedGuidance) — but that is a single
// global top-N every turn, so once many constraints accumulate, the relevant
// one for THIS turn can fall below the cap and be silently dropped. This lane
// adds turn-MATCHED surfacing: behavioral memories (non-hard Constraints and
// Patterns carrying a trigger) whose embedded content is close to the live
// turn are surfaced regardless of their salience rank, so the right behavior
// fires on the right turn.
//
// It is read-time only over cortex's existing HNSW index (one Near query, then
// a Go-side type + cosine filter — the same shape as neighbors()/Retrieve), so
// nothing is persisted and no replay-critical byte changes. Trigger memories
// are themselves ordinary Constraint/Pattern writes (already replay-safe).
package memory

import (
	"context"
	"sort"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// triggerMatchCosine is the minimum cosine similarity between the live turn and
// a behavioral memory's embedded content for the memory to fire as a trigger.
// cortex returns HNSW distance = 1 − cosine (0 = identical), so the equivalent
// distance ceiling is 1 − triggerMatchCosine. 0.50 is deliberately stricter
// than the relation band (cosine ~0.65 / distance 0.35 is "topically similar")
// — a trigger must be clearly ABOUT this turn before it overrides salience, so
// off-topic behaviors never pin themselves (mirrors risk R5's conservatism).
const triggerMatchCosine float32 = 0.50

// triggerScanK bounds how many nearest memories the trigger lane inspects per
// turn. The Near query overshoots the Constraint/Pattern post-filter so a turn
// surrounded by Facts/Events still yields candidates, while the cap keeps the
// single vector query cheap.
const triggerScanK = 24

// triggerGuidanceMax bounds how many trigger guidance lines are injected in one
// turn so a turn that matches many behaviors can never crowd the window.
const triggerGuidanceMax = 6

// TriggeredGuidance returns behavioral guidance whose trigger matches the live
// turn by embedding similarity, independent of global salience. Sources:
//
//   - non-hard Constraints (hard ones are already pinned verbatim by
//     hardConstraints; surfacing them here would duplicate them);
//   - Patterns that carry a Trigger (an explicit "when to apply" behavior),
//     surfaced WITHOUT the procedural coverage gate because a trigger memory
//     encodes a behavior to apply now, not a proven recipe to reuse.
//
// Each result keeps its URI so the loop can attest it as USED like any other
// surfaced memory. Requires an embedder (trigger matching is inherently
// semantic); without one it returns nil so behavior stays salience-ranked.
// Best-effort: any error yields no trigger guidance (never load-bearing alone).
func (p *Pager) TriggeredGuidance(ctx context.Context, turnText string) []Snippet {
	_ = ctx
	if p == nil || !p.hasEmbedder || p.cortex == nil {
		return nil
	}
	turnText = strings.TrimSpace(turnText)
	if turnText == "" {
		return nil
	}
	res, err := p.cortex.Find(query.Query{
		Near:  turnText,
		Limit: triggerScanK,
		Form:  query.FormMedium,
	})
	if err != nil || res == nil {
		return nil
	}

	type scored struct {
		snip   Snippet
		cosine float32
	}
	var hits []scored
	for i, m := range res.Memories {
		if m.Head.Type != memory.TypeConstraint && m.Head.Type != memory.TypePattern {
			continue
		}
		d, ok := res.Distances[m.Head.ID]
		if !ok {
			continue
		}
		cosine := 1 - d
		if cosine < triggerMatchCosine {
			continue
		}
		data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
		if derr != nil {
			continue
		}
		line := triggerLine(m.Head.Type, data, res, i)
		if line == "" {
			continue
		}
		hits = append(hits, scored{
			snip: Snippet{
				Text: line,
				URI:  string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
				Type: m.Head.Type.String(),
			},
			cosine: cosine,
		})
	}
	// Strongest match first; the Near order is already distance-ascending, but
	// the type post-filter can interleave, so re-sort to keep the most
	// on-trigger guidance at the top before the cap.
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].cosine > hits[b].cosine })
	out := make([]Snippet, 0, triggerGuidanceMax)
	for _, h := range hits {
		out = append(out, h.snip)
		if len(out) >= triggerGuidanceMax {
			break
		}
	}
	return out
}

// triggerLine renders the imperative guidance for a matched trigger memory.
// Hard Constraints are skipped (already pinned verbatim). A Pattern only fires
// as a trigger when it carries an explicit Trigger field; its guidance is the
// proven path (name → steps), so "what to do when this turn shows up" is
// concrete. Returns "" to drop a candidate that should not surface.
func triggerLine(t memory.Type, data memory.TypedData, res *query.Result, i int) string {
	switch t {
	case memory.TypeConstraint:
		var cd memory.ConstraintData
		switch x := data.(type) {
		case memory.ConstraintData:
			cd = x
		case *memory.ConstraintData:
			cd = *x
		default:
			return ""
		}
		if cd.StrengthVal == memory.StrengthHard {
			return "" // already pinned verbatim by hardConstraints
		}
		return strings.TrimSpace(cd.Statement)
	case memory.TypePattern:
		var pd memory.PatternData
		switch x := data.(type) {
		case memory.PatternData:
			pd = x
		case *memory.PatternData:
			pd = *x
		default:
			return ""
		}
		spec := DecodePatternSpec(pd.Statement)
		if strings.TrimSpace(spec.Trigger) == "" {
			return "" // not a trigger memory — leave it to the procedural lane
		}
		return (Pattern{Spec: spec, Coverage: pd.Coverage}).Render()
	default:
		_ = res
		_ = i
		return ""
	}
}
