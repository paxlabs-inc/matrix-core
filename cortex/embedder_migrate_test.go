// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Lazy-migration tests for the embedder model swap (sess#19 Q3 lock).
//
// When StartEmbedder is called with an Embedder whose Model() differs from the
// last-persisted model (meta/embed_model), the worker rewinds its cursor to 0
// and re-walks j/<seq>, re-embedding every memory under the new model. The
// model-pin check at processWriteEntry (head.EmbeddingRef.Model ==
// s.embedder.Model() && !Stale) makes the re-walk idempotent: memories
// already at the new model are skipped.

package cortex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"matrix/cortex/embed"
	"matrix/cortex/memory"
)

// saltedEmbedder wraps HashEmbedder but reports a custom Model() so we can
// simulate a model swap without standing up a real API endpoint.
type saltedEmbedder struct {
	inner    *embed.HashEmbedder
	modelTag string
}

func newSaltedEmbedder(salt, tag string) *saltedEmbedder {
	return &saltedEmbedder{
		inner:    embed.NewHashEmbedderWith(embed.DefaultDim, salt),
		modelTag: tag,
	}
}

func (s *saltedEmbedder) Embed(text string) ([]float32, error) { return s.inner.Embed(text) }
func (s *saltedEmbedder) Dim() int                             { return s.inner.Dim() }
func (s *saltedEmbedder) Model() string                        { return s.modelTag }

// TestEmbedderModelSwap_LazyMigration verifies the Q3 α lock:
//  1. Write a memory under model A; verify it embeds under A.
//  2. Stop the embedder.
//  3. Restart with model B; verify the cursor rewinds and the memory
//     re-embeds under B.
//  4. The Head's EmbeddingRef.Model now reads B.
func TestEmbedderModelSwap_LazyMigration(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)

	idxPath := filepath.Join(t.TempDir(), "index.hnsw")

	// First pass: model A.
	embA := newSaltedEmbedder("", "model-A@test")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embA,
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder (A): %v", err)
	}
	drainEmbedder(t, c)

	mem1, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve pre-swap: %v", err)
	}
	if mem1.Head.EmbeddingRef == nil {
		t.Fatal("EmbeddingRef nil after first pass")
	}
	if mem1.Head.EmbeddingRef.Model != "model-A@test" {
		t.Errorf("first pass model = %q, want %q", mem1.Head.EmbeddingRef.Model, "model-A@test")
	}
	vertexA := mem1.Head.EmbeddingRef.VertexID

	// Stop and swap.
	if err := c.StopEmbedder(); err != nil {
		t.Fatalf("StopEmbedder: %v", err)
	}

	// Second pass: model B. Lazy-migration must rewind cursor and re-embed.
	idxPath2 := filepath.Join(t.TempDir(), "index2.hnsw")
	embB := newSaltedEmbedder("alt-salt", "model-B@test")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embB,
		IndexPath:    idxPath2,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder (B): %v", err)
	}
	drainEmbedder(t, c)

	mem2, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve post-swap: %v", err)
	}
	if mem2.Head.EmbeddingRef == nil {
		t.Fatal("EmbeddingRef nil after migration")
	}
	if mem2.Head.EmbeddingRef.Model != "model-B@test" {
		t.Errorf("post-swap model = %q, want %q", mem2.Head.EmbeddingRef.Model, "model-B@test")
	}
	// Vertex ID is reused per the existing reuse-on-re-embed logic
	// (embedder.go:451-454).
	if mem2.Head.EmbeddingRef.VertexID != vertexA {
		t.Errorf("vertex id changed across re-embed: %d → %d (want stable)",
			vertexA, mem2.Head.EmbeddingRef.VertexID)
	}
}

