# Find Query

Package `matrix/cortex/query` implements the typed `Find` query engine described in §12. `cortex.Find` is a thin facade that resolves near-vectors and delegates to `query.Run`. The engine handles typed predicates, secondary-index candidate selection, salience-ranked ordering, budget-aware rendering, and bounded BFS graph traversal.

Source files: `cortex/query/find.go`, `cortex/query/predicate.go`, `cortex/query/eval.go`, `cortex/query/graph.go`.

---

## Design decisions

**Unbounded queries are rejected.** Every `Find` must set either `Limit` or `BudgetTokens`. No `Limit` AND no `BudgetTokens` → `ErrUnbounded`. This is a load-bearing OOM guard.

**Full-store scans are rejected.** A query with neither a `Type` filter nor a `HasTag` predicate → `ErrTooBroad`. The engine requires at least one narrowing criterion before scanning.

**Salience is the default order.** When no `OrderBy` is specified (and no `Near` is set), results are ranked by the live cold-score descending. OrderBy clauses override this. A ULID-ascending tiebreak ensures deterministic ordering across runs.

**Budget trim drops lowest salience first.** When `BudgetTokens` is set and rendered tokens exceed the budget, entries are dropped in ascending salience order regardless of `OrderBy`. At least one entry is always retained (the "relief valve" convention).

---

## Query struct

```go
type Query struct {
    Type            []memory.Type    // union; at least one required (unless HasTag used)
    Where           Predicate        // optional typed predicate tree
    OrderBy         []OrderClause    // default: salience desc
    Limit           int              // cap on result count
    Offset          int              // for pagination
    BudgetTokens    int              // cap on rendered token sum; requires Form to be set
    Form            FormKind         // "short" | "medium" | "full"
    IncludeTombstoned bool           // default false
    LateBinding     bool             // D13: journal a KindFind entry for audit
    Near            string           // text → vector → HNSW nearest neighbors
    NearURI         *memory.URI      // memory → embedding → HNSW nearest neighbors
    From            *memory.ID       // entry point for graph traversal
    Follow          *EdgeExpr        // BFS edge filter
    Scope           *scope.Scope     // Phase 10: sub-agent read gating
}
```

---

## Planner strategy

`Run` chooses the candidate set before evaluating `Where`:

```
1. If Where contains HasTag predicates reachable through pure And-conjunction:
   → scan idx/tag/<sha256(tag)[:8]>/… for each; INTERSECT candidate IDs

2. Else if Type is set:
   → scan idx/type/<t>/… for each; UNION candidate IDs

3. Else if From is set (graph traversal):
   → BFS from From via Follow edges; collect reachable IDs

4. Else → ErrTooBroad
```

Each candidate ID is then point-read (`m/<id>` + `mv/<id>/v/<n>`), decoded, and evaluated against `Where`. Tombstoned candidates are excluded unless `IncludeTombstoned=true`.

---

## Predicates

The predicate AST is a closed interface — no way to synthesize predicate types outside the package.

| Predicate | Description |
|---|---|
| `Eq{Field, Value}` | Field equals Value |
| `Ne{Field, Value}` | Field does not equal Value |
| `Gt / Gte / Lt / Lte` | Ordered comparison (numeric, time.Time, string lexicographic) |
| `In{Field, Values}` | Field equals any of Values |
| `HasTag{Tag}` | Memory has this tag — also drives the idx/tag planner path |
| `Matches{Field, Pattern}` | Field matches Go regexp pattern |
| `And{Children}` | All children true; empty And = vacuously true |
| `Or{Children}` | Any child true; empty Or = vacuously false |
| `Not{Inner}` | Logical negation |

### Field references

```
head.<name>       -- Head struct fields (id, type, actor_scope, visibility, declared_importance, …)
version.<name>    -- Version fields (created_at, confidence, provenance_kind, …)
data.<name>       -- Type-specific Data fields (subject, predicate, statement, topic, polarity, stance, …)
```

Unknown `data.*` fields are false-y for `Eq`/`Ne`/`In`/`HasTag`; they return `ErrFieldUnknown` for ordered comparisons.

### Example queries

