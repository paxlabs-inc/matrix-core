# Write API

Package `matrix/cortex` exposes four mutating primitives: `Write`, `Update`, `Tombstone`, and `UpdateHead`. Every call commits an atomic Pebble batch containing the canonical record changes, all affected index keys, a salience update, and a journal entry. Either everything commits or nothing does.

Source files: `cortex/cortex.go`, `cortex/update_head.go`.

---

## Design decisions

**Atomicity is non-negotiable.** Every write uses a single `store.BeginWrite` → `AppendJournal` → `Commit` batch. There is no path that mutates a store key without a corresponding journal entry; `Commit` enforces this via `ErrBatchNoJournal`.

**Forms are rendered at write time.** `forms.Render` runs inside `Write` and `Update` before the batch commits. Short and medium forms are persisted alongside the data. This pays a one-time cost at write time so all subsequent reads are zero-recompute.

**Version bumps are for Data changes only.** `UpdateHead` rewrites mutable Head fields — Tags, Frames, DeclaredImportance, Visibility — without incrementing the version counter. The URI stays `matrix://cortex/<type>/<id>#<n>` where `n` is unchanged. A new Data version requires `Update`.

---

## Write

Inserts a new memory at version 1. Returns `matrix://cortex/<type>/<id>#1`.

```go
uri, err := c.Write(
    memory.Head{
        ActorScope:         "andrew",
        Visibility:         memory.VisPrivate,
        Tags:               []memory.Tag{"onchain", "paxeer"},
        DeclaredImportance: 7,
    },
    memory.FactData{
        Subject:   "matrix://tool/paxeer-net/chain_info@0.1.0",
        Predicate: "latest_block",
        Statement: "12345678",
        Source:    memory.SourceObserved,
    },
    cortex.WriteMeta{
        CreatedBy:  "paxeer-assistant",
        Confidence: 1.0,
        Provenance: memory.Provenance{Kind: "tool_call"},
    },
)
```

### What the batch contains

```
m/<id>                  ← canonical CBOR Head (v=1, Forms rendered)
mv/<id>/v/1             ← canonical CBOR Version (Data, Hash, Forms)
idx/type/<t>/<ts>/<id>  ← type index entry
idx/tag/<h>/<ts>/<id>   ← one entry per tag
idx/frame/<…>/<id>      ← one entry per FrameRef on the Head
idx/actor_obj/<…>/<id>  ← one entry per FrameRef if TypeEvent
salience/<id>           ← cold score seeded from DeclaredImportance
j/<seq>                 ← KindWrite journal entry
accum/mmr/…             ← MMR leaf (via JournalHook)
idx/smt/memories/…      ← memories SMT update (via StageMemoryUpdate)
```

### URI scheme

```
matrix://cortex/<Type>/<crockford-ulid>#<version>
```

`ParseURI` rejects `#latest` (D13). All version references must be pinned.

---

## Update

Creates a new Data version on an existing memory. Increments `CurrentVersion`. Returns `matrix://cortex/<type>/<id>#<n+1>`.

```go
newURI, err := c.Update(existingURI, newFactData, meta)
```

- Rejected if the memory is tombstoned (`ErrTombstoned`).
- Type of `newData` must match the URI type (`ErrTypeDataMismatch`).
- Old version records remain intact and resolvable forever — audit trail per §6.
- Forms are re-rendered against the new Data.
- Salience `LastUsed` is bumped.

The URI passed to `Update` may reference any version number — only the ID is used to locate the head.

---

## Tombstone

Soft-deletes a memory. Idempotent.

```go
err := c.Tombstone(uri, "superseded by v2", "paxeer-assistant")
```

The `MemoryHead` is rewritten with `Tombstoned` set. Version records are NOT deleted — they remain readable for audit via `Resolve(uri)`. A `tomb/<id>` marker is written as a fast existence probe.

Salience is collapsed to 0 (factor inputs are preserved so an un-tombstone path could recompute correctly). The memories SMT root advances to reflect the soft-delete.

---

## UpdateHead

Rewrites mutable Head fields without creating a new Data version. Returns the unchanged URI.

```go
tags := []memory.Tag{"audited", "onchain"}
newURI, err := c.UpdateHead(
    uri,
    cortex.HeadPatch{
        Tags:               &tags,
        DeclaredImportance: &importance,
    },
    cortex.UpdateHeadMeta{CreatedBy: "auditor"},
)
```

`HeadPatch` uses pointer fields — `nil` means "no change", non-nil means "replace wholesale". To remove all tags, pass `&[]memory.Tag{}`.

### Mutable fields

| Field | Notes |
|---|---|
| `Tags` | Pointer semantics; immutable in old versions |
| `Frames` | Pointer semantics; idx/frame entries updated atomically |
| `DeclaredImportance` | 0..10; affects salience.D factor |
| `Visibility` | Must be a valid `Visibility` value |

### idx-key mechanics

`UpdateHead` computes the diff of old vs new Tags and Frames and surgically inserts/removes the matching index keys in the same atomic batch:

- `idx/type/*` — never touched (type is immutable)
- `idx/tag/*` — scan for old tag keys (shape includes creation timestamp); hard-delete; insert new at current `now`
- `idx/frame/*` — direct delete by reconstruction (no timestamp component); insert new
- `idx/actor_obj/*` — scan-and-delete for Event-typed memories; insert new

### Scope gating

Sub-agent `UpdateHead` requires `Scope.Writable=true` (Phase 10 Q7). If the scope is non-nil but not writable, `ErrNotWritable` is returned and a `KindScopeViolation` entry is journaled.

---

## URI parsing and construction

```go
uri := cortex.BuildURI(memory.TypeFact, id, version)
// → "matrix://cortex/Fact/01JXXX...#1"

t, id, version, err := cortex.ParseURI(uri)
// #latest → ErrBadURI (D13)
// version 0 → ErrBadURI
// unknown type → ErrBadURI
```

---

## Error reference

| Error | Cause |
|---|---|
| `memory.ErrEmptyData` | `Write`/`Update` called with nil data |
| `memory.ErrTypeDataMismatch` | `Update` data type doesn't match URI type |
| `memory.ErrTombstoned` | `Update` or `UpdateHead` on a tombstoned memory |
| `memory.ErrNotFound` | `Resolve` can't find the head or version |
| `memory.ErrBadURI` | Malformed URI or `#latest` (D13) |
| `ErrNoOp` | `UpdateHead` called with an empty patch |
| `memory.ErrFormTooLong` | `WriteMeta.FormsOverride` exceeds token budget |

---

## Modifying write behavior

| What to change | Where |
|---|---|
| Auto-form templates | `forms/forms.go` — per-type render functions |
| Validation rules | `memory/validate.go` — `ValidateMemory` |
| WriteMeta shape | `cortex/cortex.go` — `WriteMeta` struct |
| HeadPatch mutable fields | `cortex/update_head.go` — `HeadPatch` struct + apply logic |
| idx-key construction | `cortex/update_head.go` — `findIdxTagKey`, `findIdxActorObjKey` |
