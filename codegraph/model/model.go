package model

import "sort"

// Kind enumerates the node kinds CodeGraph models. Structural-first: every kind
// is a pure function of source.
type Kind string

const (
	KindRepo      Kind = "repo"
	KindModule    Kind = "module"
	KindPackage   Kind = "package"
	KindFile      Kind = "file"
	KindFunc      Kind = "func"
	KindMethod    Kind = "method"
	KindType      Kind = "type"
	KindInterface Kind = "interface"
	KindConst     Kind = "const"
	KindVar       Kind = "var"
	KindField     Kind = "field"
)

// EdgeType enumerates the fixed relationship set. Reverse lookups are mandatory
// for every type (see Index).
type EdgeType string

const (
	EdgeContains   EdgeType = "contains"
	EdgeImports    EdgeType = "imports"
	EdgeCalls      EdgeType = "calls"
	EdgeImplements EdgeType = "implements"
	EdgeEmbeds     EdgeType = "embeds"
	EdgeReferences EdgeType = "references"
	EdgeDefines    EdgeType = "defines"
	EdgeTests      EdgeType = "tests"
)

// EdgeTypes lists every edge type in a stable order (used for deterministic
// grouping during serialization).
var EdgeTypes = []EdgeType{
	EdgeContains, EdgeImports, EdgeCalls, EdgeImplements,
	EdgeEmbeds, EdgeReferences, EdgeDefines, EdgeTests,
}

// Range is a 1-indexed, inclusive line span within a file.
type Range struct {
	StartLine int
	EndLine   int
}

// ByteRange is an optional precise byte span within a file. The zero value
// means "absent".
type ByteRange struct {
	StartByte int
	EndByte   int
}

func (b ByteRange) IsZero() bool { return b.StartByte == 0 && b.EndByte == 0 }

// Enrichment holds the quarantined, regenerable fields. It has a lifecycle
// entirely separate from a Node's structural fields: writes here never change a
// node id, digest, or any structural field.
type Enrichment struct {
	Summary       string
	SummaryDigest string
	Salience      float64
	EmbedRef      string
}

func (e Enrichment) IsZero() bool {
	return e.Summary == "" && e.SummaryDigest == "" && e.Salience == 0 && e.EmbedRef == ""
}

// Node is one symbol in the graph. Structural fields are a pure function of
// source; the embedded Enrichment is the separate, discardable layer.
type Node struct {
	Id        string
	Kind      Kind
	Name      string
	QName     string
	Lang      string
	File      string
	Range     Range
	ByteRange ByteRange
	Digest    string // "b3:<hex>"
	Sig       string
	Exported  bool
	Doc       string

	Enrich Enrichment
}

// Edge is a directed, typed relationship. (Src, Type, Dst) is unique; Site is an
// optional occurrence location ("file:line") that duplicate occurrences collapse
// onto.
type Edge struct {
	Src  string
	Dst  string
	Type EdgeType
	Site string
}

type edgeKey struct {
	src string
	typ EdgeType
	dst string
}

// Index is the node store plus the mandatory bidirectional edge index. Every
// forward edge is queryable in reverse, so impact analysis and incremental
// invalidation are cheap.
type Index struct {
	nodes map[string]*Node
	fwd   map[string]map[EdgeType][]string // src -> type -> sorted-unique dsts
	rev   map[string]map[EdgeType][]string // dst -> type -> sorted-unique srcs
	sites map[edgeKey]string
}

func NewIndex() *Index {
	return &Index{
		nodes: map[string]*Node{},
		fwd:   map[string]map[EdgeType][]string{},
		rev:   map[string]map[EdgeType][]string{},
		sites: map[edgeKey]string{},
	}
}

// AddNode inserts or replaces a node by id.
func (ix *Index) AddNode(n *Node) { ix.nodes[n.Id] = n }

// Node returns the node with the given id, or nil.
func (ix *Index) Node(id string) *Node { return ix.nodes[id] }

// Len reports the number of nodes.
func (ix *Index) Len() int { return len(ix.nodes) }

