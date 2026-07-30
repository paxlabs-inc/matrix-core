package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codegraph-ultra/internal/export"
	"codegraph-ultra/internal/extract"
	"codegraph-ultra/internal/model"
	"codegraph-ultra/internal/specgen"
	"codegraph-ultra/internal/store"

	// Register language extractors via init().
	_ "codegraph-ultra/internal/extract/go"
	_ "codegraph-ultra/internal/extract/python"
	_ "codegraph-ultra/internal/extract/typescript"

	"github.com/spf13/cobra"
)

// openDB resolves the DB path from flags and opens it.
func openDB(cmd *cobra.Command) (*store.DB, error) {
	db, _ := cmd.Flags().GetString("db")
	if db == "" {
		repo, _ := cmd.Flags().GetString("repo")
		if repo == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("cannot determine working directory: %w", err)
			}
			repo = filepath.Base(cwd)
		}
		repo = strings.ReplaceAll(repo, "/", "_")
		repo = strings.ReplaceAll(repo, " ", "_")
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory: %w", err)
		}
		dir := filepath.Join(home, ".cg")
		os.MkdirAll(dir, 0o755)
		db = filepath.Join(dir, repo+".db")
	}
	return store.Open(db)
}

// ───────────────────────── build ─────────────────────────

func BuildCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build [path]",
		Short: "Parse a codebase and build the code graph",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			verbose, _ := cmd.Flags().GetBool("verbose")
			excludePatterns, _ := cmd.Flags().GetStringSlice("exclude")

			repoName := filepath.Base(absDir)

			if verbose {
				fmt.Fprintf(os.Stderr, "building graph for %s (repo=%s)\n", absDir, repoName)
			}

			// Clear existing graph for a fresh build.
			if err := database.Clear(); err != nil {
				return fmt.Errorf("clear db: %w", err)
			}

			var totalNodes, totalEdges int

			// Run each registered extractor.
			for lang, factory := range extract.Registry {
				if verbose {
					fmt.Fprintf(os.Stderr, "  extracting %s...\n", lang)
				}
				ex := factory()
				// Go discovers its own modules; Python/TS walk from root.
				var modules []string
				if lang == "go" {
					modules = nil // let the Go extractor auto-discover go.mod files
				} else {
					modules = filterExclude([]string{absDir}, excludePatterns)
				}
				result, err := ex.Extract(extract.Config{
					RepoRoot: absDir,
					RepoName: repoName,
					Modules:  modules,
				})
				if err != nil {
					if verbose {
						fmt.Fprintf(os.Stderr, "  skipping %s: %v\n", lang, err)
					}
					continue
				}

				// Collect node IDs for edge validation.
				nodeIDs := make(map[string]bool, len(result.Nodes))
				for _, n := range result.Nodes {
					nodeIDs[n.ID] = true
					if err := database.UpsertNode(n); err != nil {
						return fmt.Errorf("upsert node %s: %w", n.ID, err)
					}
				}
				for _, e := range result.Edges {
					// Skip edges to nodes outside this extraction (e.g. stdlib).
					if !nodeIDs[e.Src] || !nodeIDs[e.Dst] {
						continue
					}
					if err := database.UpsertEdge(e); err != nil {
						return fmt.Errorf("upsert edge %s->%s: %w", e.Src, e.Dst, err)
					}
				}
				totalNodes += len(result.Nodes)
				totalEdges += len(result.Edges)

				if verbose {
					fmt.Fprintf(os.Stderr, "  %s: %d nodes, %d edges\n", lang, len(result.Nodes), len(result.Edges))
				}
			}

			database.SetMeta("repo", repoName)
			database.SetMeta("root", absDir)

			stats := database.Stats()
			fmt.Printf("graph ready — %d nodes, %d edges across %d files\n",
				stats.TotalNodes, stats.TotalEdges, stats.FilesCount)
			return nil
		},
	}
	cmd.Flags().IntP("workers", "w", 4, "parallel parser workers")
	cmd.Flags().StringSliceP("exclude", "x", nil, "glob patterns to exclude")
	return cmd
}

