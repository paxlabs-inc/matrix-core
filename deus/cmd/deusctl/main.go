package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "deusctl",
		Short: "Deus operator CLI",
	}
	root.AddCommand(newMigrateCmd())
	root.AddCommand(newManifestCmd())
	return root
}
