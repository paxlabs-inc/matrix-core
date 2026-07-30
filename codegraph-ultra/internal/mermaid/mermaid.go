// Package mermaid generates Mermaid markdown diagrams from the code graph.
// It supports dependency graphs, call graphs, inheritance trees, and full
// architecture diagrams, with proper node styling by kind.
package mermaid

import (
	"fmt"
	"html"
	"os"
	"sort"
	"strings"

	"codegraph-ultra/internal/model"
	"codegraph-ultra/internal/store"
)

// DiagramType selects which diagram to generate.
type DiagramType int

const (
	DiagramDependency   DiagramType = iota // imports edges
	DiagramCallGraph                       // calls edges
	DiagramInheritance                     // implements + inherits edges
	DiagramArchitecture                    // all structural edges
)

// Options control diagram generation.
type Options struct {
	Type     DiagramType
	Kinds    []model.Kind      // filter nodes to these kinds (nil = all)
	EdgeType model.EdgeType    // single edge type filter (empty = depends on DiagramType)
	Depth    int               // max traversal depth from root nodes (0 = unlimited)
	Roots    []string          // optional root node IDs to start from (nil = all nodes)
	Title    string            // diagram title
}

// Generate produces a Mermaid markdown string from the store.
func Generate(db *store.DB, opts Options) string {
	edgeTypes := edgeTypesForDiagram(opts.Type, opts.EdgeType)
	nodes := collectNodes(db, opts)
	edges := collectEdges(db, nodes, edgeTypes)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("---\ntitle: %s\n---\n", firstOf(opts.Title, "CodeGraph")))
	b.WriteString(headerForType(opts.Type))
	b.WriteByte('\n')

	// Write nodes with Mermaid-safe IDs and shapes.
	nodeByID := make(map[string]*model.Node, len(nodes))
	for _, n := range nodes {
		nodeByID[n.ID] = n
		b.WriteString(fmt.Sprintf("    %s%s\n", mermaidID(n.ID), mermaidLabel(n)))
	}
	b.WriteByte('\n')

	// Write edges.
	for _, e := range edges {
		if nodeByID[e.Src] == nil || nodeByID[e.Dst] == nil {
			continue
		}
		b.WriteString(fmt.Sprintf("    %s %s %s\n",
			mermaidID(e.Src), arrowForType(opts.Type), mermaidID(e.Dst)))
	}

	// Write classDef styling.
	b.WriteByte('\n')
	b.WriteString(classDefBlock())

	return b.String()
}

// GenerateAndWrite writes a Mermaid diagram to an HTML file with embedded Mermaid.js CDN.
func GenerateAndWrite(db *store.DB, opts Options, path string) error {
	diagram := Generate(db, opts)
	htmlContent := wrapHTML(firstOf(opts.Title, "CodeGraph"), diagram)
	return os.WriteFile(path, []byte(htmlContent), 0o644)
}

// ── internal helpers ────────────────────────────────────────────────

func edgeTypesForDiagram(dt DiagramType, filter model.EdgeType) []model.EdgeType {
	if filter != "" {
		return []model.EdgeType{filter}
	}
	switch dt {
	case DiagramDependency:
		return []model.EdgeType{model.EdgeImports}
	case DiagramCallGraph:
		return []model.EdgeType{model.EdgeCalls}
	case DiagramInheritance:
		return []model.EdgeType{model.EdgeImplements, model.EdgeInherits}
	case DiagramArchitecture:
		return []model.EdgeType{
			model.EdgeImports, model.EdgeCalls,
			model.EdgeImplements, model.EdgeInherits,
			model.EdgeContains, model.EdgeDefines,
		}
	}
	return nil
}

func collectNodes(db *store.DB, opts Options) []*model.Node {
	kindSet := make(map[model.Kind]bool, len(opts.Kinds))
	for _, k := range opts.Kinds {
		kindSet[k] = true
	}

	var nodes []*model.Node
	if len(opts.Roots) > 0 {
		// Start from roots and collect reachable nodes.
		seen := make(map[string]bool)
		frontier := make([]string, len(opts.Roots))
		copy(frontier, opts.Roots)
		for _, id := range frontier {
			seen[id] = true
		}
		depth := opts.Depth
		if depth <= 0 {
			depth = 3
		}
		for d := 0; d < depth && len(frontier) > 0; d++ {
			var next []string
			for _, id := range frontier {
				n := db.GetNode(id)
				if n == nil {
					continue
				}
				if len(kindSet) > 0 && !kindSet[n.Kind] {
					continue
				}
				// Forward and reverse neighbors.
				for _, nb := range append(db.ForwardAll(id), db.ReverseAll(id)...) {
					if !seen[nb] {
						seen[nb] = true
						next = append(next, nb)
					}
				}
			}
			frontier = next
		}
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if n := db.GetNode(id); n != nil {
				if len(kindSet) > 0 && !kindSet[n.Kind] {
					continue
				}
				nodes = append(nodes, n)
			}
		}
	} else {
		if len(opts.Kinds) > 0 {
			for _, k := range opts.Kinds {
				nodes = append(nodes, db.Nodes(k)...)
			}
		} else {
			nodes = db.Nodes("")
		}
	}
	return nodes
}

