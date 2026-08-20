// Package diff provides graph diffing between two CodeGraph builds.
// It compares node digests and edge sets to produce a structured delta.
package diff

import (
	"fmt"
	"sort"
	"strings"

	"centra/protocol/codegraph-ultra/internal/model"
)

// Delta represents the difference between two graph builds.
type Delta struct {
	AddedNodes    []*model.Node
	RemovedNodes  []*model.Node
	ChangedNodes  []NodeChange
	AddedEdges    []model.Edge
	RemovedEdges  []model.Edge
	Stats         DeltaStats
}

// NodeChange describes how a node changed between builds.
type NodeChange struct {
	ID      string
	Old     *model.Node
	New     *model.Node
	Fields  []string // changed field names
}

// DeltaStats provides aggregate counts.
type DeltaStats struct {
	NodesAdded    int
	NodesRemoved  int
	NodesChanged  int
	EdgesAdded    int
	EdgesRemoved  int
}

// Compare performs a structural diff between two in-memory graph indices.
func Compare(oldIX, newIX *model.Index) *Delta {
	d := &Delta{}

	// Build maps for edge lookup.
	oldEdges := edgeSet(oldIX)
	newEdges := edgeSet(newIX)

	// Find added/removed/changed nodes.
	for id, newNode := range newIX.Nodes {
		oldNode := oldIX.Nodes[id]
		if oldNode == nil {
			d.AddedNodes = append(d.AddedNodes, newNode)
		} else if oldNode.Digest != newNode.Digest {
			changed := NodeChange{
				ID:  id,
				Old: oldNode,
				New: newNode,
			}
			if oldNode.Sig != newNode.Sig {
				changed.Fields = append(changed.Fields, "sig")
			}
			if oldNode.Doc != newNode.Doc {
				changed.Fields = append(changed.Fields, "doc")
			}
			if oldNode.File != newNode.File || oldNode.Range != newNode.Range {
				changed.Fields = append(changed.Fields, "location")
			}
			if oldNode.Exported != newNode.Exported {
				changed.Fields = append(changed.Fields, "exported")
			}
			d.ChangedNodes = append(d.ChangedNodes, changed)
		}
	}
	for id, oldNode := range oldIX.Nodes {
		if newIX.Nodes[id] == nil {
			d.RemovedNodes = append(d.RemovedNodes, oldNode)
		}
	}

	// Find added/removed edges.
	for key := range newEdges {
		if !oldEdges[key] {
			parts := parseEdgeKey(key)
			d.AddedEdges = append(d.AddedEdges, parts)
		}
	}
	for key := range oldEdges {
		if !newEdges[key] {
			parts := parseEdgeKey(key)
			d.RemovedEdges = append(d.RemovedEdges, parts)
		}
	}

	// Sort for deterministic output.
	sort.Slice(d.AddedNodes, func(i, j int) bool { return d.AddedNodes[i].ID < d.AddedNodes[j].ID })
	sort.Slice(d.RemovedNodes, func(i, j int) bool { return d.RemovedNodes[i].ID < d.RemovedNodes[j].ID })
	sort.Slice(d.ChangedNodes, func(i, j int) bool { return d.ChangedNodes[i].ID < d.ChangedNodes[j].ID })
	sortEdges(d.AddedEdges)
	sortEdges(d.RemovedEdges)

	d.Stats = DeltaStats{
		NodesAdded:    len(d.AddedNodes),
		NodesRemoved:  len(d.RemovedNodes),
		NodesChanged:  len(d.ChangedNodes),
		EdgesAdded:    len(d.AddedEdges),
		EdgesRemoved:  len(d.RemovedEdges),
	}

	return d
}

// FormatKVX renders the delta as a .kvx fragment.
func (d *Delta) FormatKVX() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# FRAGMENT tool=graph_diff\n")
	fmt.Fprintf(&b, "DELTA nodes_added=%d nodes_removed=%d nodes_changed=%d edges_added=%d edges_removed=%d\n",
		d.Stats.NodesAdded, d.Stats.NodesRemoved, d.Stats.NodesChanged,
		d.Stats.EdgesAdded, d.Stats.EdgesRemoved)

	if len(d.AddedNodes) > 0 {
		b.WriteString("\n# Added nodes:\n")
		for _, n := range d.AddedNodes {
			fmt.Fprintf(&b, "+NODE id=%s kind=%s\n", n.ID, n.Kind)
		}
	}
	if len(d.RemovedNodes) > 0 {
		b.WriteString("\n# Removed nodes:\n")
		for _, n := range d.RemovedNodes {
			fmt.Fprintf(&b, "-NODE id=%s kind=%s\n", n.ID, n.Kind)
		}
	}
	if len(d.ChangedNodes) > 0 {
		b.WriteString("\n# Changed nodes:\n")
		for _, c := range d.ChangedNodes {
			fmt.Fprintf(&b, "~NODE id=%s fields=%s\n", c.ID, strings.Join(c.Fields, ","))
		}
	}
	if len(d.AddedEdges) > 0 {
		b.WriteString("\n# Added edges:\n")
		for _, e := range d.AddedEdges {
			fmt.Fprintf(&b, "+EDGE src=%s dst=%s type=%s\n", e.Src, e.Dst, e.Type)
		}
	}
	if len(d.RemovedEdges) > 0 {
		b.WriteString("\n# Removed edges:\n")
		for _, e := range d.RemovedEdges {
			fmt.Fprintf(&b, "-EDGE src=%s dst=%s type=%s\n", e.Src, e.Dst, e.Type)
		}
	}

	return b.String()
}

func edgeSet(ix *model.Index) map[string]bool {
	edges := make(map[string]bool)
	for src, types := range ix.Forward {
		for typ, dsts := range types {
			for _, dst := range dsts {
				edges[string(typ)+":"+src+":"+dst] = true
			}
		}
	}
	return edges
}

func parseEdgeKey(key string) model.Edge {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 {
		return model.Edge{}
	}
	return model.Edge{Type: model.EdgeType(parts[0]), Src: parts[1], Dst: parts[2]}
}

func sortEdges(edges []model.Edge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Src != edges[j].Src {
			return edges[i].Src < edges[j].Src
		}
		if edges[i].Type != edges[j].Type {
			return edges[i].Type < edges[j].Type
		}
		return edges[i].Dst < edges[j].Dst
	})
}
