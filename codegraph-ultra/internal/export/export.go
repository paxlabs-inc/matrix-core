package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"codegraph-ultra/internal/model"
	"codegraph-ultra/internal/store"
)

// titleCase capitalizes the first letter of a string.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// ExportJSON writes the full graph as a JSON file.
func ExportJSON(db *store.DB, path string) error {
	data, err := db.ExportJSON()
	if err != nil {
		return fmt.Errorf("export json: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// ExportKVX writes the graph as a key-value-indexed text format.
// Each node is a block: key = ID, fields are tab-separated.
func ExportKVX(db *store.DB, path string) error {
	stats := db.Stats()
	var b strings.Builder

	b.WriteString("# codegraph-ultra kvx export\n")
	b.WriteString(fmt.Sprintf("# nodes=%d edges=%d\n\n", stats.TotalNodes, stats.TotalEdges))

 kinds := sortedKinds(stats.NodesByKind)
	for _, k := range kinds {
		nodes := db.Nodes(model.Kind(k))
		if len(nodes) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("## %s\n", k))
		for _, n := range nodes {
			exported := "0"
			if n.Exported {
				exported = "1"
			}
			b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				n.ID, n.Name, n.QName, n.Lang, n.File, exported, n.Sig))
		}
		b.WriteString("\n")
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ExportMermaid writes the graph as a Mermaid diagram.
// diagramType: "graph" (default), "flowchart", "sequence"
func ExportMermaid(db *store.DB, path string, diagramType string) error {
	if diagramType == "" {
		diagramType = "graph"
	}

	stats := db.Stats()
	var b strings.Builder

	switch diagramType {
	case "flowchart":
		b.WriteString("flowchart TD\n")
	case "sequence":
		b.WriteString("sequenceDiagram\n")
	default:
		b.WriteString("graph LR\n")
	}

 kinds := sortedKinds(stats.NodesByKind)

	nodeIDs := make(map[string]string) // node ID -> mermaid-safe ID
	counter := 0

	for _, kind := range kinds {
		if kind == "file" {
			continue // skip files in diagrams
		}
		nodes := db.Nodes(model.Kind(kind))
		if len(nodes) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("  subgraph kind_%s [%s]\n", kind, titleCase(kind)+"s"))
		for _, n := range nodes {
			safeID := fmt.Sprintf("n%d", counter)
			counter++
			nodeIDs[n.ID] = safeID
			label := n.Name
			if len(label) > 30 {
				label = label[:27] + "..."
			}
			switch model.Kind(kind) {
			case model.KindFunc, model.KindMethod:
				b.WriteString(fmt.Sprintf("    %s[\"%s()\"]\n", safeID, label))
			case model.KindClass, model.KindStruct, model.KindInterface:
				b.WriteString(fmt.Sprintf("    %s{\"%s\"}\n", safeID, label))
			case model.KindPackage, model.KindModule:
				b.WriteString(fmt.Sprintf("    %s(\"%s\")\n", safeID, label))
			default:
				b.WriteString(fmt.Sprintf("    %s[\"%s\"]\n", safeID, label))
			}
		}
		b.WriteString("  end\n\n")
	}

	// Write edges
	edgeTypes := []model.EdgeType{
		model.EdgeCalls,
		model.EdgeImports,
		model.EdgeReferences,
		model.EdgeContains,
		model.EdgeImplements,
	}
	for _, etype := range edgeTypes {
		for id, mermaidID := range nodeIDs {
			targetIDs := db.Forward(id, etype)
			for _, tid := range targetIDs {
				targetMermaid, ok := nodeIDs[tid]
				if !ok {
					continue
				}
				switch etype {
				case model.EdgeCalls:
					b.WriteString(fmt.Sprintf("  %s --> %s\n", mermaidID, targetMermaid))
				case model.EdgeImports:
					b.WriteString(fmt.Sprintf("  %s -.-> %s\n", mermaidID, targetMermaid))
				case model.EdgeImplements:
					b.WriteString(fmt.Sprintf("  %s ==> %s\n", mermaidID, targetMermaid))
				case model.EdgeContains:
					b.WriteString(fmt.Sprintf("  %s -- %s\n", mermaidID, targetMermaid))
				default:
					b.WriteString(fmt.Sprintf("  %s --> %s\n", mermaidID, targetMermaid))
				}
			}
		}
	}

	// Style directives
	b.WriteString("\n")
	b.WriteString("  classDef default fill:#1a1a18,stroke:#96918e,color:#e3d9d4\n")
	b.WriteString("  classDef highlight fill:#1a1a18,stroke:#fe9a00,color:#fe9a00\n")

	if path == "" {
		fmt.Print(b.String())
		return nil
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ExportMCPServer generates a standalone MCP server config JSON file
// that can be used to serve the code graph over MCP protocol.
func ExportMCPServer(db *store.DB, path string) error {
	stats := db.Stats()

	config := map[string]interface{}{
		"name":    "codegraph-ultra-mcp",
		"version": "1.0.0",
		"server": map[string]interface{}{
			"command": "cg",
			"args":    []string{"mcp", "serve"},
			"env": map[string]string{
				"CG_DB_PATH": "auto",
			},
		},
		"capabilities": map[string]interface{}{
			"tools": []map[string]interface{}{
				{
					"name":        "search_symbols",
					"description": "Search for code symbols by name, type, or pattern",
					"parameters": map[string]interface{}{
						"query": map[string]string{
							"type":        "string",
							"description": "Search query (name, qualified name, or regex)",
						},
						"limit": map[string]interface{}{
							"type":    "integer",
							"default": 20,
						},
					},
				},
				{
					"name":        "get_node",
					"description": "Get detailed information about a specific code node",
					"parameters": map[string]interface{}{
						"node_id": map[string]string{
							"type":        "string",
							"description": "The node ID",
						},
					},
				},
				{
					"name":        "get_neighbors",
					"description": "Get neighbors of a node (callers, callees, imports, etc.)",
					"parameters": map[string]interface{}{
						"node_id": map[string]string{
							"type":        "string",
							"description": "The node ID",
						},
						"edge_type": map[string]string{
							"type":        "string",
							"description": "Edge type filter: calls, imports, references, contains",
						},
						"depth": map[string]interface{}{
							"type":    "integer",
							"default": 1,
						},
					},
				},
				{
					"name":        "impact_analysis",
					"description": "Analyze the impact radius of a node change",
					"parameters": map[string]interface{}{
						"node_id": map[string]string{
							"type":        "string",
							"description": "The node ID to analyze",
						},
						"max_depth": map[string]interface{}{
							"type":    "integer",
							"default": 3,
						},
					},
				},
				{
					"name":        "graph_stats",
					"description": "Get overall graph statistics",
					"parameters":  map[string]interface{}{},
				},
			},
			"resources": []map[string]interface{}{
				{
					"uri":         "codegraph://stats",
					"name":        "Graph Statistics",
					"description": "Overview of the code graph",
					"mimeType":    "application/json",
				},
			},
		},
		"metadata": map[string]interface{}{
			"total_nodes":    stats.TotalNodes,
			"total_edges":    stats.TotalEdges,
			"total_files":    stats.FilesCount,
			"languages":      stats.Languages,
			"nodes_by_kind":  stats.NodesByKind,
			"edges_by_type":  stats.EdgesByType,
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	return os.WriteFile(path, data, 0o644)
}

func sortedKinds(m map[model.Kind]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}
