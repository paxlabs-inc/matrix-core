// Command codegraph-ultra is the CLI entry point for CodeGraph Ultra — a
// multi-language code graph builder, query engine, MCP server, and TUI.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codegraph-ultra/internal/cli"
	"codegraph-ultra/internal/mcp"
	"codegraph-ultra/internal/store"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "cg",
		Short: "CodeGraph Ultra — multi-language code graph builder and query engine",
	}
	root.PersistentFlags().String("db", "", "path to SQLite graph database")
	root.PersistentFlags().String("repo", "", "repo name (default: basename of cwd)")
	root.PersistentFlags().Bool("verbose", false, "verbose output")

	root.AddCommand(
		cli.BuildCmd(),
		cli.QueryCmd(),
		cli.SearchCmd(),
		cli.NeighborsCmd(),
		cli.ImpactCmd(),
		cli.StatsCmd(),
		cli.MCPCmd(),
		cli.MermaidCmd(),
		cli.TUICmd(),
		cli.SpecCmd(),
		cli.ExportCmd(),
		cli.SetupCmd(),
	)

	// "cg mcp serve" starts the MCP stdio server directly.
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start MCP stdio server (for use as MCP tool server)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDBFromFlags(cmd)
			if err != nil {
				return err
			}
			defer database.Close()
			fmt.Fprintf(os.Stderr, "codegraph-ultra mcp: serving over stdio\n")
			return mcp.RunStdio(database)
		},
	}
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server commands",
	}
	mcpCmd.AddCommand(serveCmd)
	root.AddCommand(mcpCmd)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func openDBFromFlags(cmd *cobra.Command) (*store.DB, error) {
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
