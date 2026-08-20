package specgen

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"centra/protocol/codegraph-ultra/internal/model"
	"centra/protocol/codegraph-ultra/internal/store"
)

// titleCase capitalizes the first letter of a string (replaces deprecated strings.Title).
func titleCase(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// GenerateAgentSpec produces an AGENTS.md containing a full symbol index,
// package map, and architecture overview for the given repository.
func GenerateAgentSpec(db *store.DB, repoName string) string {
	stats := db.Stats()
	var b strings.Builder

	// header
	b.WriteString(fmt.Sprintf("# %s — Code Graph Agent Spec\n\n", repoName))
	b.WriteString("Auto-generated from the code graph. Do not edit manually.\n\n")

	// overview
	b.WriteString("## Overview\n\n")
	b.WriteString(fmt.Sprintf("- **Nodes:** %d\n", stats.TotalNodes))
	b.WriteString(fmt.Sprintf("- **Edges:** %d\n", stats.TotalEdges))
	b.WriteString(fmt.Sprintf("- **Files:** %d\n", stats.FilesCount))
	if len(stats.Languages) > 0 {
		b.WriteString(fmt.Sprintf("- **Languages:** %s\n", strings.Join(stats.Languages, ", ")))
	}
	b.WriteString("\n")

	// node breakdown
	b.WriteString("## Node Breakdown\n\n")
	b.WriteString("| Kind | Count |\n")
	b.WriteString("|------|-------|\n")
 kinds := sortedKinds(stats.NodesByKind)
	for _, k := range kinds {
		b.WriteString(fmt.Sprintf("| %s | %d |\n", k, stats.NodesByKind[model.Kind(k)]))
	}
	b.WriteString("\n")

	// symbol index
	b.WriteString("## Symbol Index\n\n")
	for _, k := range kinds {
		if k == "file" || k == "package" {
			continue
		}
		nodes := db.Nodes(model.Kind(k))
		if len(nodes) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("### %s\n\n", titleCase(k)+"s"))
		b.WriteString("| Name | File | Exported |\n")
		b.WriteString("|------|------|----------|\n")
		for _, n := range nodes {
			exported := ""
			if n.Exported {
				exported = "yes"
			}
			b.WriteString(fmt.Sprintf("| `%s` | %s | %s |\n", n.QName, n.File, exported))
		}
		b.WriteString("\n")
	}

	// package map
	pkgNodes := db.Nodes("package")
	if len(pkgNodes) > 0 {
		b.WriteString("## Package Map\n\n")
		for _, pkg := range pkgNodes {
			b.WriteString(fmt.Sprintf("### %s\n\n", pkg.Name))
			if pkg.Doc != "" {
				b.WriteString(fmt.Sprintf("%s\n\n", pkg.Doc))
			}
			childIDs := db.Forward(pkg.ID, model.EdgeContains)
			if len(childIDs) > 0 {
				b.WriteString("**Contains:**\n")
				for _, cid := range childIDs {
					if c := db.GetNode(cid); c != nil {
						b.WriteString(fmt.Sprintf("- `%s` (%s)\n", c.Name, c.Kind))
					}
				}
				b.WriteString("\n")
			}
		}
	}

	// edge summary
	b.WriteString("## Edge Types\n\n")
	b.WriteString("| Type | Count |\n")
	b.WriteString("|------|-------|\n")
	for _, k := range sortedEdgeTypes(stats.EdgesByType) {
		b.WriteString(fmt.Sprintf("| %s | %d |\n", k, stats.EdgesByType[model.EdgeType(k)]))
	}

	return b.String()
}

// GeneratePackageSpec produces a README.md for a single package,
// listing its exported symbols, dependencies, and dependents.
func GeneratePackageSpec(db *store.DB, pkgID string) string {
	pkg := db.GetNode(pkgID)
	if pkg == nil {
		return fmt.Sprintf("<!-- package %s not found -->\n", pkgID)
	}

	var b strings.Builder

	b.WriteString(fmt.Sprintf("# %s\n\n", pkg.Name))
	if pkg.Doc != "" {
		b.WriteString(fmt.Sprintf("%s\n\n", pkg.Doc))
	}
	b.WriteString(fmt.Sprintf("**File:** `%s`\n\n", pkg.File))

	// Exported symbols
	childIDs := db.Forward(pkg.ID, model.EdgeContains)
	if len(childIDs) > 0 {
		exported := make([]*model.Node, 0)
		internal := make([]*model.Node, 0)
		for _, cid := range childIDs {
			c := db.GetNode(cid)
			if c == nil {
				continue
			}
			if c.Exported {
				exported = append(exported, c)
			} else {
				internal = append(internal, c)
			}
		}

		if len(exported) > 0 {
			b.WriteString("## Exported Symbols\n\n")
			b.WriteString("| Name | Kind | Signature |\n")
			b.WriteString("|------|------|----------|\n")
			for _, n := range exported {
				sig := n.Sig
				if sig == "" {
					sig = "-"
				}
				b.WriteString(fmt.Sprintf("| `%s` | %s | `%s` |\n", n.Name, n.Kind, sig))
			}
			b.WriteString("\n")
		}

		if len(internal) > 0 {
			b.WriteString("## Internal Symbols\n\n")
			b.WriteString("| Name | Kind |\n")
			b.WriteString("|------|------|\n")
			for _, n := range internal {
				b.WriteString(fmt.Sprintf("| `%s` | %s |\n", n.Name, n.Kind))
			}
			b.WriteString("\n")
		}
	}

	// Dependencies (what this package imports)
	depIDs := db.Forward(pkg.ID, model.EdgeImports)
	if len(depIDs) > 0 {
		b.WriteString("## Dependencies\n\n")
		for _, did := range depIDs {
			d := db.GetNode(did)
			if d == nil {
				continue
			}
			b.WriteString(fmt.Sprintf("- `%s`", d.Name))
			if d.File != "" {
				b.WriteString(fmt.Sprintf(" (%s)", d.File))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Dependents (what imports this package)
	dependentIDs := db.Reverse(pkg.ID, model.EdgeImports)
	if len(dependentIDs) > 0 {
		b.WriteString("## Dependents\n\n")
		for _, did := range dependentIDs {
			d := db.GetNode(did)
			if d == nil {
				continue
			}
			b.WriteString(fmt.Sprintf("- `%s`", d.Name))
			if d.File != "" {
				b.WriteString(fmt.Sprintf(" (%s)", d.File))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	return b.String()
}

func sortedKinds(m map[model.Kind]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}

func sortedEdgeTypes(m map[model.EdgeType]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}
