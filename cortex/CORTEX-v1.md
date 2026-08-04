# `matrix/cortex` — Phases 1–9

Per-actor Pebble database, typed memory taxonomy, write API, typed `Find`
query engine with secondary index (`idx/tag`), salience-cold ranking,
auto-rendered short/medium forms with budget-aware Find rendering, an
async embedding worker + pure-Go HNSW vector index powering `Find Near` /
`Find NearURI`, and typed edges + bounded BFS graph traversal powering
`Find From` / `Find Follow`.

Implements steps 1–9 of the impl order in `research/04-cortex.md §18`:

> 1. Pebble shell — open per-actor DB, basic key encoding, journal writes.
> 2. MemoryHead/Version + write API — `Write`, `Update`, `Tombstone` with batch atomicity.
> 3. Predicate indexes + Resolve + typed `Find`.
> 4. Salience cache + auto-form generation + BudgetTokens render.
> 5. Vector embedding worker + HNSW + `Find Near`.
> 6. Edge writes (forward + reverse atomic) + `Find From / Follow`.
> 7. Snapshots + journal MMR + per-namespace SMTs + manifest schema.
> 8. Phase-1 cold-start `Context` composer + `idx/frame` + `idx/actor_obj`.
> 9. `Compact` (summarize-and-link) — Phase 4 budget-aware compaction + journal checkpoints.

Still landing: sub-agent scoping with Merkle proofs, replay harness, EMA
salience-weight learning.

## Layout

```
cortex/
├── go.mod
├── cortex.go                 # §11–12 facade: Write / Resolve / Update / Tombstone / Find / ListByType
├── cortex_test.go
├── keys/                     # §2 key encoding (m/, mv/, e/, j/, idx/, snap/, …)
│   ├── keys.go
│   └── keys_test.go
├── journal/                  # §7.1 / §11.1 entry shape + canonical CBOR + leaf hash
│   ├── journal.go
│   └── journal_test.go
├── memory/                   # §3–§4 typed memory taxonomy + validation
│   ├── types.go              # 9 Type codes, Head, Version, Forms, Tombstone, Provenance
│   ├── data.go               # IdentityData, FactData, ..., PatternData (9 schemas)
│   ├── codec.go              # canonical CBOR + HashVersion (sha256 over body)
│   ├── validate.go           # write-time schema validation (§4.3)
│   └── *_test.go
├── store/                    # per-actor Pebble DB + atomic write batch
│   ├── store.go              # Open, Get, IterJournal, PrefixIter
│   ├── writebatch.go         # BeginWrite → atomic journal+head+version+idx commit
│   └── store_test.go
├── salience/                 # §8 cold-weight salience cache (no learning yet)
│   ├── salience.go           # Score record, ColdScore formula, Read/Encode/Decode
│   └── salience_test.go
├── forms/                    # §9 auto-form generation (short/medium/full)
│   ├── forms.go              # per-type deterministic Render(Head,Data) → Forms
│   ├── truncate.go           # UTF-8-safe TruncateToTokens (bytes/4 heuristic)
│   └── forms_test.go
├── query/                    # §12 typed Find query engine
│   ├── predicate.go          # Predicate AST: Eq Ne Gt Gte Lt Lte In HasTag Matches And Or Not
│   ├── eval.go               # field resolver + scalar comparator
│   ├── find.go               # Run: planner (HNSW Near → idx/tag → idx/type) + executor + audit
│   └── eval_test.go
├── embed/                    # §11.2 / §13.1 — embedder contract + deterministic stub
│   ├── embed.go              # Embedder interface + HashEmbedder (sha256-driven, unit-norm)
│   └── embed_test.go
├── vector/                   # §13.1 — pure-Go HNSW + on-disk persistence
│   ├── vector.go             # NewIndex, Add, Search, Save, Load, MapStore, VectorStore
│   └── vector_test.go
├── embedder.go               # §11.2 async worker: StartEmbedder/StopEmbedder/DrainEmbedder
└── cmd/cortex-shell/         # smoke-test CLI: write/resolve/update/tombstone/list/find (incl -near)
    └── main.go
```

## Spec-vs-implementation note (read this before reviewing)

§1 sketches separate runtime subdirectories per namespace
(`store/memories/<actor>/`, `store/edges/<actor>/`, …). §11.1 requires that
every write batch atomically touch journal + head + version + indexes. Pebble
batches cannot span databases, so **there is exactly one Pebble DB per actor**.
`m/`, `mv/`, `e/`, `j/`, `tomb/`, `snap/`, `idx/`, `salience/`, `vec/meta/` are
**key prefixes inside that single DB** (consistent with §2.2). Per-actor data
directories live at `<root>/<actor>/store/`.

This is the only place the impl knowingly diverges from the literal text of
§1; §2.2 already treats the namespaces as key prefixes, so the divergence is
just a runtime-path clarification.

## Invariants enforced

| Invariant | Where |
|---|---|
| Byte-sort == numeric-sort for all keyed numerics | `keys.PutUint64BE`, `TestJournalKeyOrdering` |
| One Pebble DB per actor, isolated on disk | `store.Open`, `TestPerActorIsolation` |
| Journal seq is per-actor monotonic, gap-free | `store.BeginWrite` + `seqMu`, `TestEveryWriteJournalsExactlyOne` |
| Journal seq persists across reopen | `meta/journal_head`, `TestJournalHeadPersistsAcrossReopen` |
| Canonical CBOR for journal entries (RFC 8949 §4.2.1 deterministic) | `TestEntryEncodeDeterministic` |
| Domain-separated leaf hash | `journal.LeafHash` w/ `"matrix.cortex.journal.v1"` |
| Domain-separated memory hash | `memory.HashVersion` w/ `"matrix.cortex.memory.v1"` + Type byte |
| Actor name cannot escape via `/` | `keys.ValidateNoSeparator`, `TestOpenRejectsBadActor` |
| Every store mutation is journaled (replay invariant) | `store.WriteBatch.Commit` requires prior `AppendJournal` (`ErrBatchNoJournal`) |
| Atomic batch: journal + head + version + idx commit together or not at all | `cortex.Write` / `Update` / `Tombstone`, all use one `BeginWrite` |
| Type ↔ Data consistency at write time (§4.3) | `memory.ValidateMemory`, `TestUpdateRejectsTypeChange` |
| `#latest` rejected at URI parse time (D13 pre-resolution) | `cortex.ParseURI`, `TestParseURIRejectsLatest` |
| Tombstoned memories block subsequent Updates | `cortex.Update`, `TestUpdateRejectsTombstonedMemory` |
| Old versions remain resolvable post-Update (audit trail per §6) | `TestUpdateBumpsVersion` |
| Every Write emits one `idx/type` and N `idx/tag` entries | `TestWriteEmitsIdxTagAndSalience` |
| Every Write seeds a salience cache entry (cold score) | `TestWriteEmitsIdxTagAndSalience` |
| Tombstone collapses salience to 0 (factor inputs preserved) | `TestTombstoneZeroesSalience` |
| `Find` rejects unbounded queries (no Limit AND no BudgetTokens) | `TestFindRefusesUnbounded` |
| `Find` rejects full-store scans (no Type AND no HasTag) | `TestFindRefusesTooBroad` |
| `Find` excludes tombstoned by default; `IncludeTombstoned=true` opts in | `TestFindExcludesTombstonedByDefault` |
| `Find` ranks by salience desc by default (importance shifts ranking) | `TestFindOrdersBySalienceDescByDefault` |
| `Find` with `LateBinding=true` journals exactly one `KindFind` entry | `TestFindLateBindingJournalsKindFind` |
| Compile-time `Find` (`LateBinding=false`) does not journal | `TestFindCompileTimeDoesNotJournal` |

## Running

```bash
cd /root/matrix/cortex
go mod tidy
go test ./...
```

Smoke test the CLI:

```bash
cd /root/matrix/cortex
go build -o /tmp/cortex-shell ./cmd/cortex-shell
S="/tmp/cortex-shell -root /tmp/cortex-data -actor andrew"

# typed writes
$S write Identity '{"name":"Andrew","did":"did:pax:owner"}'
URI=$($S write Preference '{"topic":"tone","polarity":"prefer","strengthval":0.9,"rationale":"terse"}' | awk -F= '{print $2}')

# resolve + update + tombstone
$S resolve "$URI"
URI2=$($S update "$URI" '{"topic":"tone","polarity":"prefer","strengthval":1.0,"rationale":"even terser"}' | awk -F= '{print $2}')
$S tombstone "$URI" "superseded"
$S resolve "$URI2"

# enumerate + journal introspection
$S list Preference
$S dump
$S head
```

## Dependencies

| Package | Purpose | Version |
|---|---|---|
| `github.com/cockroachdb/pebble` | per-actor LSM KV (D17) | v1.1.0 |
| `github.com/fxamacker/cbor/v2` | canonical CBOR for journal entries + future memory hashing | v2.6.0 |
| `github.com/oklog/ulid/v2` | ULID generation at API boundaries (binary in keys) | v2.1.0 |

## Phase 3 surface (now landed)

The §12 read API is wired end-to-end for the typed-predicate path.

`cortex.Find(query.Query)` honours:

- `Type []memory.Type` — single or union; planner does idx/type prefix scans.
- `Where Predicate` — full AST: `Eq Ne Gt Gte Lt Lte In HasTag Matches And Or Not`.
  Field refs resolve over `head.<X> | version.<X> | data.<X>` (per-type whitelist
  in `query/eval.go::dataField`). Unknown fields are false-y for `Eq`/`Ne`/`In`/
  `HasTag` and surface as `ErrFieldUnknown` for ordered comparisons.
- `OrderBy []OrderClause` — fields: `salience` (default desc), `version.created_at`,
  `head.last_updated_at`, `head.declared_importance`. Stable ULID tie-break.
- `Limit` and `Offset` — required: at least one of `Limit` or `BudgetTokens`.
  Unbounded queries return `ErrUnbounded`.
- `IncludeTombstoned bool` — default false; tombstoned memories are filtered
  pre-rank and would always rank at zero per §8.2.
- `LateBinding bool` — D13 anti-pattern audit. When true, `Run` appends a
  `journal.KindFind` entry recording the predicate string, types scanned,
  result count, and tags used.

