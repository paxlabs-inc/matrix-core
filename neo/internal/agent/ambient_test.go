// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

// ambientCfg returns a Default config pointed at a hermetic temp cortex store
// (the hash embedder needs no network), with the ambient cap overridden.
func ambientCfg(t *testing.T, topK int) config.Config {
	t.Helper()
	c := config.Default()
	c.CortexRoot = t.TempDir()
	c.CortexActor = "neo-ambient-test"
	c.AmbientRetrievalTopK = topK
	return c
}

// The ambient seed is the bulk semantic retrieval demoted to a thin push (v3
// #1). AmbientRetrievalTopK=0 must inject NOTHING (fully tool-driven — the
// model pulls with memory_recall); a positive cap must bound the seed to that
// many items even when more memories are relevant.
func TestAmbientMemoryHonoursCap(t *testing.T) {
	ctx := context.Background()

	// One shared store, seeded with more facts than any cap under test.
	base := ambientCfg(t, 0)
	p, err := memory.Open(base)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer p.Close()
	for _, f := range []string{
		"the router admin API binds 127.0.0.1:8088",
		"the gateway listens on 127.0.0.1:9090",
		"the executor model is kimi-k2p6",
		"the planner model is deepseek-v4-pro",
		"the dev box repo is at /root/matrix",
	} {
		if _, err := p.RememberFact(ctx, f); err != nil {
			t.Fatalf("RememberFact %q: %v", f, err)
		}
	}

	// Cap 0: fully tool-driven — no ambient seed at all.
	off := New(Options{Config: base, Pager: p})
	if got := off.ambientMemory(ctx, ""); len(got) != 0 {
		t.Errorf("AmbientRetrievalTopK=0 must inject no seed, got %d snippets", len(got))
	}

	// Cap 2: a thin seed, bounded to the cap even though >2 facts are stored.
	cfg2 := base
	cfg2.AmbientRetrievalTopK = 2
	seed := New(Options{Config: cfg2, Pager: p})
	got := seed.ambientMemory(ctx, "")
	if len(got) == 0 {
		t.Fatal("AmbientRetrievalTopK=2 should still inject a (thin) seed when memories exist")
	}
	if len(got) > 2 {
		t.Errorf("ambient seed must be capped at 2, got %d", len(got))
	}
}

// A nil pager (no memory wired) must be a safe no-op regardless of the cap.
func TestAmbientMemoryNilPager(t *testing.T) {
	a := New(Options{Config: ambientCfg(t, 2)})
	if got := a.ambientMemory(context.Background(), "anything"); got != nil {
		t.Errorf("nil pager must yield no ambient seed, got %+v", got)
	}
}

// The demoted seed block must frame itself as a partial seed and point the
// model at memory_recall for the rest (v3 #1), not present as the whole truth.
func TestBuildSystemSeedHeaderNudgesRecall(t *testing.T) {
	a := New(Options{Config: config.Default()})
	retrieved := []memory.Snippet{{Text: "a durable fact", URI: "matrix://cortex/Fact/x#1", Type: "Fact"}}
	sys := a.buildSystem("", retrieved, nil, nil, nil)
	if !strings.Contains(sys, "Memory seed") {
		t.Errorf("retrieved block should be framed as a seed, got:\n%s", sys)
	}
	if !strings.Contains(sys, "memory_recall") {
		t.Errorf("seed block should nudge the model to pull more via memory_recall, got:\n%s", sys)
	}
	if !strings.Contains(sys, "a durable fact") {
		t.Error("the seed snippet text must still be rendered")
	}
}