// TestEmbedderModelSwap_NoSwapNoRewalk verifies that restarting with the
// SAME model does NOT rewind the cursor — the worker resumes from where
// it left off. This is the steady-state restart path (process restart on
// an unchanged deployment).
func TestEmbedderModelSwap_NoSwapNoRewalk(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)

	idxPath := filepath.Join(t.TempDir(), "index.hnsw")
	emb := newSaltedEmbedder("", "stable-model@test")

	// First start: drains everything, cursor advances past journal head.
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     emb,
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder #1: %v", err)
	}
	drainEmbedder(t, c)

	// Cursor should be > 0 now.
	cursor1, err := readMetaUint64(c.s, metaEmbedCursor)
	if err != nil {
		t.Fatalf("read cursor #1: %v", err)
	}
	if cursor1 == 0 {
		t.Fatal("cursor should advance past 0 after embedding two memories")
	}

	if err := c.StopEmbedder(); err != nil {
		t.Fatalf("StopEmbedder: %v", err)
	}

	// Second start: SAME model. Cursor must NOT reset.
	emb2 := newSaltedEmbedder("", "stable-model@test")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     emb2,
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder #2: %v", err)
	}

	cursor2, err := readMetaUint64(c.s, metaEmbedCursor)
	if err != nil {
		t.Fatalf("read cursor #2: %v", err)
	}
	if cursor2 != cursor1 {
		t.Errorf("cursor reset across same-model restart: %d → %d (want stable)",
			cursor1, cursor2)
	}
}

// TestEmbedderModelSwap_PersistsModelTag verifies that meta/embed_model is
// written on first StartEmbedder so the next start can detect a swap.
func TestEmbedderModelSwap_PersistsModelTag(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)

	idxPath := filepath.Join(t.TempDir(), "index.hnsw")
	emb := newSaltedEmbedder("", "tag-test@v1")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     emb,
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder: %v", err)
	}
	t.Cleanup(func() { _ = c.StopEmbedder() })

	got, ok, err := c.s.Get(metaEmbedModel)
	if err != nil {
		t.Fatalf("get embed_model: %v", err)
	}
	if !ok {
		t.Fatal("meta/embed_model not persisted after StartEmbedder")
	}
	if string(got) != "tag-test@v1" {
		t.Errorf("meta/embed_model = %q, want %q", string(got), "tag-test@v1")
	}
}

// TestEmbedderModelSwap_DrainBlocksOnRewalk verifies that the initial-drain
// inside StartEmbedder picks up the rewalk: by the time StartEmbedder
// returns, all memories are re-embedded under the new model. Otherwise we'd
// have a stale-read window where Find Near returns mixed-model results.
func TestEmbedderModelSwap_DrainBlocksOnRewalk(t *testing.T) {
	c := openCortex(t)
	uri1 := writePref(t, c, "tone", 5)
	uri2 := writePref(t, c, "tempo", 7)
	uri3 := writePref(t, c, "verbosity", 3)

	idxPath := filepath.Join(t.TempDir(), "index.hnsw")

	// Pass 1: model A.
	embA := newSaltedEmbedder("", "alpha@v1")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embA,
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder A: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.DrainEmbedder(ctx)

	if err := c.StopEmbedder(); err != nil {
		t.Fatalf("StopEmbedder: %v", err)
	}

	// Pass 2: model B. The post-StartEmbedder drain must catch every memory.
	embB := newSaltedEmbedder("salt", "beta@v1")
	idxPath2 := filepath.Join(t.TempDir(), "index2.hnsw")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embB,
		IndexPath:    idxPath2,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder B: %v", err)
	}
	t.Cleanup(func() { _ = c.StopEmbedder() })

	// Verify all three memories are at model B WITHOUT calling drain — the
	// initial-drain inside StartEmbedder must have already caught them.
	for _, uri := range []memory.URI{uri1, uri2, uri3} {
		mem, err := c.Resolve(uri)
		if err != nil {
			t.Fatalf("Resolve %s: %v", uri, err)
		}
		if mem.Head.EmbeddingRef == nil {
			t.Errorf("%s: EmbeddingRef nil after migration", uri)
			continue
		}
		if mem.Head.EmbeddingRef.Model != "beta@v1" {
			t.Errorf("%s: model = %q, want beta@v1", uri, mem.Head.EmbeddingRef.Model)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