Planner strategy:

1. If the top-level And-conjunction contains `HasTag` predicates, scan
   `idx/tag/<sha256(tag)[:8]>/...` for each, intersect → candidate IDs.
2. Else scan `idx/type/<t>/...` for each `Type` in the query, union → candidate IDs.
3. Else `ErrTooBroad` (we refuse full-store scans; latency budget is in §15).

Each candidate point-reads `m/<id>` then `mv/<id>/v/<head.CurrentVersion>`,
decodes the typed Data, evaluates `Where`, and pulls salience from
`salience/<id>` (or live-computes a cold score on cache miss).

### Salience (§8)

Cold weights from §8.2 with V (vector) skipped (Phase 5 lights it up):

```
salience(m) = (0.25·R + 0.15·A + 0.30·C + 0.20·D) / 0.90    # vector-weight gating
R(m) = exp(-Δt / 90d)                 # recency
A(m) = log(1+access)/log(1+1000)      # 0 in Phase 3 (no access tracker)
C(m) = log(1+citations)/log(1+1000)   # 0 in Phase 3 (no attestation pipeline)
D(m) = declared_importance / 10
```

Pinned floor 0.7. Tombstoned ranks to 0 (filtered upstream).

Per-actor EMA weight learning (§8.3) lands with Phase 12.

### Indexes maintained on write (§2.3)

- `idx/type/<t:1>/<created:8>/<id:16>` — every Write.
- `idx/tag/<tag_hash:8>/<created:8>/<id:16>` — every Write, one entry per tag.
- `idx/frame` and `idx/actor_obj` are skill-authored indexes per §12.1
  ("frame-relevant tier"); they're not auto-derived from `Head`/`Data` and
  ship with Phase 8 (Context bundle composer) when the writer surface
  exists.
- Tags are immutable across `Update` in Phase 3 (only `Data` is replaced).
  A separate `UpdateHead` surface for tag mutation lands when sub-agent
  scoping (Phase 10) needs it.

## Phase 4 surface (now landed)

Form generation §9 + budget-aware Find render §12.1. Compact (§12 retrieval
phase 4) is **deferred to its own track** — see `IMPL_STATUS` comment in
`/root/matrix/knowledge/matrix.ctx`. Compact has no consumer until Context
bundle composer (impl step 8) lands and depends on snapshot/Merkle from
step 7; building it now would lock a speculative API into the snapshot hash.

### Auto-form generation (§9.1)

`forms.Render(head, data)` produces deterministic short and medium scaffolds
per memory type. Pure function of inputs — no clocks, no PRNG, no locale —
so the rendered bytes are stable across hosts and runs. Forms are written
time-stamped onto **both** `Head.Forms` and `Version.Forms` so Find can
scan-and-render in one Pebble Get per result instead of two.

Token budget enforcement uses the bytes/4 heuristic (`memory.CountTokens`):
`tokens ≈ ceil(utf8_bytes / 4)`. Deterministic, zero-dep, snapshot-hash-stable.
Undercounts a real BPE tokenizer by ~10–20% for English (small built-in
margin). Switching to a real tokenizer is a snapshot-hash-affecting change.

Auto path always fits: `forms.TruncateToTokens` clips to budget with an
ellipsis suffix on a UTF-8-safe boundary. Skill-supplied overrides
(`WriteMeta.FormsOverride=true`) that exceed budget are hard-rejected by
`memory.ValidateMemory` with `ErrFormTooLong` (§9.3).

Per-type templates (see `forms/forms.go`):

| Type | Short scaffold |
|------|----------------|
| Identity | `{name}` or `{name} ({did})` |
| Fact | `{predicate}({subject})={statement}` |
| Preference | `prefers {topic} ({polarity}, strength={s:.2f})` |
| Belief | `{stance} {statement}` |
| Event | `{outcome} {kind} with {counterparty} cost={...}` |
| Goal | `[{status}] {statement}` |
| Constraint | `[{strength}] {polarity} {statement}` |
| Capability | `{subject} can {capability} ({verified?})` |
| Pattern | `{statement} (strength={s:.2f}, coverage={c})` |

Medium adds detail (rationale, evidence counts, durations, horizon dates,
etc.) bounded to 200 tokens.

### Find render + BudgetTokens (§12.1)

`Query.Form` selects which scaffold renders into `Result.Rendered` (parallel-
indexed with `Result.Memories`):

- `FormShort` / `FormMedium` — read straight from persisted `Version.Forms`.
- `FormFull` — live render via `forms.RenderFull` (full is never persisted).
- Unset + `BudgetTokens > 0` — defaults to `FormMedium` per §12.1.

