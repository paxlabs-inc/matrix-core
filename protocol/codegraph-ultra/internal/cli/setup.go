package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

func SetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Auto-configure MCP integration for AI editors and agents",
		Long: `Detect installed editors and generate MCP server configurations.
Supports: Claude Desktop, Cursor, VS Code, opencode, and generic stdio.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			binaryPath, _ := cmd.Flags().GetString("binary")
			dbPath, _ := cmd.Flags().GetString("db")
			repoPath, _ := cmd.Flags().GetString("repo-path")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			clients, _ := cmd.Flags().GetStringSlice("clients")

			if binaryPath == "" {
				p, err := os.Executable()
				if err != nil {
					return fmt.Errorf("cannot determine binary path: %w", err)
				}
				binaryPath = p
			}

			if repoPath == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("cannot determine working directory: %w", err)
				}
				repoPath = cwd
			}

			if dbPath == "" {
				dbPath = "~/.cg/<repo>.db"
			}

			configs := generateConfigs(binaryPath, dbPath, repoPath)

			if len(clients) > 0 {
				filtered := make(map[string]clientConfig)
				for _, c := range clients {
					if cfg, ok := configs[c]; ok {
						filtered[c] = cfg
					}
				}
				configs = filtered
			}

			if len(configs) == 0 {
				fmt.Println("No supported editors detected. Use --clients to specify.")
				return nil
			}

			for name, cfg := range configs {
				if dryRun {
					fmt.Printf("\n=== %s ===\n", name)
					fmt.Printf("  Config path: %s\n", cfg.Path)
					fmt.Printf("  Exists:      %v\n", cfg.Exists)
					data, _ := json.MarshalIndent(cfg.Config, "", "  ")
					fmt.Printf("  Config:\n%s\n", string(data))
					continue
				}

				if err := writeConfig(cfg); err != nil {
					fmt.Fprintf(os.Stderr, "  skipping %s: %v\n", name, err)
					continue
				}
				fmt.Printf("  configured %s -> %s\n", name, cfg.Path)
			}

			fmt.Println("\nSetup complete. Restart your editor to activate.")
			return nil
		},
	}
	cmd.Flags().String("binary", "", "path to codegraph-ultra binary (default: auto-detect)")
	cmd.Flags().String("db", "", "path to graph database (default: auto)")
	cmd.Flags().String("repo-path", "", "repo root path (default: cwd)")
	cmd.Flags().Bool("dry-run", false, "print configs without writing")
	cmd.Flags().StringSlice("clients", nil, "specific clients to configure (claude,cursor,vscode,opencode)")
	return cmd
}

type clientConfig struct {
	Path   string
	Exists bool
	Config map[string]any
}

func generateConfigs(binary, dbPath, repoPath string) map[string]clientConfig {
	configs := make(map[string]clientConfig)

	mcpServerConfig := map[string]any{
		"command": binary,
		"args":    []string{"mcp", "serve"},
		"env": map[string]string{
			"CG_DB":   dbPath,
			"CG_REPO": repoPath,
		},
	}

	// Claude Desktop
	claudePath := claudeConfigPath()
	if claudePath != "" {
		configs["claude"] = clientConfig{
			Path:   claudePath,
			Exists: fileExists(claudePath),
			Config: map[string]any{
				"mcpServers": map[string]any{
					"codegraph-ultra": mcpServerConfig,
				},
			},
		}
	}

	// Cursor
	cursorPath := cursorConfigPath()
	if cursorPath != "" {
		configs["cursor"] = clientConfig{
			Path:   cursorPath,
			Exists: fileExists(cursorPath),
			Config: map[string]any{
				"mcpServers": map[string]any{
					"codegraph-ultra": mcpServerConfig,
				},
			},
		}
	}

	// VS Code (with Continue or Cline extension)
	vscodePath := vscodeSettingsPath()
	if vscodePath != "" {
		configs["vscode"] = clientConfig{
			Path:   vscodePath,
			Exists: fileExists(vscodePath),
			Config: map[string]any{
				"cline.mcpServers": map[string]any{
					"codegraph-ultra": map[string]any{
						"command": binary,
						"args":    []string{"mcp", "serve"},
						"env": map[string]string{
							"CG_DB":   dbPath,
							"CG_REPO": repoPath,
						},
					},
				},
			},
		}
	}

	// opencode
	opencodePath := opencodeConfigPath()
	if opencodePath != "" {
		configs["opencode"] = clientConfig{
			Path:   opencodePath,
			Exists: fileExists(opencodePath),
			Config: map[string]any{
				"mcp": map[string]any{
					"codegraph-ultra": map[string]any{
						"type":    "stdio",
						"command": binary,
						"args":    []string{"mcp", "serve"},
						"env": map[string]string{
							"CG_DB":   dbPath,
							"CG_REPO": repoPath,
						},
					},
				},
			},
		}
	}

	return configs
}

func writeConfig(cfg clientConfig) error {
	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// If config exists, merge rather than overwrite
	if cfg.Exists {
		existing, err := os.ReadFile(cfg.Path)
		if err == nil {
			var merged map[string]any
			if json.Unmarshal(existing, &merged) == nil {
				for k, v := range cfg.Config {
					merged[k] = v
				}
				cfg.Config = merged
			}
		}
	}

	data, err := json.MarshalIndent(cfg.Config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(cfg.Path, data, 0o644)
}

func claudeConfigPath() string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "linux":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "claude", "claude_desktop_config.json")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData != "" {
			return filepath.Join(appData, "Claude", "claude_desktop_config.json")
		}
	}
	return ""
}

func cursorConfigPath() string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".cursor", "mcp.json")
	case "linux":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".cursor", "mcp.json")
	case "windows":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".cursor", "mcp.json")
	}
	return ""
}

func vscodeSettingsPath() string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json")
	case "linux":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "Code", "User", "settings.json")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData != "" {
			return filepath.Join(appData, "Code", "User", "settings.json")
		}
	}
	return ""
}

func opencodeConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode", "mcp.json")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func detectInstalledBinary() string {
	if path, err := exec.LookPath("cg"); err == nil {
		return path
	}
	if path, err := exec.LookPath("codegraph-ultra"); err == nil {
		return path
	}
	self, err := os.Executable()
	if err != nil {
		return "cg"
	}
	return self
}

func resolveDBPath(repoPath string) string {
	if db := os.Getenv("CG_DB"); db != "" {
		return db
	}
	repo := filepath.Base(repoPath)
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.cg/" + repo + ".db"
	}
	return filepath.Join(home, ".cg", repo+".db")
}

func generateStdioConfig(binary, dbPath string) map[string]any {
	return map[string]any{
		"command": binary,
		"args":    []string{"mcp", "serve"},
		"env": map[string]string{
			"CG_DB": dbPath,
		},
	}
}

// mergeConfig merges new config into an existing JSON file's content.
func mergeConfig(existing []byte, newCfg map[string]any) ([]byte, error) {
	var merged map[string]any
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &merged); err != nil {
			merged = make(map[string]any)
		}
	} else {
		merged = make(map[string]any)
	}
	for k, v := range newCfg {
		merged[k] = v
	}
	return json.MarshalIndent(merged, "", "  ")
}

// printSetupSummary prints a summary of what was configured.
func printSetupSummary(configs map[string]clientConfig) {
	fmt.Println("\nMCP Server Configuration Summary:")
	fmt.Println(strings.Repeat("=", 40))
	for name, cfg := range configs {
		status := "new"
		if cfg.Exists {
			status = "updated"
		}
		fmt.Printf("  %-12s %s (%s)\n", name, status, cfg.Path)
	}
	fmt.Println()
	fmt.Println("The MCP server exposes these tools to your AI editor:")
	fmt.Println("  - symbol_lookup:     Look up a symbol by ID")
	fmt.Println("  - search:            Full-text search across all symbols")
	fmt.Println("  - neighbors:         BFS subgraph around a node")
	fmt.Println("  - impact:            What breaks if this changes")
	fmt.Println("  - call_chain:        Trace call paths between symbols")
	fmt.Println("  - type_hierarchy:    Type hierarchy (implements/extends)")
	fmt.Println("  - file_symbols:      All symbols in a file")
	fmt.Println("  - graph_diff:        Compare two graph builds")
	fmt.Println("  - stats:             Aggregate graph statistics")
}
