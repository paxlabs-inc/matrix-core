# Replay

Package `matrix/cortex/replay` implements the cortex replay invariant: drop every derived index and rebuild it deterministically from the canonical journal. `cortex.Rebuild` is the caller-facing surface. The invariant is: drop indexes → walk journal → roots match.

Source files: `cortex/replay/replay.go`, `cortex/replay/drop.go`, `cortex/rebuild.go`.

---

## Design decisions

**Canonical vs derived is the fundamental split.** Everything in `store/` is canonical (kept across drop+rebuild). Everything in `indexes/` is derived (dropped then re-emitted from the canonical state).

```
CANONICAL — never touched by Rebuild:
  m/       mv/      e/      j/      tomb/    snap/    chk/
  meta/journal_head    meta/snapshot_seq

DERIVED — dropped then rebuilt:
  vec/     idx/     salience/    accum/
  meta/embed_cursor    meta/embed_vertex_next    meta/embed_model
  meta/salience_weights    (also a sidecar — rebuilt from KindLearnWeights)
```

**Vec/* is NOT rebuilt by replay.** Re-embedding lives behind the `Embedder` boundary. After `Rebuild`, `Head.EmbeddingRef` bytes are preserved in `m/<id>` (canonical) and the memories SMT root is identical. The HNSW graph file must be rebuilt by the next `StartEmbedder` call — its `loadOrBuildIndex` fallback scans `vec/meta` (which is empty post-Rebuild) and re-processes the journal from seq=0.

**Salience values depend on wall clock.** `salience.Cached` uses a recency-decay component. Different rebuild clocks produce different caches and identical `OverallRoot`s — by design. The `OverallRoot` is not salience-dependent.

---

## Rebuild

```go
result, err := c.Rebuild(cortex.RebuildOptions{
    Logf: log.Printf,
})
```

Pre-conditions:
- `StopEmbedder()` must have been called (or no embedder ever started). `Rebuild` returns `ErrEmbedderRunning` if `c.embed != nil`.
- No concurrent `Write` / `Update` / `UpdateHead` / `Tombstone` / `AddEdge` / `RemoveEdge` / `Compact` operations. Rebuild is not concurrent-safe with mutating operations.

Post-conditions:
- `c.OverallRoot()` equals the pre-drop root (assuming no new writes happened).
- `vec/*` is empty — start the embedder to re-embed.

### RebuildResult

```go
type RebuildResult struct {
    JournalSeq            uint64   // journal head at rebuild time
    MemoriesScanned       uint64   // m/<id> heads re-emitted into idx/ + SMT
    EdgesScanned          uint64   // e/from/* records re-emitted into SMT
    JournalLeavesAppended uint64   // j/<seq> entries staged onto MMR
    PreDropRoot           [32]byte // OverallRoot captured before drop
    PostRebuildRoot       [32]byte // OverallRoot after rebuild
}
```

### Verifying the invariant

```go
// Strongest check: pre-drop root == post-rebuild root (no snapshot needed)
if result.PreDropRoot != result.PostRebuildRoot {
    log.Fatal("replay invariant violated")
}

// Spec §13.4 literal: verify against a specific snapshot manifest
manifest, _ := c.Snapshot("pre-rebuild")
// ... run Rebuild ...
err = replay.VerifyAgainstSnapshot(result, manifest)
```

---

## Drop

`replay.DropDerived(s)` deletes every key under spec's `indexes/` namespace. Uses Pebble's `DeleteRange` for prefix-scoped wipes — O(1) on the LSM level (a single range tombstone), not O(N) point deletes.

```go
err := replay.DropDerived(s)
```

Idempotent — running twice is safe.

```go
// Audit: count how many derived keys remain
n, err := replay.CountDerived(s)
```

---

## Rebuild steps

`replay.Rebuild(s, snap, opts)` runs these phases in order:

```
1. Capture PreDropRoot = s.OverallRoot()

2. DropDerived(s)  — all derived prefixes + sidecar meta keys

3. Rebuild memories:
   - Scan m/<id> for every Head
   - Re-emit idx/type/<t>/<ts>/<id>, idx/tag/<h>/<ts>/<id>,
     idx/frame/<…>/<id>, idx/actor_obj/<…>/<id> (from Head fields)
   - Re-emit salience/<id> (from Head.DeclaredImportance + now)
   - Stage memories SMT update (StageMemoryUpdate)

4. Rebuild edges:
   - Scan e/from/<src>/<t>/<dst> for every EdgeRecord
   - Stage edges SMT update (StageEdgeUpdate)

5. Rebuild journal MMR:
   - Walk j/<seq> from 0 to head
   - Re-stage each leaf into the MMR accumulator
   - Walk KindLearnWeights entries → re-apply EMA steps → rebuild meta/salience_weights

6. Capture PostRebuildRoot = s.OverallRoot()
```

Each phase commits its own individual Pebble batches. Rebuild is NOT atomic across drop+rebuild as a unit. If Rebuild crashes mid-cycle:
- Drop is idempotent — the next Rebuild call starts from scratch with a clean slate.
- Rebuild is also idempotent — re-running produces the same output.
- Between crash and re-run, `OverallRoot()` returns a root over partially-rebuilt state, which compares unequal to any prior snapshot — the correct "dirty state" signal.

---

## Derived vs canonical — the authoritative list

### Kept (canonical)

| Key prefix | Description |
|---|---|
| `m/` | MemoryHead records |
| `mv/` | MemoryVersion records |
| `e/from/` + `e/to/` | EdgeRecord records |
| `j/` | Journal entries |
| `tomb/` | Tombstone markers |
| `snap/` | SnapshotManifest records |
| `chk/` | Compact checkpoint records |
| `meta/journal_head` | Next journal seq |

### Dropped and re-derived

| Key prefix | Re-derived from |
|---|---|
| `idx/type/` | m/<id> Head.Type |
| `idx/tag/` | m/<id> Head.Tags |
| `idx/frame/` | m/<id> Head.Frames |
| `idx/actor_obj/` | m/<id> Head.Frames (TypeEvent only) |
| `idx/smt/` | m/<id> + e/from/* canonical bytes |
| `salience/` | m/<id> Head.DeclaredImportance + now |
| `accum/` | j/<seq> leaf hashes |
| `vec/` | NOT rebuilt by replay — requires embedder re-run |
| `meta/embed_cursor` | Reset to 0 (embedder re-processes from start) |
| `meta/embed_vertex_next` | Reset (embedder re-allocates) |
| `meta/embed_model` | Reset (triggers model-change rewind on next StartEmbedder) |
| `meta/salience_weights` | Re-applied from KindLearnWeights journal entries |
| `meta/goal_state/*` | NOT rebuilt — scheduler re-derives on next tick |
| `meta/compile_cache/*` | NOT rebuilt — compile cache is keyed to snapshot hashes |

---

## Modifying replay

| What to change | Where |
|---|---|
| Add a new derived namespace | `replay/drop.go` — `derivedPrefixes`; `replay/rebuild.go` — re-derive in the appropriate rebuild phase |
| Add a new canonical namespace | Document it; ensure it is NOT in `derivedPrefixes` and NOT dropped |
| Add a new journal kind that affects derived state | `replay/rebuild.go` — handle in the journal walk (step 5) |
| Change the rebuild verification surface | `replay/replay.go` — `VerifyPreservesRoot` / `VerifyAgainstSnapshot` |
