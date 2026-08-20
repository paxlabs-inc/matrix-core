package model

// Index is an in-memory graph for fast traversal without SQLite round-trips.
type Index struct {
	Nodes    map[string]*Node
	Forward  map[string]map[EdgeType][]string // src -> type -> [dst]
	Reverse  map[string]map[EdgeType][]string // dst -> type -> [src]
}

// NewIndex creates an empty in-memory graph index.
func NewIndex() *Index {
	return &Index{
		Nodes:   make(map[string]*Node),
		Forward: make(map[string]map[EdgeType][]string),
		Reverse: make(map[string]map[EdgeType][]string),
	}
}

// AddNode inserts a node into the index.
func (ix *Index) AddNode(n *Node) {
	ix.Nodes[n.ID] = n
}

// AddEdge inserts an edge into the forward and reverse adjacency lists.
func (ix *Index) AddEdge(e Edge) {
	if ix.Forward[e.Src] == nil {
		ix.Forward[e.Src] = make(map[EdgeType][]string)
	}
	ix.Forward[e.Src][e.Type] = append(ix.Forward[e.Src][e.Type], e.Dst)

	if ix.Reverse[e.Dst] == nil {
		ix.Reverse[e.Dst] = make(map[EdgeType][]string)
	}
	ix.Reverse[e.Dst][e.Type] = append(ix.Reverse[e.Dst][e.Type], e.Src)
}

// GetNode returns a node by ID, or nil.
func (ix *Index) GetNode(id string) *Node {
	return ix.Nodes[id]
}

// ForwardNodes returns destination nodes for a source and edge type.
func (ix *Index) ForwardNodes(src string, typ EdgeType) []*Node {
	ids := ix.Forward[src][typ]
	out := make([]*Node, 0, len(ids))
	for _, id := range ids {
		if n := ix.Nodes[id]; n != nil {
			out = append(out, n)
		}
	}
	return out
}

// ReverseNodes returns source nodes for a destination and edge type.
func (ix *Index) ReverseNodes(dst string, typ EdgeType) []*Node {
	ids := ix.Reverse[dst][typ]
	out := make([]*Node, 0, len(ids))
	for _, id := range ids {
		if n := ix.Nodes[id]; n != nil {
			out = append(out, n)
		}
	}
	return out
}
