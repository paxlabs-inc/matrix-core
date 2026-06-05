// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/store"
	"matrix/executor/mcp"
	"matrix/executor/tool"
)

// infra packages every long-lived dependency the walk subcommand owns:
// the MCP Manager, cortex.Cortex (+ optional embedder), tool.Registry.
//
// Construction is split into NewInfra (open everything, ready for use)
// and infra.Close (drain + stop in reverse order). The walk subcommand
// is the only place these are created; helper subcommands (loader,
// classify) reuse SkillLoader directly without needing infra.

type infra struct {
	manifest *tool.AgentManifest
	manager  *mcp.Manager
	registry *tool.Registry
	cortex   *cortex.Cortex
	store    *store.Store
	hasEmb   bool
}

// infraOpts configures NewInfra.
type infraOpts struct {
	ManifestPath       string
	CortexRoot         string // empty disables cortex
	CortexActor        string // actor name (directory under cortex root)
	WithEmbedder       bool
	WithFireworksEmbed bool
	StderrSink         io.Writer
	SpawnTimeout       time.Duration // default 90s per server
}

// newInfra wires every dependency. Returns an error early on any
// configuration failure; partial-init state is cleaned up before
// returning so callers can simply propagate err.
func newInfra(ctx context.Context, opts infraOpts, t *transcript) (*infra, error) {
	if opts.ManifestPath == "" {
		return nil, fmt.Errorf("infra: ManifestPath required")
	}
	if opts.SpawnTimeout == 0 {
		opts.SpawnTimeout = 90 * time.Second
	}
	if opts.StderrSink == nil {
		opts.StderrSink = os.Stderr
	}

	in := &infra{}
	cleanupOnError := func() {
		_ = in.Close()
	}

	// --- manifest ---
	manifest, err := tool.LoadAgentManifest(opts.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("infra: load manifest %s: %w", opts.ManifestPath, err)
	}
	in.manifest = manifest
	t.Event("manifest.loaded", "infra", map[string]interface{}{
		"path":                 opts.ManifestPath,
		"agent":                manifest.Agent,
		"servers":              len(manifest.Servers),
		"allowed_side_effects": manifest.AllowedSideEffects,
	})

	// --- mcp manager + spawn ---
	in.manager = mcp.NewManager(mcp.ManagerParams{
		StderrSink: opts.StderrSink,
	})
	for _, s := range manifest.Servers {
		var subEnv []string
		// Q18 lock: $env: refs resolve to host process env at spawn time.
		resolved, _, rerr := tool.ResolveEnvList(s.Env, os.LookupEnv)
		if rerr != nil {
			cleanupOnError()
			return nil, fmt.Errorf("infra: env resolve for %q: %w", s.Alias, rerr)
		}
		if len(resolved) > 0 || len(s.Env) > 0 {
			// Inherit parent env (PATH/HOME etc.) plus manifest overrides.
			subEnv = append(append([]string{}, os.Environ()...), resolved...)
		}
		spec := mcp.ServerSpec{
			Alias:         s.Alias,
			Transport:     s.Transport,
			Command:       s.Command,
			Args:          s.Args,
			Env:           subEnv,
			Endpoint:      s.Endpoint,
			Headers:       resolveHeaderEnv(s.Headers),
			PackageDigest: s.PackageDigest,
			ExpectedTools: toolNames(s.Tools),
		}
		spawnCtx, cancel := context.WithTimeout(ctx, opts.SpawnTimeout)
		_, spawnErr := in.manager.Spawn(spawnCtx, spec)
		cancel()
		if spawnErr != nil {
			t.Event("mcp.spawn.error", "infra", map[string]interface{}{
				"alias": s.Alias,
				"error": spawnErr.Error(),
			})
			cleanupOnError()
			return nil, fmt.Errorf("infra: spawn %q: %w", s.Alias, spawnErr)
		}
		t.Event("mcp.spawn.ok", "infra", map[string]interface{}{
			"alias":   s.Alias,
			"version": s.Version,
			"tools":   len(s.Tools),
		})
	}

	// --- registry ---
	reg, err := tool.NewRegistry(tool.RegistryParams{
		Manifest: manifest,
		MCP:      in.manager,
	})
	if err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("infra: registry: %w", err)
	}
	in.registry = reg
	t.Event("registry.built", "infra", map[string]interface{}{
		"tools": len(reg.List()),
	})

	// --- cortex (optional) ---
	if opts.CortexRoot != "" {
		if opts.CortexActor == "" {
			opts.CortexActor = "executor"
		}
		if err := os.MkdirAll(opts.CortexRoot, 0o755); err != nil {
			cleanupOnError()
			return nil, fmt.Errorf("infra: mkdir cortex-root: %w", err)
		}
		s, err := store.Open(opts.CortexRoot, opts.CortexActor, nil)
		if err != nil {
			cleanupOnError()
			return nil, fmt.Errorf("infra: store.Open: %w", err)
		}
		in.store = s
		in.cortex = cortex.New(s)
		t.Event("cortex.opened", "infra", map[string]interface{}{
			"root":  opts.CortexRoot,
			"actor": opts.CortexActor,
		})

		if opts.WithEmbedder || opts.WithFireworksEmbed {
			var emb embed.Embedder
			if opts.WithFireworksEmbed {
				client, eerr := embed.NewAPIEmbedder(embed.APIEmbedderConfig{})
				if eerr != nil {
					t.Event("embedder.fallback", "infra", map[string]interface{}{
						"error": eerr.Error(),
					})
					emb = embed.NewHashEmbedder()
				} else {
					emb = client
				}
			} else {
				emb = embed.NewHashEmbedder()
			}
			if serr := in.cortex.StartEmbedder(cortex.EmbedderOptions{Embedder: emb}); serr != nil {
				t.Event("embedder.start.error", "infra", map[string]interface{}{
					"error": serr.Error(),
				})
			} else {
				in.hasEmb = true
				drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_ = in.cortex.DrainEmbedder(drainCtx)
				cancel()
				t.Event("embedder.started", "infra", map[string]interface{}{
					"fireworks": opts.WithFireworksEmbed,
				})
			}
		}
	}

	return in, nil
}

// Close drains the embedder, closes the cortex store, and stops every
// MCP server. Safe to call multiple times. Errors are logged but not
// returned aggregated (each step is best-effort cleanup).
func (in *infra) Close() error {
	if in == nil {
		return nil
	}
	if in.cortex != nil && in.hasEmb {
		_ = in.cortex.StopEmbedder()
	}
	if in.store != nil {
		_ = in.store.Close()
	}
	if in.manager != nil {
		_ = in.manager.Close()
	}
	return nil
}

func toolNames(list []tool.ToolEntry) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.Name)
	}
	return out
}

func resolveHeaderEnv(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, ok := tool.ResolveEnv(v, os.LookupEnv)
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: unresolved env ref in header %q\n", k)
		}
		out[k] = resolved
	}
	return out
}

// shortHash returns the first 16 chars of a hex string for compact
// transcript fields.
func shortHash(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

// derefIfNotEmpty is a small helper for `transcript.Event` fields.
func derefIfNotEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nowRFC3339 returns time.Now() in RFC3339Nano UTC. Centralised so
// transcripts agree across stages.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// joinClean wraps filepath.Join + filepath.Clean so callers can build
// journal sub-paths in one call.
func joinClean(parts ...string) string {
	return filepath.Clean(filepath.Join(parts...))
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
