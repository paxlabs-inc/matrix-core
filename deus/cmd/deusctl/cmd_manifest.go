package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/paxlabs-inc/deus/pkg/manifest"
)

func newManifestCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Manifest utilities",
	}
	validate := &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a service manifest JSON file",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read manifest: %w", err)
			}
			m, err := manifest.ValidateBytes(data)
			if err != nil {
				return err
			}
			hash, err := manifest.Hash(m)
			if err != nil {
				return err
			}
			pricingHash, err := manifest.PricingCommitmentHash(m)
			if err != nil {
				return err
			}
			if jsonOut {
				out := map[string]string{
					"slug":           m.Slug,
					"manifest_hash":  hash,
					"pricing_hash":   pricingHash,
					"schema_version": m.SchemaVersion,
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			fmt.Fprintf(os.Stdout, "ok: slug=%s manifest_hash=%s pricing_hash=%s\n", m.Slug, hash, pricingHash)
			return nil
		},
	}
	validate.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	cmd.AddCommand(validate)
	return cmd
}