// AddEdge records a forward+reverse edge. Duplicate (Src,Type,Dst) triples
// collapse; the first non-empty Site is retained.
func (ix *Index) AddEdge(e Edge) {
	if insertSorted(ix.fwd, e.Src, e.Type, e.Dst) {
		insertSorted(ix.rev, e.Dst, e.Type, e.Src)
	}
	if e.Site != "" {
		k := edgeKey{e.Src, e.Type, e.Dst}
		if _, ok := ix.sites[k]; !ok {
			ix.sites[k] = e.Site
		}
	}
}

// Site returns the retained occurrence site for an edge, if any.
func (ix *Index) Site(src string, typ EdgeType, dst string) string {
	return ix.sites[edgeKey{src, typ, dst}]
}

// Direction selects which adjacency Neighbors traverses.
type Direction int

const (
	Out Direction = iota
	In
	Both
)

// Nodes returns every node sorted by id (bytewise).
func (ix *Index) Nodes() []*Node {
	out := make([]*Node, 0, len(ix.nodes))
	for _, n := range ix.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// Edges returns every forward edge sorted by (Src, Type, Dst) bytewise.
func (ix *Index) Edges() []Edge {
	var out []Edge
	for src, byType := range ix.fwd {
		for typ, dsts := range byType {
			for _, dst := range dsts {
				out = append(out, Edge{Src: src, Dst: dst, Type: typ, Site: ix.sites[edgeKey{src, typ, dst}]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Dst < out[j].Dst
	})
	return out
}

// Forward returns the sorted-unique destinations of id over edge type typ.
func (ix *Index) Forward(id string, typ EdgeType) []string { return lookup(ix.fwd, id, typ) }

// Reverse returns the sorted-unique sources that point at id over edge type typ.
func (ix *Index) Reverse(id string, typ EdgeType) []string { return lookup(ix.rev, id, typ) }

// ForwardTypes returns the edge types present on id's outgoing side, in the
// canonical EdgeTypes order.
func (ix *Index) ForwardTypes(id string) []EdgeType { return typesOf(ix.fwd, id) }

// ReverseTypes returns the edge types present on id's incoming side, in the
// canonical EdgeTypes order.
func (ix *Index) ReverseTypes(id string) []EdgeType { return typesOf(ix.rev, id) }

// Neighbors returns the direct neighbors of id over edge type typ (or all types
// when typ == ""), in the given direction. Results are sorted-unique.
func (ix *Index) Neighbors(id string, typ EdgeType, dir Direction) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(ids []string) {
		for _, x := range ids {
			if _, ok := seen[x]; !ok {
				seen[x] = struct{}{}
				out = append(out, x)
			}
		}
	}
	types := []EdgeType{typ}
	if typ == "" {
		types = EdgeTypes
	}
	for _, t := range types {
		if dir == Out || dir == Both {
			add(lookup(ix.fwd, id, t))
		}
		if dir == In || dir == Both {
			add(lookup(ix.rev, id, t))
		}
	}
	sort.Strings(out)
	return out
}

func insertSorted(m map[string]map[EdgeType][]string, key string, typ EdgeType, val string) bool {
	byType := m[key]
	if byType == nil {
		byType = map[EdgeType][]string{}
		m[key] = byType
	}
	lst := byType[typ]
	i := sort.SearchStrings(lst, val)
	if i < len(lst) && lst[i] == val {
		return false // duplicate
	}
	lst = append(lst, "")
	copy(lst[i+1:], lst[i:])
	lst[i] = val
	byType[typ] = lst
	return true
}

func lookup(m map[string]map[EdgeType][]string, key string, typ EdgeType) []string {
	if byType := m[key]; byType != nil {
		return byType[typ]
	}
	return nil
}

func typesOf(m map[string]map[EdgeType][]string, key string) []EdgeType {
	byType := m[key]
	if byType == nil {
		return nil
	}
	var out []EdgeType
	for _, t := range EdgeTypes {
		if len(byType[t]) > 0 {
			out = append(out, t)
		}
	}
	return out
}