func collectEdges(db *store.DB, nodes []*model.Node, edgeTypes []model.EdgeType) []model.Edge {
	idSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		idSet[n.ID] = true
	}

	var edges []model.Edge
	for _, et := range edgeTypes {
		for _, e := range db.Edges(et) {
			if idSet[e.Src] && idSet[e.Dst] {
				edges = append(edges, e)
			}
		}
	}
	return edges
}

// mermaidID makes a node ID safe for Mermaid (alphanumeric + underscore).
func mermaidID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) == 0 || (s[0] >= '0' && s[0] <= '9') {
		s = "n_" + s
	}
	return s
}

// mermaidLabel returns the node declaration with shape based on kind.
func mermaidLabel(n *model.Node) string {
	label := html.EscapeString(n.Name)
	if label == "" {
		label = html.EscapeString(n.ID)
	}
	switch n.Kind {
	case model.KindFunc, model.KindMethod:
		// Rounded rectangle for functions/methods.
		return fmt.Sprintf("(%s)", label)
	case model.KindInterface, model.KindTrait:
		// Diamond for interfaces/traits.
		return fmt.Sprintf("{%s}", label)
	case model.KindClass, model.KindStruct, model.KindType, model.KindEnum:
		// Rectangle with double border.
		return fmt.Sprintf("[[%s]]", label)
	case model.KindFile:
		// Cylindrical (database-like) for files.
		return fmt.Sprintf("[(%s)]", label)
	case model.KindPackage, model.KindModule, model.KindRepo:
		// Hexagon for packages/modules/repos.
		return fmt.Sprintf("{{%s}}", label)
	case model.KindConst, model.KindVar, model.KindField:
		// Circle for constants/vars/fields.
		return fmt.Sprintf("((%s))", label)
	default:
		return fmt.Sprintf("[%s]", label)
	}
}

func headerForType(dt DiagramType) string {
	switch dt {
	case DiagramDependency:
		return "graph LR"
	case DiagramCallGraph:
		return "graph LR"
	case DiagramInheritance:
		return "graph TD"
	case DiagramArchitecture:
		return "graph LR"
	}
	return "graph LR"
}

func arrowForType(dt DiagramType) string {
	switch dt {
	case DiagramInheritance:
		return "-->"
	default:
		return "-->"
	}
}

// classDefBlock returns Mermaid classDef statements for node styling.
func classDefBlock() string {
	return `classDef repo fill:#1a1a2e,stroke:#e94560,color:#fff
classDef module fill:#16213e,stroke:#0f3460,color:#fff
classDef package fill:#1a1a2e,stroke:#533483,color:#fff
classDef file fill:#0f3460,stroke:#e94560,color:#fff
classDef func fill:#2d4059,stroke:#ea5455,color:#fff
classDef method fill:#2d4059,stroke:#f07b3f,color:#fff
classDef type fill:#222831,stroke:#00adb5,color:#fff
classDef interface fill:#222831,stroke:#32e0c4,color:#000
classDef const fill:#393e46,stroke:#eeeeee,color:#fff
classDef var fill:#393e46,stroke:#d4d4d4,color:#fff
classDef field fill:#393e46,stroke:#a8a8a8,color:#fff
classDef class fill:#1b262c,stroke:#bbe1fa,color:#000
classDef enum fill:#1b262c,stroke:#3282b8,color:#fff
classDef trait fill:#2c3333,stroke:#a6e22e,color:#000
classDef struct fill:#2c3333,stroke:#66d9ef,color:#000`
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── HTML wrapper ────────────────────────────────────────────────────

func wrapHTML(title, diagram string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s</title>
<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
<style>
  body {
    margin: 0; padding: 20px;
    background: #0d1117; color: #c9d1d9;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
  }
  h1 { color: #58a6ff; font-size: 1.4em; }
  .mermaid {
    background: #161b22; border-radius: 8px;
    padding: 20px; overflow-x: auto;
  }
</style>
</head>
<body>
<h1>%s</h1>
<pre class="mermaid">
%s
</pre>
<script>
  mermaid.initialize({
    startOnLoad: true,
    theme: 'dark',
    themeVariables: {
      darkMode: true,
      background: '#161b22',
      primaryColor: '#1f6feb',
      primaryTextColor: '#c9d1d9',
      lineColor: '#30363d'
    }
  });
</script>
</body>
</html>`, html.EscapeString(title), html.EscapeString(title), diagram)
}
