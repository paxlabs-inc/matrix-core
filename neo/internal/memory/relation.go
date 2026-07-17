// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/forms"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// Relation is how a freshly consolidated memory relates to an existing nearby
// one of the same type. It is the decision the conflict-aware write path makes
// instead of the old binary keep/skip dedup, and it maps onto a cortex edge so
// retrieval can later cascade over the relationship (supersession filtering,
// contradiction surfacing). The string values are the wire shape the cheap
// consolidator model emits in its classification JSON.
type Relation string

const (
	// RelationNew: distinct assertion — write it, link nothing.
	RelationNew Relation = "new"
	// RelationDuplicate: near-identical — skip the write (recency is
	// refreshed instead via the Attest usage loop when it is surfaced).
	RelationDuplicate Relation = "duplicate"
	// RelationSupersedes: refines/replaces the older memory (e.g. a
	// correction or an updated preference). Write the new one, then link
	// new -> old with EdgeSupersedes so the stale one drops out of retrieval.
	RelationSupersedes Relation = "supersedes"
	// RelationContradicts: conflicts with the older memory. Write the new
	// one, then link new -> old with EdgeContradicts so both surface with a
	// reconcile-first annotation rather than one silently winning.
	RelationContradicts Relation = "contradicts"
	// RelationRelates: topically related but a distinct assertion. Link with
	// EdgeReferences (a soft, benign pointer) for cascade context.
	RelationRelates Relation = "relates"
)

// ParseRelation coerces a free-form classifier string to a Relation, defaulting
// to RelationNew (the safe, edge-free outcome) when unrecognized.
func ParseRelation(s string) Relation {
	switch Relation(strings.ToLower(strings.TrimSpace(s))) {
	case RelationDuplicate:
		return RelationDuplicate
	case RelationSupersedes:
		return RelationSupersedes
	case RelationContradicts:
		return RelationContradicts
	case RelationRelates:
		return RelationRelates
	default:
		return RelationNew
	}
}

// edgeType maps a Relation onto its cortex edge byte. The second return is
// false for relations that imply no edge (new/duplicate).
func (r Relation) edgeType() (memory.EdgeType, bool) {
	switch r {
	case RelationSupersedes:
		return memory.EdgeSupersedes, true
	case RelationContradicts:
		return memory.EdgeContradicts, true
	case RelationRelates:
		return memory.EdgeReferences, true
	default:
		return 0, false
	}
}

// Neighbor is one nearby existing memory considered as a relation target.
type Neighbor struct {
	URI      string
	Text     string
	Type     string
	Distance float32 // HNSW distance (0 = identical) from the candidate write
}

// RelationClassifier decides how a new statement relates to its nearest
// existing same-type neighbors. It is implemented by the consolidator using
// the cheap background model and is invoked by the conflict-aware write path
// ONLY when a neighbor lands in the relation band (similar but not identical),
// so the common "nothing nearby" case never spends a model call. It returns
// the relation, the chosen target URI (must be one of candidates' URIs), and
// an optional free-text hint carried verbatim on the edge (e.g. which fields
// conflict). A nil classifier means "dedup-or-write, never link" — the v1
// behavior — so non-consolidator callers are unaffected.
type RelationClassifier func(ctx context.Context, newText string, candidates []Neighbor) (rel Relation, targetURI, hint string)

// relationBandDistance is the upper HNSW-distance bound for treating a neighbor
// as a relation candidate worth a classifier call: topically similar (cosine
// ~>=0.65) but beyond the tight duplicate threshold. Conservative on purpose —
// a wider band would burn cheap-model calls on unrelated memories and risk
// noisy contradiction edges (risk R5).
const relationBandDistance float32 = 0.35

// neighbors returns up to k live same-type memories nearest to text, ascending
// by distance. text MUST be the exact render cortex embeds for the candidate
// write — forms.RenderFull(head, data) — so the distances line up with the
// dedup path. The Find limit overshoots k so the same-type post-filter still
// yields up to k. Empty when text is blank or the embedder is unavailable.
func (p *Pager) neighbors(t memory.Type, text string, k int) []Neighbor {
	if !p.hasEmbedder || k <= 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	res, err := p.cortex.Find(query.Query{Near: text, Limit: k + 4, Form: query.FormMedium})
	if err != nil || res == nil {
		return nil
	}
	out := make([]Neighbor, 0, k)
	for i, m := range res.Memories {
		if m.Head.Type != t {
			continue
		}
		d, ok := res.Distances[m.Head.ID]
		if !ok {
			continue
		}
		ntext := ""
		if i < len(res.Rendered) {
			ntext = res.Rendered[i]
		}
		if ntext == "" {
			ntext = m.Version.Forms.Medium
		}
		out = append(out, Neighbor{
			URI:      string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
			Text:     strings.TrimSpace(ntext),
			Type:     m.Head.Type.String(),
			Distance: d,
		})
		if len(out) >= k {
			break
		}
	}
	return out
}

