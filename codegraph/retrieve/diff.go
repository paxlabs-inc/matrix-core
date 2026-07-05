package retrieve

import (
	"fmt"
	"sort"
	"strings"

	"matrix/codegraph/model"
)

// nodeDelta accumulates the change to one node id across both the node set and
// the edge set, so a single NODE record in the fragment carries everything that
// moved for that id.
type nodeDelta struct {
	kind     model.Kind
	status   string // "added" | "removed" | "changed" | "" (edges only)
	digestA  string
	digestB  string
	addEdges []string // "type=dst" present in b, absent in a
	remEdges []string // "type=dst" present in a, absent in b
}

type edgeTriple struct {
	src string
	typ model.EdgeType
	dst string
}

// Diff compares two graphs by node digest and edge set and returns a compact,
// guard-conformant .kvx fragment: added/removed/changed node ids (by digest) and
// per-source edge deltas. It contains only ids, kinds, digests, and edge triples
// — never raw source. a is the base ("rev_a"), b is the new revision ("rev_b").
func Diff(a, b *model.Index) string {
	deltas := map[string]*nodeDelta{}
	kindOf := func(id string) model.Kind {
		if n := b.Node(id); n != nil {
			return n.Kind
		}
		if n := a.Node(id); n != nil {
			return n.Kind
		}
		return ""
	}
	ensure := func(id string) *nodeDelta {
		d := deltas[id]
		if d == nil {
			d = &nodeDelta{kind: kindOf(id)}
			deltas[id] = d
		}
		return d
	}

	for _, n := range b.Nodes() {
		old := a.Node(n.Id)
		switch {
		case old == nil:
			d := ensure(n.Id)
			d.status = "added"
			d.digestB = n.Digest
		case old.Digest != n.Digest:
			d := ensure(n.Id)
			d.status = "changed"
			d.digestA = old.Digest
			d.digestB = n.Digest
		}
	}
	for _, n := range a.Nodes() {
		if b.Node(n.Id) == nil {
			d := ensure(n.Id)
			d.status = "removed"
			d.digestA = n.Digest
		}
	}

	aEdges := edgeSet(a)
	bEdges := edgeSet(b)
	var edgesAdded, edgesRemoved int
	for t := range bEdges {
		if !aEdges[t] {
			d := ensure(t.src)
			d.addEdges = append(d.addEdges, fmt.Sprintf("%s=%s", t.typ, t.dst))
			edgesAdded++
		}
	}
	for t := range aEdges {
		if !bEdges[t] {
			d := ensure(t.src)
			d.remEdges = append(d.remEdges, fmt.Sprintf("%s=%s", t.typ, t.dst))
			edgesRemoved++
		}
	}

	var nAdded, nRemoved, nChanged int
	for _, d := range deltas {
		switch d.status {
		case "added":
			nAdded++
		case "removed":
			nRemoved++
		case "changed":
			nChanged++
		}
	}

	ids := make([]string, 0, len(deltas))
	for id := range deltas {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var sb strings.Builder
	fmt.Fprintf(&sb, "# FRAGMENT tool=diff nodes_added=%d nodes_removed=%d nodes_changed=%d edges_added=%d edges_removed=%d\n",
		nAdded, nRemoved, nChanged, edgesAdded, edgesRemoved)
	for _, id := range ids {
		d := deltas[id]
		kind := d.kind
		if kind == "" {
			kind = "unresolved"
		}
		fmt.Fprintf(&sb, "NODE id=%s kind=%s\n", id, kind)
		if d.status != "" {
			fmt.Fprintf(&sb, "  change=%s\n", d.status)
		}
		if d.status == "changed" {
			fmt.Fprintf(&sb, "  digest_old=%s\n", d.digestA)
			fmt.Fprintf(&sb, "  digest_new=%s\n", d.digestB)
		}
		sort.Strings(d.addEdges)
		sort.Strings(d.remEdges)
		for _, e := range d.addEdges {
			fmt.Fprintf(&sb, "  +%s\n", e)
		}
		for _, e := range d.remEdges {
			fmt.Fprintf(&sb, "  -%s\n", e)
		}
	}
	return sb.String()
}

func edgeSet(ix *model.Index) map[edgeTriple]bool {
	out := map[edgeTriple]bool{}
	for _, e := range ix.Edges() {
		out[edgeTriple{e.Src, e.Type, e.Dst}] = true
	}
	return out
}
