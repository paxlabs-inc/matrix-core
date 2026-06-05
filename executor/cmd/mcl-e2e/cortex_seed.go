// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/memory"
)

// FixedClock returns a closure that yields ts on every call. Used by
// cortex.WithClock so memory CreatedAt is byte-stable across runs.
func FixedClock(ts time.Time) func() time.Time {
	return ts.UTC
}

// SeededIDGen returns a memory.NewID-compatible generator backed by a
// deterministic counter so cortex memory IDs are byte-stable across runs.
//
// memory.ID is a 16-byte ULID; we synthesize one from a 6-byte big-endian
// timestamp + 10-byte big-endian counter. Format-compatible with oklog/ulid
// at the byte level (which the cortex store treats as opaque bytes).
func SeededIDGen(initial uint64) func() memory.ID {
	counter := initial
	const fixedTSMs = uint64(1700000000000) // 2023-11-14T22:13:20Z
	tsBytes := make([]byte, 6)
	for i := 0; i < 6; i++ {
		tsBytes[i] = byte((fixedTSMs >> uint((5-i)*8)) & 0xff)
	}
	return func() memory.ID {
		counter++
		var id memory.ID
		copy(id[:6], tsBytes)
		// 10 bytes counter (right-aligned).
		var ctr [10]byte
		binary.BigEndian.PutUint64(ctr[2:], counter)
		copy(id[6:], ctr[:])
		return id
	}
}

// SeedCortex writes the baseline knowledge-graph state every run starts
// with. Same memories, same order, same content → byte-identical
// OverallRoot post-seed across all runs sharing the same FixedClock +
// SeededIDGen. The embedder is started AFTER seeding (and DrainEmbedder
// runs once) so vector writes are also captured into the deterministic
// initial state.
//
// Returns the seed URIs (in write order) so attest can cite them by ref.
func SeedCortex(c *cortex.Cortex, actorURI string, t *Transcript) ([]memory.URI, error) {
	creator := actorURI
	now := time.Now().UTC()

	type seed struct {
		label string
		head  memory.Head
		data  memory.TypedData
	}

	seeds := []seed{
		{
			label: "Identity:Andrew",
			head: memory.Head{
				ActorScope: "private",
				Tags:       []memory.Tag{"andrew", "founder"},
			},
			data: memory.IdentityData{
				SchemaVersion: 1,
				Name:          "Andrew",
				DID:           "did:pax:0xANDREW_DEMO_PLACEHOLDER",
				Roles:         []string{"founder", "engineer"},
			},
		},
		{
			label: "Fact:Matrix-project",
			head: memory.Head{
				ActorScope: "private",
				Tags:       []memory.Tag{"matrix", "project", "v1"},
			},
			data: memory.FactData{
				SchemaVersion: 1,
				Statement:     "Matrix is the cognition+UX layer atop Paxeer Network targeting non-developer end-users.",
				Subject:       "matrix://knowledge/matrix-project",
				Predicate:     "is",
				Source:        "matrix://knowledge/foundations",
			},
		},
		{
			label: "Fact:Paxeer-chain",
			head: memory.Head{
				ActorScope: "private",
				Tags:       []memory.Tag{"paxeer", "blockchain", "infra"},
			},
			data: memory.FactData{
				SchemaVersion: 1,
				Statement:     "Paxeer Network is an L1 blockchain with chain ID 125 (Cronos Release / Alexandria Fork) hosting PAX, OROB, PLV, PoFQ, Argus, and HPS primitives.",
				Subject:       "matrix://knowledge/paxeer-network",
				Predicate:     "is",
			},
		},
		{
			label: "Goal:v1-launch",
			head: memory.Head{
				ActorScope:         "private",
				Tags:               []memory.Tag{"v1", "launch", "active"},
				DeclaredImportance: 9, // 9/10 importance per memory.validateImportance
			},
			data: memory.GoalData{
				SchemaVersion: 1,
				Statement:     "Ship Matrix v1: MCL compiler + cortex + executor + bridge with no chain coupling.",
				Status:        memory.GoalActive,
			},
		},
		{
			label: "Constraint:no-chain-v1",
			head: memory.Head{
				ActorScope: "private",
				Tags:       []memory.Tag{"v1", "scope"},
			},
			data: memory.ConstraintData{
				SchemaVersion: 1,
				Statement:     "v1 must not couple to chain primitives. Tools/* may be stubbed; no on-chain transactions executed.",
				Polarity:      memory.PolarityAvoid,
				StrengthVal:   memory.StrengthHard,
				Source:        memory.ConstraintSourceUserDeclared,
			},
		},
		{
			label: "Pattern:executor-walks-plan",
			head: memory.Head{
				ActorScope: "private",
				Tags:       []memory.Tag{"architecture", "executor", "plan"},
			},
			data: memory.PatternData{
				SchemaVersion: 1,
				Statement:     "The executor walks a typed PlanTree DFS, dispatching tool calls via matrix://tool/mcp/<alias>/<name>@<version> URIs.",
				Strength:      0.9,
				Coverage:      3,
			},
		},
	}

	uris := make([]memory.URI, 0, len(seeds))
	for i := range seeds {
		s := &seeds[i]
		uri, err := c.Write(s.head, s.data, cortex.WriteMeta{
			CreatedBy:  creator,
			Confidence: 1.0,
			Provenance: memory.Provenance{Source: memory.SourceUserInput},
		})
		if err != nil {
			return nil, fmt.Errorf("seed %s: %w", s.label, err)
		}
		uris = append(uris, uri)
		t.Event("cortex.write", "seed", map[string]interface{}{
			"label": s.label,
			"uri":   string(uri),
		})
	}

	// Render OverallRoot post-seed for reporting (used as the snapshot_hash
	// that flows into the compiler determinism seed D11).
	root, err := c.OverallRoot()
	if err != nil {
		return nil, fmt.Errorf("post-seed OverallRoot: %w", err)
	}
	t.Event("cortex.seed.complete", "seed", map[string]interface{}{
		"count":        len(seeds),
		"overall_root": fmt.Sprintf("%x", root),
		"now":          now.Format(time.RFC3339Nano),
	})
	return uris, nil
}

// MakeAndDrainEmbedder constructs the real Fireworks APIEmbedder, attaches
// it to c, and blocks until the worker has caught up. The drain timeout is
// generous (60s) because the first call cold-starts an HTTP connection.
func MakeAndDrainEmbedder(c *cortex.Cortex) (embed.Embedder, error) {
	emb, err := embed.NewAPIEmbedder(embed.APIEmbedderConfig{})
	if err != nil {
		return nil, fmt.Errorf("embed.NewAPIEmbedder: %w", err)
	}
	if err := c.StartEmbedder(cortex.EmbedderOptions{Embedder: emb}); err != nil {
		return nil, fmt.Errorf("c.StartEmbedder: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.DrainEmbedder(ctx); err != nil {
		return nil, fmt.Errorf("c.DrainEmbedder: %w", err)
	}
	return emb, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
