// Package retrieve provides the query API over the graph store.
// Every tool returns compact .kvx fragments — never raw source.
package retrieve

import (
	"fmt"
	"sort"
	"strings"

	"centra/protocol/codegraph-ultra/internal/model"
	"centra/protocol/codegraph-ultra/internal/store"
)

const sigCap = 160

// API answers retrieval queries over a graph store.
type API struct {
	db *store.DB
}

// New wraps a DB in the retrieval API.
func New(db *store.DB) *API { return &API{db: db} }

// SymbolLookup returns NODE fragments matching a name: exact first, then fuzzy.
func (a *API) SymbolLookup(name string, kind model.Kind) string {
	var exact, fuzzy []*model.Node
	for _, n := range a.db.Nodes("") {
		if kind != "" && n.Kind != kind {
			continue
		}
		switch {
		case n.Name == name:
			exact = append(exact, n)
		case strings.Contains(strings.ToLower(n.Name), strings.ToLower(name)) ||
			strings.Contains(strings.ToLower(n.ID), strings.ToLower(name)):
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

// Search performs full-text search and returns matching NODE fragments.
func (a *API) Search(query string, limit int) string {
	nodes := a.db.Search(query, limit)
	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=search query=%s matches=%d\n", query, len(nodes))
	for _, n := range nodes {
		renderNode(&b, n)
	}
	return b.String()
}

// Neighbors returns the bounded subgraph around a node.
func (a *API) Neighbors(id string, edge model.EdgeType, depth int) string {
	if depth <= 0 {
		depth = 1
	}
	nodes := a.db.Neighbors(id, edge, depth)
	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=neighbors root=%s depth=%d nodes=%d\n", id, depth, len(nodes))
	for _, n := range nodes {
		renderNode(&b, n)
		renderNodeEdges(&b, a.db, n.ID, edge)
	}
	return b.String()
}

// Impact returns the reverse transitive closure of callers/references/implementors.
func (a *API) Impact(id string, maxDepth int) string {
	nodes := a.db.Impact(id, maxDepth)
	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=impact root=%s affected=%d\n", id, len(nodes))
	for _, n := range nodes {
		renderNode(&b, n)
	}
	return b.String()
}

// Stats returns a formatted summary of the graph.
func (a *API) Stats() string {
	s := a.db.Stats()
	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=stats\n")
	fmt.Fprintf(&b, "nodes=%d edges=%d files=%d\n", s.TotalNodes, s.TotalEdges, s.FilesCount)
	if len(s.Languages) > 0 {
		fmt.Fprintf(&b, "languages=%s\n", strings.Join(s.Languages, ","))
	}
	fmt.Fprintf(&b, "## Nodes by kind\n")
	for _, k := range sortedKinds(s.NodesByKind) {
		fmt.Fprintf(&b, "  %s=%d\n", k, s.NodesByKind[k])
	}
	fmt.Fprintf(&b, "## Edges by type\n")
	for _, t := range sortedEdgeTypes(s.EdgesByType) {
		fmt.Fprintf(&b, "  %s=%d\n", t, s.EdgesByType[t])
	}
	return b.String()
}

// Subgraph returns a filtered view of the graph around a set of nodes.
func (a *API) Subgraph(ids []string, edgeType model.EdgeType, depth int) string {
	var allNodes []*model.Node
	seen := map[string]bool{}
	for _, id := range ids {
		for _, n := range a.db.Neighbors(id, edgeType, depth) {
			if !seen[n.ID] {
				seen[n.ID] = true
				allNodes = append(allNodes, n)
			}
		}
	}
	model.SortNodes(allNodes)
	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=subgraph roots=%d nodes=%d\n", len(ids), len(allNodes))
	for _, n := range allNodes {
		renderNode(&b, n)
		renderNodeEdges(&b, a.db, n.ID, edgeType)
	}
	return b.String()
}

// --- render helpers ---

func renderNode(b *strings.Builder, n *model.Node) {
	fmt.Fprintf(b, "NODE id=%s kind=%s loc=%s:%s\n", n.ID, n.Kind, n.File, n.Range.String())
	if n.Sig != "" {
		fmt.Fprintf(b, "  sig=%s\n", oneLine(n.Sig, sigCap))
	}
	if n.Doc != "" {
		fmt.Fprintf(b, "  doc=%s\n", oneLine(n.Doc, 120))
	}
	if n.Enrich.Summary != "" {
		fmt.Fprintf(b, "  summary=%s\n", oneLine(n.Enrich.Summary, 0))
	}
}

func renderNodeEdges(b *strings.Builder, db *store.DB, id string, filter model.EdgeType) {
	etypes := []model.EdgeType{filter}
	if filter == "" {
		etypes = model.EdgeTypes
	}
	for _, t := range etypes {
		if dsts := db.Forward(id, t); len(dsts) > 0 {
			fmt.Fprintf(b, "  %s=%s\n", t, strings.Join(dsts, ","))
		}
		if srcs := db.Reverse(id, t); len(srcs) > 0 {
			fmt.Fprintf(b, "  ^%s=%s\n", t, strings.Join(srcs, ","))
		}
	}
}

func oneLine(s string, cap int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if cap > 0 && len(s) > cap {
		s = s[:cap] + "..."
	}
	return s
}

func sortedKinds(m map[model.Kind]int) []model.Kind {
	out := make([]model.Kind, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedEdgeTypes(m map[model.EdgeType]int) []model.EdgeType {
	out := make([]model.EdgeType, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