func filterExclude(paths []string, patterns []string) []string {
	if len(patterns) == 0 {
		return paths
	}
	var out []string
	for _, p := range paths {
		skip := false
		for _, pat := range patterns {
			if matched, _ := filepath.Match(pat, filepath.Base(p)); matched {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, p)
		}
	}
	return out
}

// ───────────────────────── query ─────────────────────────

func QueryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "query <node-id>",
		Short: "Show details for a specific node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			node := database.GetNode(args[0])
			if node == nil {
				return fmt.Errorf("node not found: %s", args[0])
			}
			printNode(node)
			return nil
		},
	}
}

// ───────────────────────── search ─────────────────────────

func SearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Search for symbols by name or qualified name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			limit, _ := cmd.Flags().GetInt("limit")
			nodes := database.Search(args[0], limit)
			if len(nodes) == 0 {
				fmt.Println("no results")
				return nil
			}
			for _, n := range nodes {
				fmt.Printf("  %-12s %-40s %s\n", n.Kind, n.QName, n.File)
			}
			return nil
		},
	}
}

// ───────────────────────── neighbors ─────────────────────────

func NeighborsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "neighbors <node-id>",
		Short: "Show neighbors of a node (callers, callees, imports, etc.)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			edgeType, _ := cmd.Flags().GetString("type")
			depth, _ := cmd.Flags().GetInt("depth")
			results := database.Neighbors(args[0], model.EdgeType(edgeType), depth)
			if len(results) == 0 {
				fmt.Println("no neighbors found")
				return nil
			}
			for _, n := range results {
				fmt.Printf("  %-12s %-40s %s\n", n.Kind, n.QName, n.File)
			}
			return nil
		},
	}
	cmd.Flags().StringP("type", "t", "", "edge type filter (calls, imports, references, contains)")
	cmd.Flags().IntP("depth", "", 1, "traversal depth")
	return cmd
}

// ───────────────────────── impact ─────────────────────────

func ImpactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "impact <node-id>",
		Short: "Show the impact radius of a node (what depends on it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			maxDepth, _ := cmd.Flags().GetInt("max-depth")
			results := database.Impact(args[0], maxDepth)
			if len(results) == 0 {
				fmt.Println("no impact — nothing depends on this node")
				return nil
			}
			fmt.Printf("impact radius: %d nodes\n", len(results))
			for _, n := range results {
				fmt.Printf("  %-12s %-40s %s\n", n.Kind, n.QName, n.File)
			}
			return nil
		},
	}
	cmd.Flags().IntP("max-depth", "", 3, "max reverse traversal depth")
	return cmd
}

// ───────────────────────── stats ─────────────────────────

func StatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show graph statistics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			s := database.Stats()
			fmt.Printf("Nodes:       %d\n", s.TotalNodes)
			fmt.Printf("Edges:       %d\n", s.TotalEdges)
			fmt.Printf("Files:       %d\n", s.FilesCount)
			fmt.Printf("Languages:   %s\n", strings.Join(s.Languages, ", "))
			fmt.Println()
			fmt.Println("By kind:")
			for kind, count := range s.NodesByKind {
				fmt.Printf("  %-20s %d\n", kind, count)
			}
			fmt.Println("By edge type:")
			for etype, count := range s.EdgesByType {
				fmt.Printf("  %-20s %d\n", etype, count)
			}
			return nil
		},
	}
}

// ───────────────────────── mcp ─────────────────────────

func MCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp-config [output-path]",
		Short: "Generate a standalone MCP server config for the graph",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outPath := "mcp-config.json"
			if len(args) > 0 {
				outPath = args[0]
			}
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			if err := export.ExportMCPServer(database, outPath); err != nil {
				return err
			}
			fmt.Printf("wrote MCP config to %s\n", outPath)
			return nil
		},
	}
}

// ───────────────────────── mermaid ─────────────────────────

func MermaidCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mermaid [output-file]",
		Short: "Export the graph as a Mermaid diagram",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outPath := ""
			if len(args) > 0 {
				outPath = args[0]
			}
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			diagramType, _ := cmd.Flags().GetString("diagram")
			if err := export.ExportMermaid(database, outPath, diagramType); err != nil {
				return err
			}
			if outPath != "" {
				fmt.Printf("wrote mermaid diagram to %s\n", outPath)
			}
			return nil
		},
	}
	cmd.Flags().StringP("diagram", "", "graph", "diagram type: graph, flowchart, sequence")
	return cmd
}

