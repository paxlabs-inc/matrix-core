# Edges & Graph

`cortex.AddEdge`, `cortex.RemoveEdge`, and `cortex.GetEdge` manage typed directed edges between memories. `cortex.IterEdgesOut` / `cortex.IterEdgesIn` scan adjacency lists. `query.Run` with `From` + `Follow` performs bounded BFS graph traversal.

Source files: `cortex/edges.go`, `cortex/memory/edge.go`, `cortex/query/graph.go`.

---

## Design decisions

**Bidirectional atomic writes.** Every `AddEdge` writes both the forward key (`e/from/<src>/<t>/<dst>`) and the reverse key (`e/to/<dst>/<t>/<src>`) in a single Pebble batch, alongside a `KindAddEdge` journal entry. You cannot have a forward edge without a reverse edge.

**Soft-delete only.** `RemoveEdge` rewrites both edge records with `Tombstoned=true` rather than deleting them. The audit trail lives in the `e/` records and in the `KindRemoveEdge` journal entry. Traversal and iteration skip tombstoned edges by default; callers that need audit history set `IncludeTombstoned=true`.

**Idempotent semantics.** `AddEdge` on a live existing edge is a no-op. `AddEdge` on a tombstoned edge revives it. `RemoveEdge` on a missing or already-tombstoned edge is a no-op.

---

## EdgeRecord

```go
type EdgeRecord struct {
    Type             EdgeType
    Src              ID
    Dst              ID
    CreatedAt        time.Time
    CreatedBy        string
    Weight           float32
    Data             []byte      // opaque CBOR for edge-type-specific payloads
    Tombstoned       bool
    TombstonedAt     *time.Time
    TombstonedReason string
    TombstonedBy     string
}
```

`Data` is reserved for edge-type-specific payloads — for example, a `contradicts` edge could name the conflicting fields. Empty for all current Phase 6 usages.

---

## AddEdge

```go
err := c.AddEdge(
    srcID,
    memory.EdgeTypeRelatedTo,
    dstID,
    cortex.AddEdgeMeta{
        CreatedBy: "paxeer-assistant",
        Weight:    0.9,
    },
)
```

Self-edges (`src == dst`) are rejected with `ErrSelfEdge`. Zero IDs are rejected.

### Batch contents

```
e/from/<src:16>/<t:1>/<dst:16>  ← canonical CBOR EdgeRecord
e/to/<dst:16>/<t:1>/<src:16>    ← identical bytes (bidirectional mirror)
j/<seq>                         ← KindAddEdge journal entry
idx/smt/edges/…                 ← edges SMT update (forward direction only)
```

The SMT stages only the forward direction to avoid double-anchoring the same fact.

---

## RemoveEdge

```go
err := c.RemoveEdge(srcID, memory.EdgeTypeRelatedTo, dstID, "superseded", "auditor")
```

Rewrites both `e/from/<src>/<t>/<dst>` and `e/to/<dst>/<t>/<src>` with `Tombstoned=true`, `TombstonedReason`, and `TombstonedBy` set. Appends a `KindRemoveEdge` journal entry and stages the edges SMT update.

---

## GetEdge

```go
rec, err := c.GetEdge(srcID, edgeType, dstID)
// err == memory.ErrNotFound if edge was never created
// rec.Tombstoned == true if removed
```

Returns tombstoned edges — callers inspect `rec.Tombstoned`. Use this for audit reads.

---

## Iteration

```go
// Outgoing edges from src
err = c.IterEdgesOut(srcID, cortex.IterEdgesOptions{
    Types:             []memory.EdgeType{memory.EdgeTypeRelatedTo},
    IncludeTombstoned: false,
}, func(rec *memory.EdgeRecord) error {
    // process rec
    return nil
})

// Incoming edges into dst
err = c.IterEdgesIn(dstID, cortex.IterEdgesOptions{}, func(rec *memory.EdgeRecord) error { ... })
```

When exactly one `Types` filter is provided, the scan uses the tighter per-type prefix (`e/from/<src>/<t>`). Otherwise it scans the full anchor prefix and post-filters by type byte. Stop iteration by returning any non-nil error; the iterator treats `errStopIter` as clean stop.

---

## Graph traversal via Find

`query.Find` with `From` and `Follow` performs bounded BFS:

```go
result, err := c.Find(query.Query{
    Type: []memory.Type{memory.TypeFact, memory.TypeEvent},
    From: &startID,
    Follow: &query.EdgeExpr{
        Types:    []memory.EdgeType{memory.EdgeTypeRelatedTo, memory.EdgeTypeDerivedFrom},
        MaxHops:  3,
        Direction: query.DirOut,
    },
    Limit: 20,
    Form:  query.FormMedium,
})
```

BFS visits neighbors in byte-ascending (edge_type, dst) order — same as Pebble's natural iteration order — so results are reproducible across runs for the same store state.

`MaxHopsCap = 6` is the hard upper bound regardless of `Follow.MaxHops`.

`Result.HopDistances[i]` is the hop count of `Result.Memories[i]` from `From`.

---

## Edge types

`EdgeType` is a 1-byte closed enum stored in `e/from` and `e/to` keys. The full set lives in `memory/edge.go`. Representative types:

| Type | Meaning |
|---|---|
| `EdgeTypeRelatedTo` | Generic semantic relationship |
| `EdgeTypeDerivedFrom` | This memory was derived from another |
| `EdgeTypeContradicts` | This memory contradicts another |
| `EdgeTypeSupersedes` | This memory supersedes another |
| `EdgeTypePartOf` | Component relationship |
| `EdgeTypeReferences` | Explicit citation |

---

## Snapshot participation

`AddEdge` and `RemoveEdge` both call `c.snap.StageEdgeUpdate(wb, src, edgeTypeByte, dst, enc)` inside the same atomic batch. This advances the `edges` namespace SMT root, which feeds `OverallRoot`. The reverse `e/to` record is byte-identical to the forward record — only the forward direction is staged into the SMT to avoid double-anchoring the same fact.

---

## Modifying edges

| What to change | Where |
|---|---|
| Add an edge type | `memory/edge.go` — new `EdgeType` const (append only — never reorder) |
| Add edge-type-specific payload | `AddEdgeMeta.Data` field — caller supplies canonical CBOR |
| Change BFS depth cap | `query/find.go` — `MaxHopsCap` constant |
| Change traversal direction defaults | `query/graph.go` — `validateEdgeExpr` default-fill |