// relate links a freshly written memory (fromURI) to an existing one (toURI)
// via the cortex edge implied by rel. Edges are bidirectional in storage (one
// record readable both ways), so a single AddEdge is enough; supersedes /
// contradicts are written new -> old so the inbound-supersedes filter and
// contradiction surfacing read the direction consistently. No-op for relations
// that imply no edge, for unparseable URIs, or for a self-edge (same memory).
func (p *Pager) relate(fromURI, toURI string, rel Relation, hint string) error {
	et, ok := rel.edgeType()
	if !ok {
		return nil
	}
	_, fromID, _, err := cortex.ParseURI(memory.URI(fromURI))
	if err != nil {
		return err
	}
	_, toID, _, err := cortex.ParseURI(memory.URI(toURI))
	if err != nil {
		return err
	}
	if fromID == toID {
		return nil // a memory cannot relate to itself
	}
	if err := p.cortex.AddEdge(fromID, et, toID, cortex.AddEdgeMeta{
		CreatedBy: p.cfg.CortexActor,
		Data:      []byte(strings.TrimSpace(hint)),
	}); err != nil {
		return err
	}
	// v3 #2: a supersedes edge (new -> old) closes the OLD memory's
	// valid-time so retrieval's O(1) validity check drops it instead of an
	// inbound-edge walk; the edge itself stays for cascade provenance and
	// "what changed" answers. CloseValidity defaults the close instant to the
	// cortex clock (≈ the successor's valid-from, written this same step) and
	// is idempotent, so a re-relate / edge revive doesn't churn versions.
	if et == memory.EdgeSupersedes {
		if _, err := p.cortex.CloseValidity(memory.URI(toURI), time.Time{}, p.cfg.CortexActor); err != nil {
			return err
		}
	}
	return nil
}

// writeWithRelations is the conflict-aware write core shared by every Remember*
// method. It finds the nearest live same-type memory to data and decides:
//
//   - distance <= dupDistanceThreshold      -> duplicate; skip the write.
//   - dup < distance <= relationBandDistance -> ask classify (when non-nil)
//     for the relation; on supersedes/contradicts/relates write the new
//     memory and link the edge to the chosen target; on duplicate skip.
//   - distance > band, no neighbor, or no embedder -> plain new write.
//
// A nil classifier collapses this to the v1 dedup-or-write behavior (no edges),
// keeping non-consolidator callers and existing tests byte-for-byte unchanged.
func (p *Pager) writeWithRelations(ctx context.Context, t memory.Type, data memory.TypedData, importance uint8, classify RelationClassifier) (string, error) {
	var newText string
	if p.hasEmbedder && data != nil {
		newText = forms.RenderFull(&memory.Head{Type: t}, data)
	}
	if cands := p.neighbors(t, newText, 5); len(cands) > 0 {
		nearest := cands[0] // Find orders by distance ascending
		if nearest.Distance <= dupDistanceThreshold {
			return "", nil // duplicate — recency refreshed via Attest when surfaced
		}
		if classify != nil && nearest.Distance <= relationBandDistance {
			rel, targetURI, hint := classify(ctx, newText, cands)
			switch rel {
			case RelationDuplicate:
				return "", nil
			case RelationSupersedes:
				if validCandidateURI(targetURI, cands) {
					uri, err := p.cortex.Supersede(memory.URI(targetURI), data, cortex.SupersedeOptions{
						Head:      p.head(importance),
						WriteMeta: p.writeMeta(),
						EdgeMeta: cortex.AddEdgeMeta{
							CreatedBy: p.cfg.CortexActor,
							Data:      []byte(strings.TrimSpace(hint)),
						},
					})
					return string(uri), err
				}
			case RelationContradicts, RelationRelates:
				uri, err := p.cortex.Write(p.head(importance), data, p.writeMeta())
				if err != nil {
					return "", err
				}
				if validCandidateURI(targetURI, cands) {
					_ = p.relate(string(uri), targetURI, rel, hint)
				}
				return string(uri), nil
			}
			// RelationNew (or unrecognized): fall through to a plain write.
		}
	}
	uri, err := p.cortex.Write(p.head(importance), data, p.writeMeta())
	return string(uri), err
}

// validCandidateURI guards against the cheap model inventing a target URI: the
// edge target must be one of the candidates we actually showed it.
func validCandidateURI(uri string, cands []Neighbor) bool {
	for _, c := range cands {
		if c.URI == uri {
			return true
		}
	}
	return false
}