// ───────────────────────── tui ─────────────────────────

func TUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive TUI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			tui, err := newTUI(database)
			if err != nil {
				return err
			}
			return tui.Run()
		},
	}
}

// ───────────────────────── spec ─────────────────────────

func SpecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spec",
		Short: "Generate AGENTS.md and per-package README specs from the graph",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				cwd, _ := os.Getwd()
				repo = filepath.Base(cwd)
			}

			outDir, _ := cmd.Flags().GetString("output")
			if outDir == "" {
				outDir = "."
			}

			// Generate AGENTS.md
			agentSpec := specgen.GenerateAgentSpec(database, repo)
			agentPath := filepath.Join(outDir, "AGENTS.md")
			if err := os.WriteFile(agentPath, []byte(agentSpec), 0o644); err != nil {
				return fmt.Errorf("write AGENTS.md: %w", err)
			}
			fmt.Printf("wrote %s\n", agentPath)

			// Generate per-package READMEs if package flag set
			generatePkgs, _ := cmd.Flags().GetBool("packages")
			if generatePkgs {
				nodes := database.Nodes("package")
				for _, n := range nodes {
					readme := specgen.GeneratePackageSpec(database, n.ID)
					pkgDir := filepath.Join(outDir, filepath.Dir(n.File))
					os.MkdirAll(pkgDir, 0o755)
					readmePath := filepath.Join(pkgDir, "README.md")
					if err := os.WriteFile(readmePath, []byte(readme), 0o644); err != nil {
						return fmt.Errorf("write %s: %w", readmePath, err)
					}
					fmt.Printf("wrote %s\n", readmePath)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringP("output", "o", ".", "output directory")
	cmd.Flags().BoolP("packages", "p", false, "also generate per-package README files")
	return cmd
}

// ───────────────────────── export ─────────────────────────

func ExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <format> [output-file]",
		Short: "Export the graph (json, kvx, mermaid)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format := args[0]
			outPath := ""
			if len(args) > 1 {
				outPath = args[1]
			}

			database, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer database.Close()

			switch format {
			case "json":
				if outPath == "" {
					outPath = "graph.json"
				}
				return export.ExportJSON(database, outPath)
			case "kvx":
				if outPath == "" {
					outPath = "graph.kvx"
				}
				return export.ExportKVX(database, outPath)
			case "mermaid":
				diagramType, _ := cmd.Flags().GetString("diagram")
				return export.ExportMermaid(database, outPath, diagramType)
			default:
				return fmt.Errorf("unknown format: %s (supported: json, kvx, mermaid)", format)
			}
		},
	}
	cmd.Flags().StringP("diagram", "", "graph", "mermaid diagram type")
	return cmd
}

// ───────────────────────── helpers ─────────────────────────

func printNode(n *model.Node) {
	fmt.Printf("ID:        %s\n", n.ID)
	fmt.Printf("Kind:      %s\n", n.Kind)
	fmt.Printf("Name:      %s\n", n.Name)
	fmt.Printf("QName:     %s\n", n.QName)
	fmt.Printf("Lang:      %s\n", n.Lang)
	fmt.Printf("File:      %s\n", n.File)
	if n.Sig != "" {
		fmt.Printf("Sig:       %s\n", n.Sig)
	}
	if n.Doc != "" {
		fmt.Printf("Doc:       %s\n", n.Doc)
	}
	fmt.Printf("Exported:  %s\n", strconv.FormatBool(n.Exported))
	if n.Enrich.Summary != "" {
		fmt.Printf("Summary:   %s\n", n.Enrich.Summary)
	}
	if n.Enrich.Salience > 0 {
		fmt.Printf("Salience:  %.2f\n", n.Enrich.Salience)
	}
}

func toJSON(v interface{}) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// TUI launch helper — delegates to internal/tui
func newTUI(database *store.DB) (runner, error) {
	return launchTUI(database)
}

type runner interface {
	Run() error
}
