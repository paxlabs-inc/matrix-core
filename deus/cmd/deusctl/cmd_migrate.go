package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/paxlabs-inc/deus/internal/config"
	"github.com/paxlabs-inc/deus/internal/store"
)

func newMigrateCmd() *cobra.Command {
	var migrationsDir string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply forward-only SQL migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			dir := migrationsDir
			if dir == "" {
				dir = cfg.MigrationsDir
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(moduleRoot(), dir)
			}
			db, err := store.New(ctx, cfg.PostgresURI)
			if err != nil {
				return err
			}
			defer db.Close()
			if err := db.Migrate(ctx, dir); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "migrations applied from %s\n", dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&migrationsDir, "migrations-dir", "", "override migrations directory")
	return cmd
}

func moduleRoot() string {
	if root := os.Getenv("DEUS_ROOT"); root != "" {
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func init() {
	// Ensure cobra subcommands inherit a non-nil context.
	cobra.EnableCommandSorting = true
	_ = context.Background()
}
