# Embedder & Vector

`matrix/cortex` ships an async embedding pipeline built on a pure-Go HNSW vector index. Writes complete in one round-trip — the embedder is a separate goroutine tailing the journal, so embedding-API latency never stalls `Cortex.Write`. Once running, the embedder unlocks `Find Near` and `Find NearURI` on any `cortex.Find` call.

Source files: `cortex/embedder.go`, `cortex/embed/embed.go`, `cortex/embed/api_embedder.go`, `cortex/vector/vector.go`.

---

## Design decisions

**Writes never block on embedding.** `cortex.Write` / `Update` / `Tombstone` commit their Pebble batch and then send a non-blocking notification to the embedder goroutine. A missed notification is recovered on the periodic tick. The embedder's latency is purely observability — `Find Near` on an un-embedded memory simply misses.

**Single-threaded embedder by design.** Multiple embedding goroutines would race on HNSW `Add` ordering and break the replay determinism of `KindEmbed` journal entries. One goroutine processes journal entries in seq order.

**Pure-Go HNSW, no cgo.** The phase-5 lock explicitly ruled out cgo bindings to usearch / hnswlib. The `VectorIndex` interface lets a cgo backend replace the pure-Go implementation without touching consumers.

**Model-change triggers full re-embed.** If the embedder is started with a different model than the last recorded pass (`meta/embed_model`), the cursor rewinds to 0. The per-memory check at `processWriteEntry` (`head.EmbeddingRef.Model == embedder.Model()`) makes this idempotent — already-embedded memories are skipped.

---

## The Embedder interface

```go
type Embedder interface {
    Embed(text string) ([]float32, error)
    Dim() int
    Model() string  // "<name>@<digest>" — deterministic model pin
}
```

Two implementations ship:

- **`embed.HashEmbedder`** — deterministic sha256-driven stub. Produces unit-normalized 768-dim vectors. Geometry is not semantic (sha256 chaos, not language understanding), so neighbors only make sense for identical or substring text. Used in tests and CI; enables a working HNSW index without any API keys.

- **`embed.NewAPIEmbedder`** — HTTP-backed embedder calling a provider endpoint (nomic-embed-text-v1.5 style). Used in production. Requires a working embedder URL + auth token.

---

## Lifecycle

```go
// Start the worker
err := c.StartEmbedder(cortex.EmbedderOptions{
    Embedder:  embed.NewHashEmbedder(),
    IndexPath: "/var/cortex/andrew/vector/index.hnsw",
})
defer c.StopEmbedder()

// Block until worker catches up with the journal head (used in tests)
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
_ = c.DrainEmbedder(ctx)
```

`StartEmbedder` returns only after an initial drain — so calls that happen *after* `StartEmbedder` returns are guaranteed to be embedded in order.

### EmbedderOptions

```go
type EmbedderOptions struct {
    Embedder     embed.Embedder   // required
    IndexPath    string           // on-disk HNSW file; empty = in-memory only
    HNSWParams   vector.Params    // M, efConstruction, efSearch; zero = spec defaults
    TickInterval time.Duration    // safety-net re-check interval; default 5s
    Logf         func(...)        // progress/error sink; default no-op
}
```

HNSW defaults from spec §19: `M=16, efConstruction=200, efSearch=64`.

---

## What the embedder does per journal entry

For every `KindWrite` or `KindUpdate` entry the worker processes:

1. Load the current Head — skip if tombstoned or if already embedded at the current model + version.
2. Load the latest Version and decode Data.
3. Render the **full** form (`forms.RenderFull`) — the most signal-rich representation.
4. Call `Embedder.Embed(text)` → `[]float32`.
5. Allocate an HNSW vertex ID (new for `KindWrite`; reuse existing for `KindUpdate`).
6. Commit a Pebble batch:
   ```
   vec/meta/<id>           ← canonical CBOR VectorMeta (vertex ID, model, dim, vector, hash)
   m/<id>                  ← updated Head with EmbeddingRef populated
   meta/embed_vertex_next  ← next vertex ID
   j/<seq>                 ← KindEmbed journal entry
   idx/smt/memories/…      ← memories SMT update (Head bytes changed)
   ```