```go
// Find all Fact memories tagged "paxeer" written in the last 24h
result, err := c.Find(query.Query{
    Type:  []memory.Type{memory.TypeFact},
    Where: query.And{Children: []query.Predicate{
        query.HasTag{"paxeer"},
        query.Gt{Field: "version.created_at", Value: time.Now().Add(-24 * time.Hour)},
    }},
    Limit: 20,
    Form:  query.FormMedium,
})

// Find semantically similar memories
result, err := c.Find(query.Query{
    Type:  []memory.Type{memory.TypeFact, memory.TypeEvent},
    Near:  "latest block height",
    Limit: 5,
})
```

---

## Result

```go
type Result struct {
    Memories        []*memory.Memory  // ordered survivors
    Rendered        []string          // parallel-indexed rendered forms (when Form set)
    RenderedTokens  []int             // parallel-indexed token counts (audit)
    TrimmedByBudget int               // how many were dropped by BudgetTokens enforcement
    Scores          []float32         // parallel-indexed salience scores (debug/audit)
    HopDistances    []int             // parallel-indexed hop counts (graph traversal only)
}
```

---

## Ordering

```go
type OrderClause struct {
    Field     OrderField   // "salience" | "version.created_at" | "head.last_updated_at" | "head.declared_importance" | "near.distance" | "hop"
    Direction OrderDirection // "asc" | "desc"
}
```

When `Near`/`NearURI` is active and no explicit `OrderBy` is given, the default order is `near.distance asc` (closest vector first). When `From`/`Follow` is active, the default is `hop asc` (closest in the graph first).

---

## BudgetTokens render + trim

When `BudgetTokens > 0`:

1. `Form` must be set (defaults to `FormMedium` in `Context`; not auto-defaulted in raw `Find` calls).
2. Results are rendered in the engine after ordering.
3. If total rendered tokens exceed `BudgetTokens`, entries are dropped in ascending salience order until the budget is met, regardless of `OrderBy`.
4. At least one entry is always retained.
5. `Result.TrimmedByBudget` reports the count dropped.

This is the same "salience-based trim" used by `Context` — the lowest-signal memories are the first to go.

---

## Graph traversal

When `From` is set, `Run` runs a bounded BFS starting at `*From` along edges matching `Follow`:

```go
type EdgeExpr struct {
    Types             []memory.EdgeType  // empty = any of the 14 types
    MinHops           int                // minimum depth to include (default 1)
    MaxHops           int                // maximum depth to traverse (default 1; hard cap: 6)
    Direction         Direction          // "out" | "in" | "both"
    IncludeTombstoned bool               // surface tombstoned edges in traversal
}
```

BFS visits neighbors in byte-ascending (edge_type, dst-or-src) order — the same order Pebble surfaces them — so traversal results are reproducible across runs. Cycles terminate because every visited ID is marked before its neighbors expand.

`MaxHopsCap = 6` is a hard ceiling regardless of what the caller sets. It mirrors the skill-composition depth cap.

The result IDs from BFS are then filtered through the same `Where` + `Type` + tombstone logic as regular Find candidates. `Result.HopDistances` is populated with the BFS hop count per surviving memory.

---

## LateBinding audit hook

D13: mid-execution `Find` calls are a potential source of non-determinism and must be surfaced for audit.

```go
result, err := c.Find(query.Query{
    Type:        []memory.Type{memory.TypeFact},
    LateBinding: true,    // journals a KindFind entry
    Limit:       10,
})
```

When `LateBinding=true`, `Run` appends a `KindFind` journal entry recording the predicate string, types scanned, tags used, and result count. This does NOT affect the `OverallRoot` in a harmful way — it is an intended audit entry.

Compile-time `Find` (at stage-3 context pre-fetch time) MUST use `LateBinding=false` (the default) so no journal entry is emitted and the snapshot root doesn't move unexpectedly.

---

## Scope gating

When `Query.Scope` is set, `VerifyScope` runs once at `Find` entry. Per-candidate filtering applies `Scope.Allows(&head)` silently — `Find` is a multi-target read and does not journal per-candidate violations (unlike `ResolveScoped` which does).

---

## Modifying query behavior

| What to change | Where |
|---|---|
| Add a predicate type | `query/predicate.go` — implement `Predicate` interface; `query/eval.go` — handle in `evalPredicate` |
| Add a sortable field | `query/find.go` — `OrderField` const + sort comparator in `sortResults` |
| Change BFS depth cap | `query/find.go` — `MaxHopsCap` constant |
| Add a field resolver | `query/eval.go` — `dataField` per-type switch |
| Change unbounded guard | `query/find.go` — `ErrUnbounded` / `ErrTooBroad` checks |
