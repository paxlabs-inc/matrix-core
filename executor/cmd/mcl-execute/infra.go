// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"centra/core/cortex"
	"centra/core/cortex/embed"
	"centra/core/cortex/store"
	"centra/executor/mcp"
	"centra/executor/tool"
	"centra/packages/vault"
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
	SpawnTimeout       time.Duration  // default 15s; adapters initialize concurrently
	Vault              *vault.Session // seals cortex values at rest; nil = plaintext dev/CLI
	VaultUser          string         // user DID bound into sealed values' associated data
}

// newInfra wires every dependency. Invalid core state (manifest, registry,
// cortex) remains fatal. Individual MCP integration adapters are optional at
// boot: a configuration, identity, transport, or initialize failure omits that
// adapter from the live manifest and registry so one external dependency
// cannot stop the daemon.
func newInfra(ctx context.Context, opts infraOpts, t *transcript) (*infra, error) {
	if opts.ManifestPath == "" {
		return nil, fmt.Errorf("infra: ManifestPath required")
	}
	if opts.SpawnTimeout == 0 {
		opts.SpawnTimeout = 15 * time.Second
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
	liveServers := make([]tool.ServerEntry, 0, len(manifest.Servers))
	unavailableServers := 0
	recordUnavailable := func(event string, server tool.ServerEntry, stage string, cause error) {
		unavailableServers++
		t.Event(event, "infra", map[string]interface{}{
			"alias": server.Alias,
			"stage": stage,
			"error": cause.Error(),
		})
	}
	type spawnResult struct {
		index  int
		server tool.ServerEntry
		stage  string
		err    error
	}
	spawnResults := make(chan spawnResult, len(manifest.Servers))
	for index, server := range manifest.Servers {
		go func(index int, s tool.ServerEntry) {
			// Q18 lock: $env: refs resolve to host process env at spawn time.
			resolved, _, rerr := tool.ResolveEnvList(s.Env, os.LookupEnv)
			if rerr != nil {
				spawnResults <- spawnResult{index: index, server: s, stage: "env.resolve", err: rerr}
				return
			}
			subEnv, privileged, rerr := tool.MCPEnvironment(s.Alias, os.Environ(), resolved)
			if rerr != nil {
				spawnResults <- spawnResult{index: index, server: s, stage: "env.policy", err: rerr}
				return
			}
			var runAs *mcp.ProcessIdentity
			if !privileged {
				identity, configured, ierr := tool.AgentIdentityFromEnv()
				if ierr != nil {
					spawnResults <- spawnResult{index: index, server: s, stage: "identity", err: ierr}
					return
				}
				if configured {
					runAs = &mcp.ProcessIdentity{
						UID: identity.UID, GID: identity.GID,
						Home: identity.Home, User: identity.User,
					}
				}
			}
			spec := mcp.ServerSpec{
				Alias: s.Alias, Transport: s.Transport, Command: s.Command, Args: s.Args,
				Env: subEnv, RunAs: runAs, Endpoint: s.Endpoint, Headers: resolveHeaderEnv(s.Headers),
				PackageDigest: s.PackageDigest, ExpectedTools: toolNames(s.Tools),
			}
			spawnCtx, cancel := context.WithTimeout(ctx, opts.SpawnTimeout)
			_, spawnErr := in.manager.Spawn(spawnCtx, spec)
			cancel()
			spawnResults <- spawnResult{index: index, server: s, stage: "spawn.initialize", err: spawnErr}
		}(index, server)
	}
	orderedResults := make([]spawnResult, len(manifest.Servers))
	for range manifest.Servers {
		result := <-spawnResults
		orderedResults[result.index] = result
	}
	for _, result := range orderedResults {
		if result.err != nil {
			event := "mcp.spawn.error"
			if result.stage != "spawn.initialize" {
				event = "mcp.config.error"
			}
			recordUnavailable(event, result.server, result.stage, result.err)
			continue
		}
		liveServers = append(liveServers, result.server)
		t.Event("mcp.spawn.ok", "infra", map[string]interface{}{
			"alias": result.server.Alias, "version": result.server.Version, "tools": len(result.server.Tools),
		})
	}
	manifest.Servers = liveServers
	if unavailableServers > 0 {
		t.Event("mcp.degraded", "infra", map[string]interface{}{
			"available":   len(liveServers),
			"unavailable": unavailableServers,
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
		// Seal cortex values at rest below the hash boundary, before any
		// consumer (cortex.New, embedder) can write plaintext.
		s.SetVault(opts.Vault, opts.VaultUser)
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

// nowRFC3339 returns time.Now() in RFC3339Nano UTC. Centralized so
// transcripts agree across stages.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// joinClean wraps filepath.Join + filepath.Clean so callers can build
// journal sub-paths in one call.
func joinClean(parts ...string) string {
	return filepath.Clean(filepath.Join(parts...))
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
