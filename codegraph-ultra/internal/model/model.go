// Package model defines the core graph types for CodeGraph Ultra.
// Every node and edge is a pure function of source — enrichment is separate.
package model

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// Kind enumerates node types. Structural hierarchy: repo > module > package > file > symbol.
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
	KindClass     Kind = "class"
	KindEnum      Kind = "enum"
	KindTrait     Kind = "trait"
	KindStruct    Kind = "struct"
)

// EdgeType enumerates the relationship set. All edges are bidirectional in the index.
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
	EdgeInherits   EdgeType = "inherits"
	EdgeOverrides  EdgeType = "overrides"
	EdgeUses       EdgeType = "uses"
)

// EdgeTypes is the canonical order for serialization.
var EdgeTypes = []EdgeType{
	EdgeContains, EdgeImports, EdgeCalls, EdgeImplements,
	EdgeEmbeds, EdgeReferences, EdgeDefines, EdgeTests,
	EdgeInherits, EdgeOverrides, EdgeUses,
}

// Range is a 1-indexed inclusive line span.
type Range struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
}

func (r Range) String() string {
	if r.StartLine == r.EndLine {
		return fmt.Sprintf("%d", r.StartLine)
	}
	return fmt.Sprintf("%d:%d", r.StartLine, r.EndLine)
}

// ParseRange parses "N" or "N:M" into a Range.
func ParseRange(s string) Range {
	var r Range
	fmt.Sscanf(s, "%d:%d", &r.StartLine, &r.EndLine)
	if r.EndLine == 0 {
		r.EndLine = r.StartLine
	}
	return r
}

// Enrichment holds regenerable fields that don't affect node identity.
type Enrichment struct {
	Summary       string  `json:"summary,omitempty"`
	SummaryDigest string  `json:"summary_digest,omitempty"`
	Salience      float64 `json:"salience,omitempty"`
	EmbedRef      string  `json:"embed_ref,omitempty"`
}

func (e Enrichment) IsZero() bool {
	return e.Summary == "" && e.SummaryDigest == "" && e.Salience == 0 && e.EmbedRef == ""
}

// Node is one symbol in the graph.
type Node struct {
	ID       string      `json:"id"`
	Kind     Kind        `json:"kind"`
	Name     string      `json:"name"`
	QName    string      `json:"qname"`
	Lang     string      `json:"lang"`
	File     string      `json:"file"`
	Range    Range       `json:"range"`
	Digest   string      `json:"digest"`
	Sig      string      `json:"sig,omitempty"`
	Exported bool        `json:"exported"`
	Doc      string      `json:"doc,omitempty"`
	Enrich   Enrichment  `json:"enrich,omitempty"`
}

// Edge is a directed typed relationship.
type Edge struct {
	Src  string   `json:"src"`
	Dst  string   `json:"dst"`
	Type EdgeType `json:"type"`
	Site string   `json:"site,omitempty"`
}

// GraphStats holds aggregate statistics about a graph.
type GraphStats struct {
	TotalNodes   int            `json:"total_nodes"`
	TotalEdges   int            `json:"total_edges"`
	NodesByKind  map[Kind]int   `json:"nodes_by_kind"`
	EdgesByType  map[EdgeType]int `json:"edges_by_type"`
	FilesCount   int            `json:"files_count"`
	Languages    []string       `json:"languages"`
}

// Digest computes a sha256-based content digest.
func Digest(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h[:8])
}

// IDForSymbol creates a deterministic node ID from package path and symbol name.
func IDForSymbol(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// SortNodes sorts nodes by ID for deterministic output.
func SortNodes(nodes []*Node) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
}

// SortEdges sorts edges by (Src, Type, Dst) for deterministic output.
func SortEdges(edges []Edge) {
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

// LanguageFromExt maps file extensions to language identifiers.
var LanguageFromExt = map[string]string{
	".go":   "go",
	".py":   "python",
	".pyi":  "python",
	".js":   "javascript",
	".jsx":  "javascript",
	".ts":   "typescript",
	".tsx":  "typescript",
	".rs":   "rust",
	".java": "java",
	".c":    "c",
	".cpp":  "cpp",
	".cc":   "cpp",
	".h":    "c",
	".hpp":  "cpp",
	".rb":   "ruby",
	".cs":   "csharp",
}

// Language returns the language identifier for a file path.
func Language(filePath string) string {
	for ext, lang := range LanguageFromExt {
		if strings.HasSuffix(filePath, ext) {
			return lang
		}
	}
	return ""
}
