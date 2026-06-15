# Store & Journal

Package `matrix/cortex/store` manages the per-actor Pebble database and the atomic write batch machinery. Package `matrix/cortex/journal` defines the append-only write log entry shape. Package `matrix/cortex/keys` encodes all Pebble keys. Together they are the foundation every other cortex subsystem builds on.

Source files: `cortex/store/store.go`, `cortex/store/writebatch.go`, `cortex/keys/keys.go`, `cortex/journal/journal.go`.

---

## Design decisions

**One Pebble DB per actor.** §11.1 requires that journal + head + version + indexes commit atomically. Pebble batches cannot span databases, so a single Pebble DB per actor is mandatory. All namespaces are key prefixes inside that DB — not separate files or databases.

**Every mutation is journaled.** `WriteBatch.Commit` returns `ErrBatchNoJournal` if the caller never called `AppendJournal`. This is load-bearing: the journal is the replay anchor, and a write that bypasses it silently breaks the `Rebuild` invariant.

**Journal sequence is per-actor monotonic, gap-free.** Allocation happens inside `BeginWrite`, which holds `seqMu` until `Commit` or `Abort`. Gap-free invariant: if seq=N exists, seq=N-1 must exist too. The embedder and replay harness depend on this.

---

## Store

```go
s, err := store.Open(root, actor, nil)
defer s.Close()
```

Opens (or creates) the Pebble DB at `<root>/<actor>/store/`. The `actor` string must not contain `/` — enforced by `keys.ValidateNoSeparator`.

```go
// Point read
value, ok, err := s.Get(key)

// Prefix scan
err = s.PrefixIter(keys.PrefixIdxType, func(k, v []byte) error { ... })

// Journal seq (next-to-allocate)
nextSeq := s.NextSeq()
```

### JournalHook

The snapshot layer installs a hook on the store so every `AppendJournal` call simultaneously stages an MMR leaf in the same atomic batch:

```go
s.SetJournalHook(c.snap.MMRHook())
```

The hook fires per `AppendJournal` call, inside the same `WriteBatch`. It receives the raw Pebble batch and the leaf hash so it can stage additional keys without a separate commit.

---

## WriteBatch

The atomic write transaction. Every mutating cortex operation (`Write`, `Update`, `Tombstone`, `AddEdge`, `UpdateHead`, `Compact`, `Attest`) creates one `WriteBatch`, stages all its keys, appends at least one journal entry, then commits.

```go
wb := s.BeginWrite()
defer wb.Abort()  // no-op if Commit already succeeded

wb.Set(key, value)          // any key: m/, mv/, idx/, salience/, …
wb.Delete(key)              // for UpdateHead idx/tag/idx/frame removals
wb.AppendJournal(entry)     // at least once; may be called twice (Attest)
seq := wb.Seq()             // seq of the most recently appended entry

if err := wb.Commit(); err != nil { ... }
```

`BeginWrite` acquires `seqMu`. Only one `WriteBatch` may be open per `Store` at a time. The backing Pebble batch is an indexed batch so that staged writes within one batch are visible to sibling reads in the same batch — required for `cortex.Attest`'s two-entry pattern (KindAttest at seq=N, KindLearnWeights at seq=N+1).

---

## Key encoding

All Pebble keys are defined in `cortex/keys`. Invariants:
- All numeric components are big-endian fixed-width so byte-sort == numeric-sort.
- IDs are 16-byte binary ULIDs (textual only at API boundaries).
- Versions and seq numbers are `uint64`, 8 bytes BE.
- The path separator `'/'` is forbidden inside path components.

### Namespace prefixes