`Query.BudgetTokens` enforces a global cap on the sum of rendered tokens.
When the rendered total exceeds budget, the engine drops the **lowest-
salience entries first regardless of OrderBy** (§12.1: "trimming low-salience
items first"). Surviving entries keep the user's OrderBy. A relief valve
guarantees at least one memory is retained even on a pathologically tight
budget.

`Result.TrimmedByBudget` reports how many were dropped; `Result.RenderedTokens`
is parallel-indexed for caller-side audit.

### Phase 4 invariants

| Invariant | Where enforced |
|-----------|----------------|
| Auto-render is deterministic (same inputs → same bytes) | `forms.Render`, `TestRender_Determinism` |
| Head.Forms == latest Version.Forms after every Write/Update | `cortex.Write`, `cortex.Update`, `TestWriteAutoRendersForms`, `TestUpdateReRendersForms` |
| Auto-rendered forms always fit budget by construction | `forms.TruncateToTokens`, `TestRender_BudgetEnforced` |
| Skill-supplied oversize forms are hard-rejected | `memory.validateForms`, `TestWriteRejectsOversizeOverride` |
| `FormsOverride=true` short-circuits auto and persists override verbatim | `cortex.Write`, `TestWriteHonoursFormsOverride` |
| BudgetTokens trim drops lowest-salience first | `query.trimByBudget`, `TestFindBudgetTokensTrimsLowSalience` |
| BudgetTokens always retains ≥ 1 memory | `query.trimByBudget`, `TestFindBudgetTokensRetainsAtLeastOne` |
| Token counter (bytes/4) is shared across write-time validate and Find render | `memory.CountTokens` consumed by `forms.TruncateToTokens` and `query.renderResults` |

## Phase 5 surface (now landed)

Async embedding pipeline + pure-Go HNSW + `Find Near` / `Find NearURI`.
Writes still complete in one round-trip — the embedder is a separate
goroutine tailing the journal, so embedding-API latency cannot stall
`Cortex.Write`.

### Embedder contract (`cortex/embed`)

```go
type Embedder interface {
    Embed(text string) ([]float32, error)
    Dim() int
    Model() string  // "<name>@<digest>"
}
```

Phase 5 ships `embed.HashEmbedder` — a deterministic sha256-driven stub
that produces unit-normalized 768-dim vectors. **Geometry is not
semantic** (sha256 chaos, not language understanding), so neighbours only
mean something for identical-or-substring text. The contract exists so a
real embedder (nomic-embed-text-v1.5 over HTTP, ONNX, etc.) drops in
behind the same interface without touching cortex.

### HNSW vector index (`cortex/vector`)

Pure-Go implementation of the Malkov & Yashunin HNSW paper
(Algorithms 1–4, "simple" neighbour selection). Single-file binary
persistence; deterministic given a seed; in-memory graph with vectors
borrowed from a `VectorStore` (so the graph file stays compact at 100k+
vertices). Defaults match `research/04-cortex.md §19`: M=16, efC=200,
efS=64.

cgo bindings to usearch / hnswlib are NOT pulled in — phase 5 lock from
sess#5 (matrix.ctx Q1). The `VectorIndex` boundary lets a cgo backend
replace the pure-Go one without touching consumers.

### Async worker

```go
c.StartEmbedder(cortex.EmbedderOptions{
    Embedder:  embed.NewHashEmbedder(),
    IndexPath: "/var/cortex/andrew/indexes/vector/index.hnsw",
})
defer c.StopEmbedder()

// Cortex.Write / Update / Tombstone notify the worker; it
// processes them in journal order and writes vec/meta + Head.EmbeddingRef
// + KindEmbed journal entry in one atomic batch.

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
_ = c.DrainEmbedder(ctx)   // block until embedder catches up to head
```

`StartEmbedder` performs an initial drain so the call returns "ready".
Subsequent writes get embedded asynchronously; tests use `DrainEmbedder`
for deterministic ordering.

### `Find Near` / `Find NearURI`

```go
res, _ := c.Find(query.Query{
    Type:  []memory.Type{memory.TypePreference},
    Near:  "minimal dark UI",   // OR NearURI: &someURI
    Limit: 5,
    NearK: 16,                  // HNSW overshoot factor (default 4*Limit)
})
// res.Memories ordered by distance ascending
// res.Distances[id] reports the HNSW distance (1 - cosine; 0 == identical)
```

`cortex.Find` resolves `Near` (embed text) / `NearURI` (load vec/meta)
into `Query.NearVector` + `Query.NearIndex`, then `query.Run` does HNSW
search and applies `Where` as a post-filter on the K-overshoot candidate
set. Default ordering switches to distance ascending when Near is set;
explicit `OrderBy` overrides.

### Phase 5 invariants

| Invariant | Where enforced |
|-----------|----------------|
| Writes never block on the embedder (async) | `Cortex.Write` returns before `notifyEmbedder` completes; the worker runs in its own goroutine |
| Every embed action journals one `KindEmbed` entry | `processWriteEntry` atomic batch, `TestEmbedderJournalsKindEmbed` |
| `vec/meta/<id>` + `Head.EmbeddingRef` + `meta/embed_vertex_next` + `j/<seq>` commit together | one `BeginWrite` per embed (replay invariant) |
| Embedder is deterministic (replay-friendly) | `embed.HashEmbedder`, `TestHashEmbedderDeterminism` |
| HNSW construction is deterministic (same seed + insertion order → byte-identical file) | `TestDeterministicConstruction` |
| Update reuses vertex id (no unbounded growth across versions) | `processWriteEntry`, `TestEmbedderUpdateReusesVertex` |
| Tombstoned memories never appear in Find Near results | `processTombstoneEntry` + `vector.Tombstone`, `TestFindNearTombstonedSkipped` |
| `Near` without a running embedder fails fast | `cortex.resolveNear`, `TestFindNearWithoutEmbedderErrors` |
| `Where` predicates filter HNSW candidates post-search | `query.Run`, `TestFindNearRespectsWherePostFilter` |
| Index file persisted on StopEmbedder; reloaded on next StartEmbedder | `Index.Save`/`Load`, `TestEmbedderIndexFilePersisted` |

### Spec deviations (Phase 5)

- **Engine.** Spec §13.1 names usearch (preferred) or hnswlib. Phase 5
  ships pure-Go HNSW (sess#5 Q1 lock) to keep the build cgo-free. The
  `VectorIndex` boundary makes a future cgo swap mechanical.
- **Build determinism is single-threaded only.** `vector.Index.Add`
  serialises through `i.mu sync.RWMutex` and the embedder worker is
  single-goroutine, so "same seed + insertion order → byte-identical
  index file" holds today. A parallel-build cgo backend (usearch /
  hnswlib) would lose that property. **Build determinism is not
  load-bearing for replay or chain anchoring** because `vec/` is
  rebuilt-from-canonical by `Cortex.Rebuild` (Phase 11) and vector
  geometry is **not** in `OverallRoot` — only `Head.EmbeddingRef`
  bytes are. Search determinism (same query → same neighbours given
  the same persisted index) IS load-bearing and is preserved by every
  candidate backend. The cgo-swap-is-mechanical claim has this
  footnote: build determinism is gone, search determinism stays.
- **`Head.EmbeddingRef` placement.** Spec §4 lists `EmbeddingRef` on Head;
  invariants_locked treats Head as canonical. We satisfy both: the
  embedder writes Head's `EmbeddingRef` inside the same atomic batch as
  `vec/meta` and `KindEmbed`, so replay reconstructs Head deterministically.

## Phase 6 surface (now landed)

Typed edges + bounded BFS graph traversal — the second half of the
§12 query language (D12: "typed predicates + graph traversal + vector +
salience").

### Edge writes (`cortex.AddEdge` / `RemoveEdge`)

14 byte-tagged edge types per §5: `derived_from`, `supersedes`,
`references`, `contradicts`, `corroborates`, `consents_to`,
`dispatched_to`, `attested_by`, `cited_in`, `tombstones`, `part_of`,
`instance_of`, `caused_by`, `observed_by`. Names exposed via
`memory.ParseEdgeType` / `EdgeType.String`.

```go
c.AddEdge(srcID, memory.EdgeReferences, dstID, cortex.AddEdgeMeta{
    CreatedBy: "andrew",
    Weight:    0.5,
})
c.RemoveEdge(srcID, memory.EdgeReferences, dstID, "obsolete", "andrew")
rec, _ := c.GetEdge(srcID, memory.EdgeReferences, dstID)
```

Atomic batch per write:

```
e/from/<src:16>/<t:1>/<dst:16>   ← canonical CBOR EdgeRecord (same bytes)
e/to/<dst:16>/<t:1>/<src:16>     ← canonical CBOR EdgeRecord (same bytes)
j/<seq>                           ← KindAddEdge / KindRemoveEdge entry
meta/journal_head                 ← seq+1
```

Both directional keys carry the **same** canonical bytes; a hop in either
direction reads consistent metadata in one Get. `RemoveEdge` rewrites
both records with `Tombstoned=true` rather than deleting — the audit
trail lives in two places (the e/ record itself plus the
`KindRemoveEdge` journal entry). `AddEdge` on a tombstoned edge revives
it (rewrites `Tombstoned=false`), so replay of an out-of-order
remove+add sequence still reproduces live state byte-identically.

`AddEdge` is idempotent on a live `(src,type,dst)` — no second journal
entry. `RemoveEdge` is idempotent on missing or already-tombstoned edges.

### `Find From / Follow`

```go
res, _ := c.Find(query.Query{
    From:  &uri,
    Limit: 10,
    Follow: &query.EdgeExpr{
        Types:     []memory.EdgeType{memory.EdgeReferences},
        Direction: query.DirOut,
        MinHops:   1,
        MaxHops:   2,
        IncludeTombstoned: false,
    },
    Where: query.Gt{Field: "head.declared_importance", Value: 5},
})
// res.Memories ordered by hop ascending (default when From set)
// res.Hops[id]  reports BFS shortest-path hop count
```

Planner runs a hop-bounded BFS from the resolved `From` vertex along
edges matching `Follow`. Visited IDs at hop ∈ [MinHops, MaxHops] are
candidates; the start vertex is excluded by construction. Cycles
terminate because every visited ID is marked before its neighbours
expand. `Where` applies as a post-filter; type/tombstone filters apply
as well. Default ordering switches to hop-asc; explicit `OrderBy`
overrides.

`MaxHopsCap = 6` is a hard ceiling (mirrors the skill-composition depth
cap from `research/05-skills-and-tools.md §S1`); requesting more fails
fast at validation.

Direction modes:

- `DirOut` — walk `e/from/<src>/…`
- `DirIn`  — walk `e/to/<dst>/…`
- `DirBoth` — union both, dedupe by neighbour ID before expanding

`Follow` may be nil with `From` set; default is "1 hop out, any edge
type, live only". `Follow` without `From` returns `ErrUnsupported`.

### CLI

```bash
$S add-edge "$A" references "$B"
$S remove-edge "$A" references "$B" "obsolete"
$S list-edges "$A" out               # also: in, both
$S find Preference -from "$A" -follow 'references:out:1..2'
$S find Preference -from "$A" -follow 'references,corroborates:both:1..3'
```

`-follow` parser: `<types>:<dir>:<min..max>`. Empty type list means
"any". Direction default `out`. Hop range default `1..1`.

### Phase 6 invariants

| Invariant | Where enforced |
|-----------|----------------|
| Forward and reverse edge keys hold the same canonical bytes | `cortex.AddEdge`/`RemoveEdge`, `TestAddEdgeWritesForwardAndReverse` |
| Edge writes atomic with the journal entry (§11.1) | one `BeginWrite` per `AddEdge` / `RemoveEdge`, `ErrBatchNoJournal` |
| `AddEdge` idempotent on a live `(src,type,dst)` (no second journal entry) | `TestAddEdgeIdempotent` |
| `AddEdge` on a tombstoned edge revives it (no orphaned tombstones) | `TestAddEdgeRevivesTombstoned` |
| `RemoveEdge` is soft — keys remain, `Tombstoned=true` set on both directions | `TestRemoveEdgeMarksTombstoned` |
| `RemoveEdge` idempotent on missing / already-tombstoned | `TestRemoveEdgeIdempotent` |
| Self-edges rejected | `memory.ErrSelfEdge`, `TestAddEdgeRejectsSelfAndInvalid` |
| Default traversal skips tombstoned edges; `IncludeTombstoned` opts in | `query.planCandidatesGraph`, `TestFindFromTombstonedEdgeSkipped` |
| BFS terminates on cycles (visited set) | `TestFindFromCycleTerminates` |
| `MaxHops` capped at `MaxHopsCap=6` | `query.validateEdgeExpr`, `TestFindFromMaxHopsCap` |
| The `From` vertex is never in result set | `planCandidatesGraph`, `TestFindFromExcludesAnchor` |
| Hop count is BFS shortest path (no double-counting via cycles) | `TestFindFromCycleTerminates` (hop=1, not 3) |
| `Follow` without `From` returns `ErrUnsupported` | `query.Run`, `TestFindRefusesUnsupported` |
| Default ordering when `From` set is `OrderHop` ascending; explicit `OrderBy` overrides | `query.orderResults`, `TestFindFromMultiHopOut` |

### Spec deviations (Phase 6)

- **Tombstoned semantics.** §5 says "RemoveEdge tombstoned, not deleted
  (audit trail)" without specifying *where* the tombstone lives. We
  rewrite both directional keys with `EdgeRecord.Tombstoned=true` and
  also journal a `KindRemoveEdge` entry. Both forms cover the audit
  case (point lookup vs replay) and replay produces byte-identical
  state.
- **EdgeRecord.Data.** §5's `Data json.RawMessage` is implemented as
  opaque `[]byte` carried through canonical CBOR. Edge-type-specific
  schemas (e.g. structured `contradicts` payloads) are deferred until a
  skill needs them.

## Phase 7 surface (now landed)

Snapshots and Merkle layout (research/04-cortex.md §7).

### What's new

- `matrix/cortex/snapshot/` package — pure, dep-free implementation of
  the journal MMR, the per-namespace SMT-256 with empty-subtree
  compression, and the SnapshotManifest persistence + OverallRoot
  computation. ~33 unit tests cover MMR cascading, SMT order-
  independence, membership AND non-membership proof verification.
- `Cortex.Snapshot(reason) → *Manifest` — pull-driven snapshot capture.
  Triggers per §7.4 ("compile" / "attest" / "periodic" / "explicit") are
  ops metadata, NOT inputs to OverallRoot.
- `Cortex.OverallRoot() → [32]byte` — the seed input for compiler
  determinism (D11). `cortex_snapshot_hash = sha256(OverallRootDomain ||
  schema_v1 || journal_root || ns_count(2B) || lp(name)||state_root for
  each ns sorted)`.
- `store.JournalHook` — installed once in `cortex.New`, the hook makes
  every `j/<seq>` write append exactly one MMR leaf inside the same
  atomic Pebble batch. `Cortex.Write/Update/Tombstone` and
  `AddEdge/RemoveEdge` plus the embedder additionally stage SMT updates
  for the namespace they touch.
- CLI subcommands: `snapshot [reason]` / `dump-snapshot <seq>` /
  `overall-root` / `prove <uri>`. `prove` self-verifies the SMT
  membership/non-membership proof against the current memories root.

### What's anchored

| Namespace | SMT key | SMT value | Updated by |
|---|---|---|---|
| `memories` | `sha256(domain.memories.v1 ‖ id[16])` | `sha256(domain.value.v1 ‖ canonical CBOR Head)` | `Write`, `Update`, `Tombstone`, embedder |
| `edges` (forward only) | `sha256(domain.edges.v1 ‖ src[16] ‖ type[1] ‖ dst[16])` | `sha256(domain.value.v1 ‖ canonical CBOR EdgeRecord)` | `AddEdge`, `RemoveEdge` |

Tombstones folded into parent canonical bytes (Head.Tombstoned,
EdgeRecord.Tombstoned) — see "Spec deviations (Phase 7)" below for why.

### Phase 7 invariants

| Invariant | Where it's tested |
|---|---|
| Every journal entry contributes exactly one MMR leaf (in the same atomic batch) | `TestPhase7JournalLeafCountTracksJournalSeq` |
| `Write` advances JournalRoot AND memories StateRoot AND OverallRoot | `TestPhase7WriteAdvancesMMRAndSMT` |
| `AddEdge` advances edges StateRoot but NOT memories StateRoot | `TestPhase7AddEdgeAdvancesEdgesRoot` |
| `Tombstone` advances memories StateRoot (the Tombstoned bit is in the canonical Head bytes) | `TestPhase7TombstoneAdvancesMemoriesRoot` |
| `RemoveEdge` advances edges StateRoot (Tombstoned bit in EdgeRecord) | `TestPhase7RemoveEdgeAdvancesEdgesRoot` |
| Two cortexes with identical input sequences produce identical OverallRoot | `TestPhase7DeterministicAcrossActors` |
| `Snapshot` is pull-driven and pure — calling it does NOT mutate state | `TestPhase7OverallRootIsPureFunction` |
| Persisted SnapshotManifest CBOR round-trips losslessly | `TestPhase7SnapshotAPIPersists`, `TestManifestEncodingRoundTrip` |
| OverallRoot includes the journal root, both state roots, schema version, and namespace count — but NOT counters | `TestComputeOverallRootCommits*`, `TestComputeOverallRootDeterministic` |
| Membership proofs verify; tampering value or root is rejected | `TestSMTProofRejectsWrongValue`, `TestSMTProofRejectsWrongRoot` |
| Non-membership proofs verify (proves a key is absent from the set) | `TestSMTNonMembershipProofVerifies` |
| MMR root commits to leaf count (same peak structure with different N → different roots) | `TestMMRRootCommitsToLeafCount` |
| MMR + SMT state lives under derived prefixes (`accum/`, `idx/smt/`) — they are rebuildable on replay | structural: `Reset()` on both, used by Phase 11 |

### Spec deviations (Phase 7)

- **Two anchored namespaces, not three.** §7.2 lists `memories_root`,
  `edges_root`, `tombstones_root`. Phase 2 never wrote the `tomb/`
  namespace as a standalone tree; tombstone state lives inside
  `Head.Tombstoned` (and from Phase 6 `EdgeRecord.Tombstoned`). Folding
  tombstones into the parent canonical bytes anchors them exactly once
  (single source of truth) instead of risking two roots disagreeing.
  Recorded in `matrix.ctx phase7_locked_design`.
- **Schema version pinned in OverallRoot.** §7.3 doesn't pin a schema
  byte; we mix `SchemaVersion=1` into OverallRoot so adding a namespace
  at v2 forces an explicit re-anchor instead of silently re-rooting.
- **`SignedBy` is metadata-only.** Cortex never signs (D4: no chain
  coupling above tools/). The runtime / `tools/attest` populates and
  signs the manifest before posting to Paxeer.
- **Counters not in OverallRoot.** Per-namespace memory + edge counts
  ride along the manifest as a replay sanity check but are not hashed
  in. Counter drift from a corrupted `accum/` would otherwise force a
  divergent root before the actual error surfaced.

### Storage cost rough-back

For 100k memories + 100k edges per actor:

- MMR nodes: ~2N = ~400k × 32 B = ~13 MB under `accum/mmr/n/*`
- SMT nodes per namespace: bounded by O(N · 256) but empty-subtree
  compression keeps the realized count near O(N · log₂N). Rough
  estimate: ~1.7M nodes per namespace × 32 B + key overhead ≈ ~80 MB
  per namespace, ~160 MB total under `idx/smt/{memories,edges}/n/*`
- Snapshot manifests: ~250 B each; at 1 per intent + periodic = ~10
  MB / month under `snap/<seq>`

Acceptable. Pebble compaction reclaims tombstoned/superseded nodes as
the SMTs evolve.

### Phase 7 deferrals (next phases)

- **Replay harness with Reset+rebuild assertion** → Phase 11
  (TestReplayProducesIdenticalSnapshotRoots end-to-end test). The
  cross-actor determinism test in Phase 7 already proves SAME inputs →
  SAME output; the Reset+walk-journal half lands with the harness.
- **Multi-proof composition for sub-agent CortexScope** → Phase 10
  (sub-agent Merkle-proof scoping). `SMT.Prove` returns single-key
  proofs today; multi-proof bundling is mechanical.
- **Periodic snapshotter goroutine** (StartSnapshotter / StopSnapshotter
  with EveryN/EveryDuration knobs) → can land as a small follow-up; the
  pull API is sufficient for Phase 7 verification.
- **Anchor envelope** (`tools/attest/anchor` consuming
  `Manifest.SignedBy + Signature` and posting to Paxeer) → tools/attest
  track per IMPL_ORDER step 11.

## Phase 8 surface (now landed)

Cold-start `Context` bundle composer + `idx/frame` + `idx/actor_obj`,
implementing Phase-1 retrieval (`research/03-retrieval-patterns.md §2`
and `research/04-cortex.md §12.1`).

### `Head.Frames []FrameRef` — skill-authored frame annotations

`memory.FrameRef{Verb, ObjKind, ObjRef}` carries the (verb, kind, ref)
tuples a skill claims this memory is relevant for. Verb is the closed
D7 enum (10 values: `find acquire build modify deliver analyze
negotiate schedule monitor delegate`); `ObjKind` is the closed v1 enum
(8 values: `service model agent knowledge intent asset plan
capability`); `ObjRef` is a free-form ≤256-char string. `Validate()`
enforces all three at Write time. `FrameRef.Hash() = sha256(ObjRef)[:16]`
is the canonical 16-byte obj_id used in idx keys.

Frames are immutable across `Update` in Phase 8 (Tags pattern); the
`UpdateHead` surface for mutation lands with Phase 10 sub-agent scoping.

### Auto-emitted secondary indexes (§2.3)

On every successful `Write`, the cortex emits one key per `FrameRef`:

| Index | Key shape | Emitted for |
|---|---|---|
| `idx/frame` | `<verb:1>/<obj_kind:1>/<obj_hash:16>/<id:16>` | ALL memory types |
| `idx/actor_obj` | `<verb:1>/<obj_hash:16>/<created:8>/<id:16>` | `TypeEvent` only |

Both writes participate in the same atomic Pebble batch as the journal
entry + Head + Version + memories-SMT update (§11.1). Frames also
contribute to canonical Head bytes, so any change is anchored under
`memories_root`. Replay drops `idx/*` and walks `j/<seq>` → re-reads
`m/<id>` → re-emits idx keys from `Head.Frames` byte-identically.

### `Cortex.Context(opts ContextOpts) (*Bundle, error)`

The Phase-1 cold-start primitive. Pure read; no journal entries,
`OverallRoot` unchanged across the call.

```go
type ContextOpts struct {
    Verb         memory.Verb            // D7 closed verb (zero = no-verb path)
    Objects      map[string]string      // ObjKind name -> ObjRef string
    BudgetTokens int                    // default 3000, ceiling 4000
    IncludeTiers []Tier                 // default {Pinned, Frame, Outcomes}
    OutcomeLimit int                    // default 3
    Form         query.FormKind         // default FormMedium
}
type Bundle struct {
    Pinned, FrameRelevant, Outcomes []*memory.Memory
    Rendered      map[memory.ID]string  // form rendered per memory
    Tokens        map[memory.ID]int     // CountTokens of each rendered form
    Scores        map[memory.ID]float32 // Pinned-tier floored at 0.7
    ReachableURIs []memory.URI          // budget-trimmed memories, capped 64
    TotalTokens   int
    Trimmed       int
    LatencyMS     int64
    Form          query.FormKind
}
```

API refusal enforced at the **type level**: no `Near` / `NearVector`
field exists on `ContextOpts`. Cold-start with vector recall is
forbidden per `research/03 §8`; eliminating the field at compile time
is the strongest possible enforcement.

#### Tier algorithm

1. **Pinned** — `idx/type/Identity` ∪ `idx/type/Constraint` (where
   `StrengthVal == StrengthHard`) ∪ `idx/type/Goal` (where
   `Status == GoalActive`). Tombstone-filtered. Salience-desc.
2. **Outcomes** — `idx/actor_obj/<verb>/<obj_hash>/` scan per
   `(verb, ref)` tuple in `Objects`; sort created-desc; top
   `OutcomeLimit`. Tombstone-filtered.
3. **FrameRelevant** — `idx/frame/<verb>/<kind>/<obj_hash>/` scan per
   `(verb, kind, ref)` tuple in `Objects`; salience-desc.
4. **Dedup priority** — Pinned > Outcomes > FrameRelevant. An Event
   memory that carries both an `idx/frame` entry and an `idx/actor_obj`
   entry lands in Outcomes (time-ordered) rather than Frame
   (salience-ordered). Matches the research/03 §9 worked-example split.
5. **Pinned floor** — Pinned-tier members carry `salience.PinnedFloor`
   (0.7) in `Bundle.Scores`. Survives tight budgets preferentially.
6. **Budget trim** — Global salience-asc trim across all tiers until
   `Σ tokens ≤ BudgetTokens`. At least one memory always retained
   (Find-engine ≥1 relief-valve convention). Trimmed IDs become
   `ReachableURIs` (capped at 64).

Latency target (`research/03 §2.3`): p50 < 80 ms, hard ceiling < 250 ms.

### Invariants added in Phase 8

| Invariant | Where |
|---|---|
| Every Write emits one `idx/frame` per `FrameRef` (any type) | `cortex.Write`, `TestPhase8WriteEmitsIdxFrameForAllTypes` |
| Every Write emits one `idx/actor_obj` per `FrameRef` iff `h.Type == TypeEvent` | `cortex.Write`, `TestPhase8WriteEventEmitsBothIndexes` |
| `FrameRef.Validate` enforced at Write time | `memory.ValidateMemory`, `TestPhase8WriteRejectsInvalidFrame` |
| `Context` is pure read (no journal entry, no `OverallRoot` change) | `TestPhase8ContextIsPureRead` |
| API refusal: `ContextOpts` has no `Near` field (compile-enforced) | type declaration in `context.go` |
| Pinned tier = Identity ∪ Constraint{Hard} ∪ Goal{Active}, tombstone-filtered | `TestPhase8ContextPinnedTierLoadsIdentityConstraintGoal` |
| Outcomes tier returns top-`OutcomeLimit` Events by created desc | `TestPhase8ContextOutcomesTopN` |
| Frame tier matches per `(verb, kind, ref)` tuples, unioned + deduped | `TestPhase8ContextFrameRelevantByVerbObject` |
| Event-with-frame goes to Outcomes (not Frame) — dedup priority | `TestPhase8ContextEventWithFrameLandsInOutcomes` |
| Tombstoned memories filtered from every tier even though `idx/*` entries persist | `TestPhase8ContextSkipsTombstoned` |
| No-verb path runs only Pinned (Frame + Outcomes silently skipped) | `TestPhase8ContextNoVerbRunsOnlyPinned` |
| Pinned-tier members floor at `salience.PinnedFloor` in `Bundle.Scores` | `TestPhase8ContextPinnedFloorApplied` |
| Global salience-asc trim with ReachableURIs reporting trimmed IDs | `TestPhase8ContextBudgetTrimsLowSalience` |
| Default form is `FormMedium` (research/03 §7) | `TestPhase8ContextRendersMediumByDefault` |
| Bundle composition is deterministic across actors w/ byte-identical inputs | `TestPhase8ContextDeterministicAcrossActors` |
| Unknown ObjKind → `memory.ErrInvalidObjKind` (no silent drop) | `TestPhase8ContextRejectsUnknownObjKind` |

### CLI surface

```
write-frame <Type> <json-body> <verb:kind:ref>...
context [-verb V] [-obj kind:ref]... [-include-tier T]... [-budget N] [-outcome-limit N] [-form F]
```

Smoke test (mirrors `research/03 §9` worked example):

```bash
S="/tmp/cortex-shell -root /tmp/cortex-data -actor andrew"
$S write Identity '{"name":"Andrew"}'
$S write Constraint '{"statement":"no PII","polarity":"dont","StrengthVal":"hard","source":"user_declared"}'
$S write Goal '{"statement":"ship matrix","status":"active"}'
$S write-frame Preference '{"topic":"precision","polarity":"prefer","StrengthVal":0.8}' acquire:service:gpu_inference
$S write-frame Event '{"kind":"intent_completed","OutcomeVal":"success","summary":"gpu1"}' acquire:service:gpu_inference
$S write-frame Event '{"kind":"intent_completed","OutcomeVal":"failure","summary":"gpu2"}' acquire:service:gpu_inference
$S context -verb acquire -obj service:gpu_inference -budget 3000
# → [pinned] Identity + Constraint{Hard} + Goal{Active}
#   [frame_relevant] Preference(precision)
#   [outcomes] 2 Events (most-recent first)
```

### Phase 8 deferrals (next phases)

- **`UpdateHead`** for mutating `Tags` and `Frames` on existing
  memories → Phase 10 (sub-agent scoping needs the same surface).
- **Per-actor tier composition tuning** — the spec's "30/50/20 flexible"
  ratios are honoured as soft hints via the PinnedFloor mechanism;
  hard per-tier caps deferred until a real workload demands them.
- **`MaxBudgetTokens=4000` is deliberate, not a small-model artifact.**
  Sourced from `research/03-retrieval-patterns.md §2.3` ("Tokens
  returned (auto form): 1.5–3k; ceiling 4k"). The framing is "cold-
  start fits in 4k regardless of model size" — the rest of any
  frontier-model context window is reserved for the plan + tool I/O,
  not for cortex memory recall. If frontier-mode workloads demand
  larger cold-start bundles, lift the cap by amending research/03
  first; do NOT raise `MaxBudgetTokens` as a code-only change.
- **Arbitrary skill-authored secondary indexes** — `idx/frame` and
  `idx/actor_obj` are auto-derived from `Head.Frames`, but the key
  shape is fixed at `(Verb closed-10, ObjKind closed-8, ObjRef ≤256
  bytes)`. A skill that wants to index by e.g. `(timestamp_bucket,
  geohash)` or `(model_family, parameter_count)` has only two outs
  today: serialise a composite struct into `ObjRef` (defeats prefix
  selectivity since `oh:16` becomes `sha256(struct_bytes)[:16]` with
  no shared prefix), or wait for a `cortex.RegisterIndex(name,
  key_extractor)` API. Bundled with the executor-model selection
  phase (D18). Adding new `ObjKind` values is the lighter-weight
  path — `phase8_locked_design Q1` describes the journaled migration
  posture for that.
- **Token counter / forms tokenizer migration** — `memory.CountTokens`
  is the bytes/4 heuristic and the rendered `Forms.Short` /
  `Forms.Medium` strings persist into canonical Head/Version CBOR
  → directly into `memories_root` → into `OverallRoot`. Switching
  to a real BPE tokenizer therefore re-roots every actor that has
  written a memory. Three options exist (see
  `research/04-cortex.md` followups, plus a TBD Q-lock):
  (a) version the tokenizer in `OverallRoot` like `SchemaVersion`
  so a swap forces explicit re-anchor, (b) commit to bytes/4 forever
  for the snapshot-purpose measure and have a separate "real" token
  count for budget-enforcement only (decouples on-disk hash stability
  from budget accuracy), (c) accept that schema v1 → v2 is a hard
  cutover with explicit migration tooling. Picking now is much
  cheaper than picking after `tools/attest/anchor` posts the first
  `OverallRoot` to Paxeer.

## Phase 9 surface (now landed)

Budget-aware compaction (`research/03-retrieval-patterns.md §5` +
`research/04-cortex.md §11/§12` IMPL_ORDER step 9). The Phase-4
retrieval primitive: summarize-and-link, never truncate.

### `Cortex.Compact(opts CompactOpts) (*CompactResult, error)`

```go
type CompactOpts struct {
    InContext     []*memory.Memory  // §5.3 "what's currently loaded"
    LoadBearing   []memory.URI      // §5.3 URIs the agent must keep full
    BudgetTokens  int               // §5.3 target; default 4000
    IntentID      string            // §5.1 step 3 path component
    StepID        string            // §5.1 step 3 path component
    CheckpointDir string            // optional filesystem mirror dir (A5)
}
type CompactResult struct {
    Kept         []*memory.Memory  // §5.3 kept
    Compacted    []CompactedItem   // §5.3 {ref, short_form, salience}
    SnapshotURI  memory.URI        // matrix://journal/logs/<intent>/<step>
    SnapshotPath string            // filesystem mirror path (empty when off)
}
type CompactedItem struct {
    Ref       memory.URI
    ShortForm string   // Version.Forms.Short (≤50 tok by §7)
    Salience  float32  // salience.Read().Cached (no bump)
}
```

### Locked design (Q&A with Andrew, sess#9)

| Lock | Decision | Citation |
|---|---|---|
| **A1** | Hybrid storage: Pebble canonical at `chk/<intent>/<step>` (canonical CBOR) + optional JSON mirror at `<dir>/<intent>/<step>.snapshot` | `research/03 §5.1` step 3 + `research/01 §4.10` |
| **A2** | `KindCompact` journal entry emitted; MMR leaf staged via the `JournalHook` from `cortex.New` | `cortex.go:79`, `journal/journal.go:KindCompact` |
| **A3** | Cortex auto-protects pinned items (`Identity ∪ Constraint{Hard} ∪ Goal{Active}`) found in `InContext` — in addition to caller's `LoadBearing` list | `research/03 §5.1` step 1 (pinned-tier inclusion) + Andrew |
| **A4** | If post-summarization total still > `BudgetTokens`, return `ErrBudgetUnreachable`. No stage-2 drop ("summarize-and-link, never truncate") | `matrix.ctx` Phase 9 framing + Andrew |
| **A5** | `SnapshotURI` = agent-facing canonical URI; `SnapshotPath` = human-debug filesystem path (empty when `CheckpointDir==""` or mirror write failed) | Andrew |
| **D1** | URI kind = `logs` → `matrix://journal/logs/<intent>/<step>` | Andrew |
| **D2** | Token cost of Kept items = `CountTokens(Version.Forms.Medium)`; matches Phase 8 cold-start default | `context.go:253`, `research/03 §7` |
| **D3** | Filesystem mirror failure is best-effort: log warning, return `SnapshotPath=""`, Pebble side already canonical | Andrew |

### Algorithm (each step cites its source)

1. Validate inputs (`§5.3`).
2. Build effective load-bearing set: `LoadBearing ∪ auto-detect pinned in InContext` (A3, mirrors `context.go:435-515` `tierPinned`).
3. Filter tombstoned items from `InContext` (defensive; matches `context.go:355`).
4. Partition into Kept vs Compactable.
5. For each Compactable item, build `{Ref, ShortForm, Salience}` per `§5.1` step 2 — ShortForm sourced from write-time `Version.Forms.Short` (rendered by `forms.Render`, capped at 50 tok by `§7`).
6. Compute total tokens (D2 lock); if `> BudgetTokens` → `ErrBudgetUnreachable` (A4).
7. Build `CheckpointRecord`, encode canonical CBOR, hash → `CheckpointHash`.
8. Atomic Pebble batch: `chk/<intent>/<step>` + `KindCompact` journal entry. MMR leaf auto-staged.
9. If `CheckpointDir != ""`, write JSON-pretty mirror at `<dir>/<intent>/<step>.snapshot` (best-effort).
10. Return `CompactResult{Kept, Compacted, SnapshotURI, SnapshotPath}`.

Latency target (`§5.4`): p50 < 100 ms, ceiling < 400 ms. All reads are point reads on `m/`, `mv/`, `salience/`; no scans, no vector recall.

### New on-disk state

| Namespace | Key shape | Value |
|---|---|---|
| `chk/` (Phase 9) | `chk/<lpstr intent>/<lpstr step>` | canonical CBOR `CheckpointRecord` |
| `j/` (existing) | `j/<seq:8>` | `KindCompact` entry with `CompactPayload` |

`CompactPayload`:

```go
type CompactPayload struct {
    SchemaVersion  uint8
    IntentID       string
    StepID         string
    BudgetTokens   uint32
    KeptCount      uint32
    CompactedCount uint32
    CheckpointHash [32]byte  // sha256(canonical CheckpointRecord)
}
```

Payload stays small; the heavy `CheckpointRecord` lives at `chk/<intent>/<step>` and is integrity-bound to the journal entry via `CheckpointHash`. Same posture as `KindEmbed` (`journal/journal.go:48-67`).

### Phase 9 invariants

| Invariant | Where enforced |
|---|---|
| Empty `InContext` → `ErrEmptyInContext` | `TestCompactEmptyInContextRejected` |
| Empty `IntentID` → `ErrEmptyIntentID`; empty `StepID` → `ErrEmptyStepID` | `TestCompactRequiresIntentAndStep` |
| `'/'` in `IntentID`/`StepID` → error from `keys.CheckpointKey` | `TestCompactRejectsSlashInIDs` |
| Non-load-bearing items summarized to `{ref, short_form, salience}` stubs | `TestCompactSummarizesNonLoadBearing` |
| Cortex auto-protects pinned items in `InContext` (A3) | `TestCompactAutoProtectsPinnedItems` |
| LoadBearing ∪ Pinned both contribute to Kept; neither blocks the other | `TestCompactLoadBearingAndPinnedUnion` |
| Hard `ErrBudgetUnreachable` when budget unreachable (A4) | `TestCompactBudgetUnreachable` |
| Zero `BudgetTokens` → `DefaultCompactBudgetTokens` (4000) | `TestCompactDefaultBudget` |
| Tombstoned items dropped from both Kept and Compacted | `TestCompactFiltersTombstoned` |
| Single atomic Pebble batch writes `chk/` + `KindCompact` journal entry | `TestCompactWritesPebbleAndJournalAtomically` |
| `CheckpointHash` in payload = sha256 of the persisted `chk/` blob | `TestCompactWritesPebbleAndJournalAtomically` |
| `OverallRoot` advances after Compact (MMR participation) | `TestCompactAdvancesOverallRoot` |
| Filesystem mirror writes pretty JSON at `<dir>/<intent>/<step>.snapshot` (A1) | `TestCompactFilesystemMirror` |
| `CheckpointDir==""` → no mirror, `SnapshotPath=""` (A5) | `TestCompactNoMirrorWhenDirEmpty` |
| Mirror-write failure does NOT fail the call; Pebble side canonical (D3) | `TestCompactMirrorFailureDoesntFailCall` |
| `SnapshotURI = matrix://journal/logs/<intent>/<step>` (D1) | `TestCompactSnapshotURIScheme` |
| `LoadCheckpoint` round-trips the persisted record | `TestCompactLoadCheckpointRoundTrip` |
| Salience is NOT mutated by Compact (research/03 §6 exhaustive trigger list) | `TestCompactDoesNotMutateSalience` |
| Source memories untouched (Head/Version unchanged, not tombstoned) | `TestCompactPreservesSourceMemories` |
| Every `CompactedItem` has `Ref=matrix://cortex/...#v`, `ShortForm`=`Forms.Short`, `Salience>=0` | `TestCompactCompactedItemShape` |
| `KindCompact` payload round-trips CBOR byte-identically; distinct payloads do not collide | `journal.TestCompactPayloadRoundTrip` |
| `chk/` key round-trips lp-string encode/parse; rejects `/` in components | `keys.TestCheckpointKeyRoundTrip` + `TestCheckpointKeyRejectsSlash` |

### CLI surface

```
compact -intent ID -step ID [-budget N] [-dir DIR] [-load URI]... <uri>...
dump-checkpoint <intent_id> <step_id>
```

Smoke:

```bash
S="/tmp/cortex-shell -root /tmp/cortex-data -actor andrew"
U1=$($S write Preference '{"schema_version":1,"topic":"tone","polarity":"prefer","strength":0.8}' | sed 's/^uri=//')
U2=$($S write Preference '{"schema_version":1,"topic":"verbosity","polarity":"prefer","strength":0.8}' | sed 's/^uri=//')
$S compact -intent intent_a -step step_1 -dir ./journal/thoughts -load "$U2" "$U1" "$U2"
# → kept=1 compacted=1
#   snapshot_uri=matrix://journal/logs/intent_a/step_1
#   snapshot_path=./journal/thoughts/intent_a/step_1.snapshot
#     compacted ref=matrix://cortex/Preference/...#1 salience=0.278 short="prefers tone (prefer, ...)"
$S dump-checkpoint intent_a step_1
```

### Phase 9 deferrals

- **Re-entry helper** (`research/03 §5.2`) — `LoadCheckpoint` is the read primitive; assembling a turn's context from the checkpoint + pinned tier + plan-node references is the agent runtime's domain, not the cortex.
- **Reasoning-state payload** — `§5.1` step 3 mentions "reasoning state, working hypotheses, open sub-goals" in a checkpoint. The cortex `CheckpointRecord` covers only what cortex itself authored (kept refs + compacted stubs); reasoning state belongs to the agent's own journal/thoughts payload.
- **`chk/` is detected-only, not provable-from-root.** Compact's
  `KindCompact` journal entry carries `CheckpointHash =
  sha256(canonical CBOR of CheckpointRecord)` and that hash is
  MMR-anchored (so corruption of `chk/<intent>/<step>` is **detectable**
  by recomputing the hash from the persisted bytes and comparing to
  the journal payload). But the `CheckpointRecord` value itself is
  NOT in any namespace SMT — only `memories` and `edges` are anchored
  per `phase7_locked_design Q6`. So an observer holding only
  `OverallRoot` cannot **prove** what the checkpoint contents *should*
  be. This is acceptable for v1 because compacts are recomputable
  from `(opts.InContext, cortex_state @ that_journal_seq)` per
  `compact.go:38-42`, but downstream skills that re-reference
  compacted stubs should be aware that they're trusting a
  hash-detected (not Merkle-proven) artifact. Adding a third SMT
  namespace for `chk/` would anchor a derived artifact and is the
  same anti-pattern Phase 7 Q6 rejected for `tomb/`.

---

## Phase 10 surface (now landed)

Sub-agent **CortexScope** (Merkle-proof scoping) + **UpdateHead** (mutate Head-only fields without bumping Data version). Spec: `research/06-agents.md §7` + `research/04-cortex.md §7.5, §10, §12`.

### Surface

- New package `matrix/cortex/scope/` (~520 LOC):
  - `Scope` canonical CBOR type with `SchemaVersion` byte mixed into signed bytes.
  - `Selector` (`Types/Tags/IDs/Frame`) + `FrameFilter` (`Verb/ObjHashes`).
  - `Sign(s, priv)` / `VerifySignature(s, pub)` over `UnsignedBytes(s)` (CBOR with `Signature=nil`).
  - `KeyResolver` interface + `StaticKeyResolver` for tests/CLI.
  - `Verify(s, snapState, resolver, opts)` runs the full chain: schema → empty-include reject → expiry → signature → snapshot resolvability → multi-proof.
  - `Selector.Matches(head)` + `Scope.Allows(head)` per-candidate filter (pure functions; verification is the caller's job).

- New `snapshot.MultiProof` (~190 LOC):
  - `BuildMultiProofWithValues(namespace, items)` composes one `MembershipProof` per key against the namespace SMT root.
  - `MultiProof.Verify()` self-contained; `MultiProof.VerifyAgainstManifest(m)` cross-checks `mp.Root` with `m.StateRoots[mp.Namespace]`.
  - `snapshot.State.FindSnapshotByRoot(root)` reverse lookup over `snap/<seq>` for scope verification.

- New `journal.KindUpdateHead` + `KindScopeViolation` kinds + canonical CBOR payloads (`UpdateHeadPayload`, `ScopeViolationPayload`).

- `cortex.WithKeyResolver(r)` `Option` injects the agent-pubkey resolver. Calls with a non-nil `Scope` on a Cortex without a resolver fail fast with `ErrNoKeyResolver`.

- Read paths gated by scope:
  - `cortex.Find(q)` honours `q.Scope`. Verifies once; per-candidate `Allows` filter inside `query.Run`. Multi-target read → silent filtering (no per-candidate violation log).
  - `cortex.Context(opts)` honours `opts.Scope`. Same pattern; also caps `BudgetTokens` against `Scope.BudgetTokens`.
  - `cortex.ResolveScoped(uri, s, now)` is the single-target read. Verifies once; per-target `enforceRead` journals a `KindScopeViolation` entry on `Allows` miss (severity:high per `research/06 §7.2`).

- `cortex.Proof(uris, manifest)` returns a `*snapshot.MultiProof` for the URIs against `manifest.StateRoots["memories"]`. Refuses if the current memories root has drifted from the manifest's pinned root (callers must re-Snapshot then re-Proof).

- `cortex.Verify(s, now)` thin wrapper over the scope verifier (read-free).

- New `cortex.UpdateHead(uri, patch, meta)` + `HeadPatch` + `UpdateHeadMeta` (~370 LOC):
  - Mutable fields = `{Tags, Frames, DeclaredImportance, Visibility}`.
  - **No Version bump** (Q5 lock — citations: `research/04 §6.1` + `§85`). `mv/<id>/v/<n>` records and URI version are unchanged.
  - idx/* hard-delete diff (Q6 lock): old idx/tag, idx/frame, idx/actor_obj entries deleted; new ones emitted at current `now`. idx/frame is recomputable from FrameRef so it's a direct `Delete`; idx/tag and idx/actor_obj need a prefix-scan-by-id-suffix lookup to recover the original `created` bytes.
  - Atomic batch: m/ rewrite + idx/* deltas + salience.LastUsed bump + KindUpdateHead journal entry + Phase 7 `StageMemoryUpdate` (memories_root advances).
  - Sub-agent gate (Q7): if `meta.Scope` is set, requires `Scope.Writable=true` AND `Scope.Allows(currentHead)`. Both failures journal a `KindScopeViolation` entry.

- CLI subcommands:
  - `update-head <uri> [-tag T]... [-frame V:K:R]... [-importance N] [-visibility V] [-clear-tags] [-clear-frames]`
  - `dump-scope <file>` — read canonical CBOR Scope bytes, decode, print JSON. Pure offline tool (no key resolver).

### Phase 10 invariants

- Verb vocab and ObjKind vocab remain CLOSED (Phase 8 invariants intact).
- `Scope.SchemaVersion` is mixed into `UnsignedBytes`, so a schema bump invalidates outstanding scopes (signature replays across schemas blocked).
- Empty `Scope.Include` selector matches NOTHING (default-deny). Caller MUST populate at least one criterion. `Verify` rejects empty-include scopes at the boundary so a misconfigured grant doesn't silently behave like default-deny without the caller noticing.
- `Scope.Allows(head)` is a pure function (no journal, no clock). Verification (signature/snapshot/proofs) is per-call once; per-candidate `Allows` is per-result.
- Find/Context filter scope-failed candidates SILENTLY (no per-candidate KindScopeViolation entries). Single-target reads (ResolveScoped) and writes (UpdateHead) DO journal violations.
- `cortex.Proof` requires the current memories root to match the supplied manifest. Drift returns `snapshot.ErrInvalidProof`; caller must re-Snapshot.
- `MultiProof.VerifyAgainstManifest(m)` is the only path that establishes "the proven keys are anchored to the manifest the parent claims". `Verify()` alone is self-contained but doesn't bind to a specific manifest.
- `UpdateHead` does NOT bump `Head.CurrentVersion` and does NOT write a new `mv/<id>/v/<n>` record. The journal entry `KindUpdateHead` is the canonical audit row; `HeadHash` cross-checks the new Head bytes for replay determinism.
- `UpdateHead` rejects tombstoned memories (mirrors `Update`) and returns `ErrNoOp` on an empty patch (no audit row written for no-op patches).
- idx/* are derived projections (replay invariant): `UpdateHead` HARD-DELETES removed idx/tag, idx/frame, idx/actor_obj entries. Soft-tombstone on idx/* would pollute scans (different from edges, which ARE user-facing facts and use soft tombstone).
- Sub-agent `UpdateHead` requires `Scope.Writable=true` (Q7). Default-deny per `research/04 §10.3`.
- Scope-violation journal entries fire even when the violation returns an error (best-effort but the entries DO participate in the MMR via the JournalHook installed in cortex.New, so OverallRoot moves on every violation — making the audit trail tamper-evident).

### Phase 10 deferrals

- **Real DID/registry resolver** — `tools/registry` impl-order step 2 (research/06 §15.1) lands the production resolver that maps `matrix://agent/<did>` to its pubkey. Cortex ships `StaticKeyResolver` for tests + a single CLI session.
- **Auto-revocation on repeated violations** — `research/06 §7.2` says "repeated violations from the same sub-agent trigger automatic dispatch revocation"; cortex emits the journal entries and the agent runtime / `tools/registry` consumer counts them and revokes. Cortex itself does not revoke.
- **Scope-violation rate limiting (open Q-lock).** `KindScopeViolation`
  entries are journaled per call (best-effort, MMR-anchored for
  tamper-evidence). A misbehaving sub-agent can deliberately spam
  scope-disallowed reads/writes to churn the parent's `OverallRoot`,
  forcing snapshot frequency up — and if anchoring is on-chain
  (Paxeer), the parent pays gas for the bad behaviour. Two
  candidate fixes (TBD Q-lock): (δ) token-bucket limiter keyed on
  `(scope.GrantedTo, scope.GrantedBy)` inside `logScopeViolation` —
  drop the journal write past N/sec but still return
  `scope.ErrViolation` to the caller; audit becomes "≥ N
  violations/sec from this scope" not per-call; or (ε) first-
  violation-revokes-scope: journal one entry, then refuse the scope
  thereafter. Cortex is the right layer for this — the runtime
  cannot see individual violations cheaply.
- **Combined Data + Head mutation pattern.** `Cortex.Update(uri, data,
  meta)` only mutates `Data` (writes a new `mv/<id>/v/<n>` + bumps
  `Head.CurrentVersion`); `Cortex.UpdateHead(uri, patch, meta)` only
  mutates Head-only fields (`Tags`, `Frames`, `DeclaredImportance`,
  `Visibility`) without bumping the data version. A callsite that
  wants atomic "retag + fix data" today calls both, producing two
  journal entries (`KindUpdate` + `KindUpdateHead`); an observer
  reading the cortex between them sees an intermediate state. This
  is open-by-design for v1. A `Cortex.UpdateWith(uri, data, headPatch,
  meta)` convenience that issues both inside one `store.BeginWrite`
  with a new `KindUpdateWith` journal kind can land later; defer
  until a callsite needs atomicity.
- **Compressed multi-proof shape** — current MultiProof is a flat list of independent MembershipProofs. RFC9162-style sibling-sharing compression is a future optimisation behind a SchemaVersion bump.
- **Scope-aware `Find.Limit` adjustment** — when scope filters out N of K results, the trim happens AFTER limit was applied to candidates. For very small Limits with very narrow scopes the surviving result count can be < Limit even when more matches exist. Acceptable for v1; revisit if a workload demands oversight.
- **Idx/snap_root reverse index** — `FindSnapshotByRoot` is currently O(N) over `snap/`. Linear at v1 actor counts; `idx/snap_root/<root:32> → seq:8` reverse index can land later if profiling demands.

## Phase 11 surface (now landed)

Phase 11 ships the `cortex.Rebuild` primitive (`replay.Rebuild` under the hood) that implements `research/04-cortex.md` §13.4 verbatim: drop the derived `indexes/` namespaces and re-emit them deterministically from the canonical `store/` state.

Surface:

- `cortex.Rebuild(opts) → *RebuildResult` — drop+rebuild a single actor's derived state in-place. Captures pre-drop `OverallRoot` and asserts equality with post-rebuild — the strongest form of the §13.4 invariant. Refuses to run while the async embedder is active (`ErrEmbedderRunning`).
- `RebuildResult{MemoriesScanned, EdgesScanned, JournalLeavesAppended, JournalSeq, PreOverallRoot, PostOverallRoot, SalienceBumpsApplied}` — counters + roots; `SalienceBumpsApplied` lands in Phase 11.5 (see below).
- `replay.VerifyPreservesRoot(r)` — returns `ErrRootMismatch` iff `r.PreOverallRoot != r.PostOverallRoot`.
- `replay.VerifyAgainstSnapshot(r, manifest)` — the §13.4 literal path; compares `r.PostOverallRoot` to a persisted `snap/<seq>` manifest.

Spec mapping (`research/04-cortex.md` §13.4):

```
store/      KEEP  m/  mv/  e/  j/  tomb/  snap/  chk/
            KEEP  meta/journal_head  meta/snapshot_seq

indexes/    DROP  vec/  idx/  salience/  accum/
            DROP  meta/embed_cursor  meta/embed_vertex_next
```

After drop: walk `m/` to re-emit `idx/type`, `idx/tag`, `idx/frame`, `idx/actor_obj`, and seed `salience/<id>`; walk `e/from/` to stage the edges SMT; walk `j/<seq>` to replay the journal MMR. `vec/*` (HNSW vector index) is intentionally NOT rebuilt by `replay` — re-embedding lives behind the `Embedder` boundary.

### Phase 11 invariants

| Invariant | Where enforced |
|-----------|-----------------|
| Drop+Rebuild produces byte-identical `OverallRoot` | `TestRebuildPreservesOverallRoot` |
| Rebuild is idempotent (running twice = same root) | `TestRebuildIdempotent` |
| Salience cache rebuilds with the supplied clock (recency drift acceptable) | `TestRebuildSalienceCacheRecomputed` |
| Rebuild refuses to run while the embedder is active | `TestRebuildErrEmbedderRunning` |
| Rebuild after `UpdateHead`/`Tombstone`/`Compact`/edge mutations all preserve root | `TestRebuildPreservesRootAfter*` |
| Stale-snapshot verification mismatches (snap from earlier seq) | `TestRebuildVerifyAgainstStaleSnapshotMismatches` |

### Phase 11 deferrals (next phases)

- **Full-DB byte-equality test.** §13.4 only requires `OverallRoot` equality; not spec-required.
- **Phase 11.5 salience instrumentation.** Phase 11 ships `salience.AccessCount` and `salience.Citations` as schema fields with no live code path feeding them. Phase 11.5 (now landed, see below) wires both up so Phase 12 EMA learning has data.

## Phase 11.5 surface (now landed)

Phase 11.5 closes the R8 instrumentation gap: `salience.AccessCount` and `salience.Citations` are now fed by `Find` (late-binding) and `cortex.Attest` respectively. This is the load-bearing data for Phase 12 EMA weight learning.

Surface:

- `cortex.Attest(opts AttestOpts) → *AttestResult` — cortex-side primitive for `intent.attest` (research/04-cortex.md §8.3). On success: bumps `salience.Citations` + `salience.AccessCount` per cited URI. On failure with `Reason ∈ {factual_error, wrong_assumption}`: decrements `salience.Citations` (floored at 0). Atomic batch with one `KindAttest` journal entry.
- `AttestOpts{IntentID, Outcome, Reason, Cited, CreatedBy}` — `Outcome` is `AttestOutcomeSuccess`|`AttestOutcomeFailure`; `Reason` is free-form audit text but only the two spec values trigger Citations decrement.
- `AttestResult{Seq, AffectedIDs, SkippedURIs, CitationsDelta}` — tombstoned/missing/malformed URIs land in `SkippedURIs` rather than failing the batch.
- `query.Run` with `LateBinding=true` now bumps `salience.AccessCount` per returned candidate inside the same atomic batch as the existing `KindFind` audit entry. Compile-time Find (`LateBinding=false`) does NOT bump — compile-time access gets accounted for downstream via `cortex.Attest` cited_uris.
- `journal.KindAttest` + `journal.AttestPayload` + `journal.LateBindingPayload.AccessedIDs[]` — payload shape carries the affected memory IDs so the replay harness can reconstruct salience deterministically.
- `replay.Rebuild` walks `j/` for `KindFind` + `KindAttest` after seeding salience from heads and re-applies `BumpForAccess` / `BumpForCitation` / `DecrementCitation` in journal order. Reported in `RebuildResult.SalienceBumpsApplied`.
- `salience.BumpForAccess(s, now)` / `salience.BumpForCitation(s, now)` / `salience.DecrementCitation(s, now)` — helpers used by both the live and replay paths.

CLI:

- `cortex-shell attest -intent ID -outcome (success|failure) [-reason R] [-by CREATOR] <uri>...` — cortex.Attest from the command line.
- `cortex-shell dump-attest <seq>` — print a `KindAttest` payload at `j/<seq>` as JSON.
- `cortex-shell dump-salience <uri>` — print the cached `Score` as JSON (debug AccessCount / Citations).

### Phase 11.5 invariants

| Invariant | Where enforced |
|-----------|-----------------|
| `cortex.Attest(success)` bumps `Citations` + `AccessCount` per cited URI | `TestAttestSuccessBumpsCitations` |
| `cortex.Attest(failure, factual_error)` / `(failure, wrong_assumption)` decrements `Citations`; other reasons leave it unchanged | `TestAttestFailureDecrementsCitationsOnReasonMatch` |
| `Citations` floors at 0 on decrement (no underflow) | `TestAttestFloorsAtZero` |
| Tombstoned cited URIs land in `SkippedURIs` (silent skip, audit retained) | `TestAttestSkipsTombstoned` |
| Duplicate cited URIs in one attest dedupe to one bump | `TestAttestDeduplicatesCitedURIs` |
| Empty `Cited` / empty `IntentID` reject at the API boundary | `TestAttestRejectsEmptyCited` / `TestAttestRejectsEmptyIntentID` |
| `Find` with `LateBinding=true` bumps `AccessCount` per returned candidate + journals a `KindFind` with `AccessedIDs[]` | `TestLateBindingFindBumpsAccessCount` |
| Compile-time `Find` (`LateBinding=false`) does NOT bump | `TestCompileTimeFindDoesNotBump` |
| `Citations` + `AccessCount` survive drop+Rebuild byte-identical (replay re-applies bumps from journal) | `TestRebuildReappliesAttestSalienceBumps`, `TestRebuildReappliesFindAccessBumps`, `TestRebuildReappliesAttestFailureDecrement` |

### Phase 11.5 deferrals (next phases)

- **Phase 12 EMA weight learning.** Per-actor salience weights (`research/04 §8.3` second + third bullet under both success and failure branches: "Compute the actual ranking that produced this success ..." / "EMA-update the actor's weights..."). Phase 12 consumes the `KindAttest` stream emitted here as its training signal and updates a per-actor weights record (location TBD; not in OverallRoot).
- **`KindUpdateHead` / `KindCompact` access signal.** `UpdateHead` and `Compact` also touch memories but don't bump `AccessCount` today. Spec §8.4 lists triggers exhaustively; if a later phase decides `UpdateHead` should count as access we'll add it to the salience replay walk.
- **`EdgeCitedIn` auto-emission.** `memory/EdgeCitedIn` (0x09) is defined but Phase 11.5 does NOT auto-emit it on attest. Skills that want a graph record of "memory X was cited in plan Y" call `cortex.AddEdge` explicitly. We may revisit if the cited-in edge becomes a load-bearing input to context composition.
- **MCL `intent.attest` envelope wiring.** Phase 11.5 ships the cortex-side primitive; the MCL message-kind handler that validates the signed envelope and resolves `cited` to memory IDs lives in the agent runtime (research/02 §3 — kind 12 of 15).

## Phase 12 surface (now landed)

Phase 12 closes the loop: every `cortex.Attest` now also EMA-updates the per-actor salience weights toward (on success) or away from (on failure with reason ∈ {factual_error, wrong_assumption}) the cited memories' factor profile. `Find` / `Context` / `Compact` ranking now reads the learned weights instead of the §8.2 cold constants. Implements `research/04 §8.3` (success + failure branches: "Compute the actual ranking that produced this success ... EMA-update the actor's weights toward / away from the high-/bad-performing weighting") and §8.4 invalidation triggers.

Surface:

- `cortex/salience/Weights` — per-actor learned weight set: `{WR, WA, WC, WD, WV, UpdatedAt, Updates}`. Canonical CBOR codec via `EncodeWeights` / `DecodeWeights`. `DefaultWeights()` returns the §8.2 cold tuple (0.25, 0.15, 0.30, 0.20, 0.10). Stored at `meta/salience_weights` — a sidecar key, **NOT** part of `OverallRoot`. The journal entries that drive it (`KindLearnWeights`) DO contribute to `journal_root`, so replay reconstructs the same Weights bytes deterministically.
- `cortex/salience/ColdScoreWith(score, weights, now)` — the new live-ranking entry point. `ColdScore(score, now)` is preserved as a thin shim over `ColdScoreWith(&score, DefaultWeights(), now)` for source-compat with cold-table call sites. Every `Find`, `Context`, and `Compact` ranking pass now loads weights via `ReadWeights` once and threads them through.
- `cortex/salience/UpdateWeightsEMA(w, citedScores, alpha, decrementOnFailure, now)` — applies one EMA step (α = `EMARate` = 0.05). Returns `false` (no-op) when `citedScores` is empty or the average factor profile is identically zero. Renormalises so the 5-weight sum stays at 1.0 within float32 tolerance.
- `cortex/journal/KindLearnWeights` (+`LearnWeightsPayload` canonical CBOR codec). Every `cortex.Attest` emits exactly one `KindLearnWeights` entry at `seq = attestSeq + 1` in the same `BeginWrite` batch. The payload captures `SourceSeq` (back-reference to the `KindAttest`), the pre and post weights, the per-factor delta, and `Skipped` (true when the EMA step was a no-op on degenerate input).
- `cortex.AttestResult` extended with `LearnSeq`, `PrevWeights`, `NewWeights`, `WeightsUpdated` so callers can log the weight transition without re-reading the journal.
- `cortex-shell dump-weights` — pretty-prints `meta/salience_weights` as JSON, including the cold-start flag when the key is absent.
- `cortex/replay/rebuildSalienceFromJournal` — now walks `KindLearnWeights` entries in seq order and re-applies them onto `meta/salience_weights` so drop + Rebuild reproduces the same learned weight set byte-exactly (modulo the advisory `UpdatedAt` nanos which are rewritten with rebuild-time wall clock; `WR/WA/WC/WD/WV/Updates` are deterministic).
- `cortex/store/WriteBatch` re-architected to support multiple `AppendJournal` calls per `BeginWrite`. Required because `KindAttest` and `KindLearnWeights` must commit atomically — if either fails, neither lands. Internally: indexed pebble batch + per-call seq allocation + multi-leaf MMR cascade with `BatchedReader` so each leaf hook sees the prior leaves staged in the same batch.

### Phase 12 invariants

| Invariant | Where enforced |
|-----------|----------------|
| `KindLearnWeights` follows `KindAttest` at `seq+1` in the same atomic batch | `TestAttestEmitsKindLearnWeightsSuccess`, `TestAttestEmitsKindLearnWeightsSkippedOnDegenerate` |
| Cold start: `ReadWeights` returns `(DefaultWeights, false, nil)` when `meta/salience_weights` is absent | `TestAttestColdStartLearnsFirstWeights`, `TestRebuildLearnedWeightsColdStartIdempotent` |
| EMA renormalises sum to 1.0 within float32 tolerance | `TestUpdateWeightsEMA_Renormalize` (100 successive updates) |
| Find ranking honours learned weights | `TestFindHonoursLearnedWeights` |
| Drop + Rebuild reproduces same `WR/WA/WC/WD/WV/Updates` byte-exact | `TestRebuildReappliesLearnedWeights` |
| `meta/salience_weights` outside `OverallRoot` — `OverallRoot` byte-stable across Attest+Learn cycles | `TestRebuildPreservesOverallRoot` (full coverage); reproduced live via CLI smoke `attest → snapshot → rebuild → overall-root` byte-identical |
| `WriteBatch` supports multiple `AppendJournal` calls per `BeginWrite` (atomic multi-entry commit) | `TestRebuildReappliesLearnedWeights` (atomic Attest+Learn replays correctly) |
| `EMARate = 0.05` (spec §8.3) — single tunable, NOT per-actor | `salience.EMARate` const |
| Vector weight `WV` left at current value through the 4-factor EMA; full 5-weight sum renormalised at the end | `TestUpdateWeightsEMA_Success` (sum-to-1 invariant) |

### Phase 12 deferrals (next phases)

- **Vector (V) weight gating.** Phase 12 trains the 4 factor weights (R, A, C, D) only; `WV` stays at the prior value and is renormalised at the end of each update. When the spec §8.2 `q.near`-gated vector ranking lands as a first-class Find input (Phase 5+ embeddings), the EMA can extend to a 5-factor profile.
- **Rate limiting on `cortex.Attest`.** Spec §8.3 doesn't constrain attest frequency; nothing in cortex throttles `Attest` calls today. If a malicious or buggy actor floods Attest with the same cited URI, weights drift without bound (modulo renormalisation). Out of scope for v1.
- **Recency-drift recompute (§8.4).** Spec §8.4 lists "weights changed" as a salience cache invalidation trigger. Today Phase 12 reads weights on every `Find`/`Context`/`Compact` so the live ranking is always up-to-date, but the cached `Score.Cached` value is NOT recomputed when weights change. Bumpers continue to use the cold-weight `ColdScore` for `sc.Cached` (Q4-locked design: `sc.Cached` is debug-only). If/when `sc.Cached` ever becomes load-bearing the recompute trigger lands here.
- **Per-actor `EMARate` tuning.** §8.3 sets one global α = 0.05. A future phase may want per-actor α (e.g. faster learning for new actors, slower for senior agents). Schema reserves the field; tuning policy not yet defined.
