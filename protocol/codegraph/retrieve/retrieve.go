// Package retrieve is the L4 retrieval surface. Every tool returns a compact
// .kvx fragment of node ids, locations, and one-line summaries — never raw
// source — so an agent locates and expands structure without pulling files back
// into context.
package retrieve

import (
	"fmt"
	"sort"
	"strings"

	"centra/protocol/codegraph/model"
)

// MaxDepth bounds neighbors traversal.
const MaxDepth = 3

// sigCap caps the rendered signature length so a fragment stays compact and
// never approximates a full source range.
const sigCap = 160

// API answers retrieval queries over an in-memory graph Index.
type API struct{ ix *model.Index }

// New wraps an Index in the retrieval API.
func New(ix *model.Index) *API { return &API{ix: ix} }

// SymbolLookup returns the NODE fragment(s) whose name matches: exact matches
// first, and only if there are none, a fuzzy (substring) fallback. kind, when
// non-empty, filters by node kind.
func (a *API) SymbolLookup(name string, kind model.Kind) string {
	var exact, fuzzy []*model.Node
	for _, n := range a.ix.Nodes() {
		if kind != "" && n.Kind != kind {
			continue
		}
		switch {
		case n.Name == name:
			exact = append(exact, n)
		case strings.Contains(n.Name, name) || strings.Contains(n.Id, name):
			fuzzy = append(fuzzy, n)
		}
	}
	res := exact
	if len(res) == 0 {
		res = fuzzy
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=symbol_lookup name=%s matches=%d\n", name, len(res))
	for _, n := range res {
		renderNode(&b, n)
	}
	return b.String()
}

// Neighbors returns a bounded subgraph around id over edge type edge (or all
// types when edge == "") in the given direction, to at most MaxDepth. Depth
// beyond MaxDepth is rejected rather than expanded.
func (a *API) Neighbors(id string, edge model.EdgeType, dir model.Direction, depth int) (string, error) {
	if depth > MaxDepth {
		return "", fmt.Errorf("neighbors: depth %d exceeds max %d", depth, MaxDepth)
	}
	if depth < 1 {
		depth = 1
	}
	visited := map[string]bool{id: true}
	frontier := []string{id}
	for d := 0; d < depth; d++ {
		var next []string
		for _, cur := range frontier {
			for _, nb := range a.ix.Neighbors(cur, edge, dir) {
				if !visited[nb] {
					visited[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}

	ids := make([]string, 0, len(visited))
	for v := range visited {
		ids = append(ids, v)
	}
	sort.Strings(ids)
	set := make(map[string]bool, len(ids))
	for _, v := range ids {
		set[v] = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=neighbors root=%s depth=%d nodes=%d\n", id, depth, len(ids))
	for _, nid := range ids {
		n := a.ix.Node(nid)
		if n == nil {
			fmt.Fprintf(&b, "NODE id=%s kind=unresolved\n", nid)
			continue
		}
		renderNode(&b, n)
		renderEdges(&b, a.ix, nid, edge, dir, set)
	}
	return b.String(), nil
}

// ImpactEdges are the edge types whose reverse closure defines blast radius:
// changing a node affects everything that calls it, references it, or (for an
// interface) implements it.
var ImpactEdges = []model.EdgeType{model.EdgeCalls, model.EdgeReferences, model.EdgeImplements}

// MaxImpactDepth bounds the transitive closure so a hub node cannot expand the
// walk unboundedly.
const MaxImpactDepth = 64

// Impact returns the affected NODE set for id: the reverse-edge transitive
// closure over calls, references, and implements. The root id is excluded (it
// is what changed; the fragment lists what it would break). maxDepth <= 0 or
// beyond MaxImpactDepth clamps to MaxImpactDepth. The result is a compact .kvx
// fragment of ids/locations/summaries — never raw source.
func (a *API) Impact(id string, maxDepth int) string {
	if maxDepth <= 0 || maxDepth > MaxImpactDepth {
		maxDepth = MaxImpactDepth
	}
	seen := map[string]bool{id: true}
	affected := map[string]int{} // id -> depth first reached
	frontier := []string{id}
	for d := 1; d <= maxDepth && len(frontier) > 0; d++ {
		var next []string
		for _, cur := range frontier {
			for _, t := range ImpactEdges {
				for _, src := range a.ix.Reverse(cur, t) {
					if !seen[src] {
						seen[src] = true
						affected[src] = d
						next = append(next, src)
					}
				}
			}
		}
		frontier = next
	}

	ids := make([]string, 0, len(affected))
	for x := range affected {
		ids = append(ids, x)
	}
	sort.Strings(ids)

	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=impact root=%s affected=%d max_depth=%d\n", id, len(ids), maxDepth)
	for _, nid := range ids {
		n := a.ix.Node(nid)
		if n == nil {
			fmt.Fprintf(&b, "NODE id=%s kind=unresolved depth=%d\n", nid, affected[nid])
			continue
		}
		renderNode(&b, n)
		fmt.Fprintf(&b, "  depth=%d\n", affected[nid])
	}
	return b.String()
}

func renderNode(b *strings.Builder, n *model.Node) {
	fmt.Fprintf(b, "NODE id=%s kind=%s loc=%s:%s\n", n.Id, n.Kind, n.File, n.Range.String())
	if n.Sig != "" {
		fmt.Fprintf(b, "  sig=%s\n", oneLine(n.Sig, sigCap))
	}
	if n.Enrich.Summary != "" {
		fmt.Fprintf(b, "  summary=%s\n", oneLine(n.Enrich.Summary, 0))
	}
}

func renderEdges(b *strings.Builder, ix *model.Index, id string, edge model.EdgeType, dir model.Direction, set map[string]bool) {
	types := []model.EdgeType{edge}
	if edge == "" {
		types = model.EdgeTypes
	}
	for _, t := range types {
		if dir == model.Out || dir == model.Both {
			if dsts := within(ix.Forward(id, t), set); len(dsts) > 0 {
				fmt.Fprintf(b, "  %s=%s\n", t, strings.Join(dsts, ","))
			}
		}
		if dir == model.In || dir == model.Both {
			if srcs := within(ix.Reverse(id, t), set); len(srcs) > 0 {
				fmt.Fprintf(b, "  ^%s=%s\n", t, strings.Join(srcs, ","))
			}
		}
	}
}

func within(ids []string, set map[string]bool) []string {
	var out []string
	for _, x := range ids {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}

func oneLine(s string, cap int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if cap > 0 && len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}