`KindTombstone` entries mark the corresponding HNSW node as tombstoned so subsequent searches skip it.

---

## Atomic embed batch

The embed commit is a single Pebble batch — either `vec/meta/<id>` + `m/<id>` + `KindEmbed` journal all land together, or nothing does. This preserves the replay invariant: the `KindEmbed` entry in the journal records the vector hash and vertex ID so a fresh `Rebuild` can re-embed deterministically (the same `Embedder` under `HashEmbedder` determinism produces byte-identical vectors).

---

## HNSW vector index

Pure-Go implementation of the Malkov & Yashunin HNSW paper (Algorithms 1–4, "simple" neighbour selection).

```go
idx := vector.NewIndex(vector.Params{
    Dim:             768,
    M:               16,
    EfConstruction: 200,
    EfSearch:        64,
    Model:           "nomic-embed-text-v1.5@sha256:...",
})

// Add a vector (during embedding)
idx.Add(vertexID, vector.MemoryID(mid), vec)

// Search
hits, err := idx.Search(queryVec, topK)
// hits[i] = {MemoryID, Distance}

// Tombstone a node (so searches skip it)
idx.Tombstone(vector.MemoryID(mid))

// Persist / load
idx.Save(path)
idx, _ = vector.Load(path)
```

The graph is backed by a `VectorStore` (interface). The default `pebbleVectorStore` reads vectors from `vec/meta` in Pebble and caches recently-used ones to avoid decode overhead during neighbor distance probes.

---

## Find Near

`cortex.Find` resolves `Near` / `NearURI` before delegating to `query.Run`:

```go
// Semantic text search
result, err := c.Find(query.Query{
    Type:  []memory.Type{memory.TypeFact, memory.TypePattern},
    Near:  "block height on paxeer mainnet",
    Limit: 5,
})

// Nearest neighbors to an existing memory
nearURI := memory.URI("matrix://cortex/Fact/01JXXX...#1")
result, err := c.Find(query.Query{
    Type:    []memory.Type{memory.TypeFact},
    NearURI: &nearURI,
    Limit:   5,
})
```

`Near` requires a running embedder (`StartEmbedder` must have been called). `NearURI` additionally requires that the referenced memory has been embedded (its `Head.EmbeddingRef` is non-nil).

When neither `OrderBy` nor explicit ordering is set and `Near`/`NearURI` is active, results are ordered by HNSW distance ascending (closest first).

---

## Index file lifecycle

When `IndexPath` is set:
- **Load on start** — `StartEmbedder` tries to load the file. On failure (missing or corrupt), it falls back to rebuilding the in-memory graph from `vec/meta` prefix scan.
- **Save on stop** — `StopEmbedder` persists the graph to `IndexPath`.

If the index file is deleted but `vec/meta` entries exist, `loadOrBuildIndex` reconstructs the graph from `vec/meta` in vertex-ID ascending order — a lossless recovery path without re-calling the embedding API.

---

## Sidecar metadata

`meta/embed_cursor`, `meta/embed_vertex_next`, and `meta/embed_model` are **not** part of `OverallRoot`. They are derived restart state, recomputable from the journal. `Rebuild` clears them; the next `StartEmbedder` call re-derives from seq=0.

---

## Modifying embedding

| What to change | Where |
|---|---|
| Embedding model / endpoint | `embed/api_embedder.go` — URL, auth, model string |
| HNSW construction params | `cortex/embedder.go` — `EmbedderOptions.HNSWParams` (or defaults in `cortex/vector/vector.go`) |
| Text used for embedding | `cortex/embedder.go` — `processWriteEntry`: currently `forms.RenderFull` |
| Tick interval | `cortex/embedder.go` — `EmbedderOptions.TickInterval` (default 5s) |
| Model-change rewind policy | `cortex/embedder.go` — `StartEmbedder` model-check block |