| Prefix | Key shape | Purpose |
|---|---|---|
| `m/` | `m/<id:16>` | MemoryHead (canonical, mutable) |
| `mv/` | `mv/<id:16>/v/<version:8>` | MemoryVersion (canonical, immutable) |
| `e/from/` | `e/from/<src:16>/<type:1>/<dst:16>` | Forward edge records |
| `e/to/` | `e/to/<dst:16>/<type:1>/<src:16>` | Reverse edge records |
| `j/` | `j/<seq:8>` | Journal entries (canonical) |
| `tomb/` | `tomb/<id:16>` | Tombstone markers |
| `snap/` | `snap/<seq:8>` | SnapshotManifest records |
| `chk/` | `chk/<intent>/<step>` | Compact checkpoint records |
| `idx/type/` | `idx/type/<t:1>/<created:8>/<id:16>` | Type index |
| `idx/tag/` | `idx/tag/<hash:8>/<created:8>/<id:16>` | Tag index |
| `idx/frame/` | `idx/frame/<verb:1>/<kind:1>/<hash:16>/<id:16>` | Frame-relevance index |
| `idx/actor_obj/` | `idx/actor_obj/<verb:1>/<hash:16>/<created:8>/<id:16>` | Outcomes index |
| `idx/smt/` | `idx/smt/<ns>/n/<depth:2>/<path:32>` | SMT node cache |
| `salience/` | `salience/<id:16>` | Per-memory salience score cache |
| `vec/meta/` | `vec/meta/<id:16>` | Vector embedding + HNSW vertex metadata |
| `accum/` | `accum/mmr/…` | Journal MMR accumulator leaves |
| `meta/` | `meta/<key>` | Store-level metadata |

### Important meta/ keys

| Key | Purpose |
|---|---|
| `meta/journal_head` | Next journal seq to allocate (uint64 BE) |
| `meta/salience_weights` | Per-actor learned EMA weights (canonical CBOR `Weights`) |
| `meta/compile_cache/<hex>` | Compile-result sidecar cache (not in OverallRoot) |
| `meta/goal_state/<id:16>` | Ambient scheduler runtime state per Goal (not in OverallRoot) |
| `meta/embed_cursor` | Next journal seq for the embedding worker to process |
| `meta/embed_vertex_next` | Next HNSW vertex ID to allocate |
| `meta/embed_model` | Model string of the last completed embedding pass |

**Sidecar posture:** `meta/salience_weights`, `meta/compile_cache/*`, and `meta/goal_state/*` are derived / runtime-policy state. They are NOT inputs to `OverallRoot`. The `Rebuild` operation drops and re-derives them from the journal.

---

## Journal

The journal is the cortex's write-ahead log and audit ledger. Every `WriteBatch.Commit` writes at least one entry at `j/<seq>`.

### Entry shape

```go
type Entry struct {
    Seq       uint64   // per-actor monotonic, gap-free
    Kind      Kind     // string: "write" | "update" | "tombstone" | …
    CreatedAt int64    // Unix nanoseconds
    CreatedBy []byte   // agent ref
    Payload   []byte   // canonical CBOR — kind-specific payload
}
```

### Leaf hash

```go
leafHash = sha256("matrix.cortex.journal.v1" || canonical_CBOR_entry)
```

Domain-separation prefix prevents cross-protocol hash collisions. The leaf hash feeds the journal MMR.

### Journal kinds

| Kind | When |
|---|---|
| `write` | `cortex.Write` |
| `update` | `cortex.Update` |
| `tombstone` | `cortex.Tombstone` |
| `add_edge` | `cortex.AddEdge` |
| `remove_edge` | `cortex.RemoveEdge` |
| `embed` | Async embedding worker — after `vec/meta` + `Head.EmbeddingRef` are staged |
| `compact` | `cortex.Compact` — checkpoint committed |
| `update_head` | `cortex.UpdateHead` — Head-only mutation |
| `find_late` | `cortex.Find` with `LateBinding=true` — D13 audit hook |
| `scope_violation` | Sub-agent read/write outside its `CortexScope` |
| `attest` | `cortex.Attest` — intent outcome + cited memory IDs |
| `learn_weights` | Always immediately follows `attest` — EMA weight delta |
| `gc` | Future version GC sweep |
| `migration` | Future schema migration step |

---

## Iterating the journal

```go
err = s.IterJournal(fromSeq, func(e *journal.Entry) error {
    // process e
    return nil
})
```

Used by the embedding worker, replay rebuild, and audit tooling. The gap-free invariant means every seq from 0 to `s.NextSeq()-1` has a corresponding entry.

---

## Modifying the store or journal

| What to change | Where |
|---|---|
| Add a new key namespace | `keys/keys.go` — new `Prefix*` var + constructor; `replay/drop.go` — add to `derivedPrefixes` if derived |
| Add a new journal Kind | `journal/journal.go` — new `Kind` const; add payload type; update `replay/rebuild.go` to re-apply on rebuild |
| Change journal encoding | `journal/journal.go` — bump `LeafDomain` version string; all existing journals become unverifiable without migration |
| Tune Pebble options | `store/store.go` — `Options.PebbleOptions` |
