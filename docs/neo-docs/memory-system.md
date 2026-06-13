# Memory System

Package `matrix/neo/internal/memory` is Neo's memory controller — the "pager" in the frozen spec's RAM/disk/pager model. The context window is RAM (scarce), cortex is disk (durable ground truth), and this package is the controller that pins, page-faults, writes back, and compacts.

Source files: `neo/internal/memory/pager.go`, `neo/internal/memory/embedder.go`, `neo/internal/memory/pattern.go`, `neo/internal/memory/writeback.go`.

---

## Design decisions

**Cortex is ground truth; the window is a cache.** All durable learnings live in cortex. The window is a disposable working set that gets re-faulted every turn. Summaries are re-derivable, not ground truth.

**Two retrieval lanes.** Semantic (HNSW vector search via embedder) + salience-ranked (type-filtered). The embedding worker is async, so a memory written seconds ago is invisible to the vector index. Without the salience lane, a "remember this" → "what do you know?" round trip inside one session would come back empty.

**Anti-overfit guard for procedural patterns.** A candidate pattern must reach `MinPatternSuccesses` (default 3) before it is injected into the window. This prevents one-off successes from being treated as proven recipes.

**Verbatim trust contract.** High-entropy tokens (addresses, tx hashes, IDs, file paths) are copied verbatim in compaction and recall. A paraphrased `0x...` is a corrupted memory (i3).

---

## Pager

```go
type Pager struct {
    cfg      config.Config
    cortex   *cortex.Cortex
    store    *store.Store
    embedder embed.Embedder
    hasEmbedder bool
}
```

Opened per-actor under the shared cortex root:

```go
pager, err := memory.Open(cfg)
```

A failed embedder is non-fatal — retrieval falls back to salience ranking. The embedding worker lazily re-embeds memories whose recorded model differs from the active one, so upgrading from hash → API vectors migrates the brain automatically.

---

## Pinned block

The pinned block is injected every turn. It contains:

1. **Identity** — agent name + DID from cortex identity record
2. **Inviolable rules** — the 6 baked-invariants from the frozen spec
3. **Hard constraints** — user/operator-declared hard-strength constraints from cortex
4. **User profile** — durable facts about the user (name, role, preferences), deduped and bounded to 12 entries
5. **Active goal** — the current conversation's task

```go
func (p *Pager) Pinned(ctx context.Context, goal string) string
```

Bounded by `PinnedBudgetTokens` (default 2000). The caller passes the per-conversation goal so many conversations can share one cortex store safely.

---

## Page-fault retrieval

```go
func (p *Pager) Retrieve(ctx context.Context, queryText string) ([]Snippet, error)
```

Returns top-K relevant cortex records, merged from both lanes:

1. **Semantic lane** — `cortex.Find` with `Near=queryText` (HNSW vector search), when embedder is running
2. **Salience lane** — `cortex.Find` with type filter: Fact, Event, Pattern, Preference, Goal

Deduplicated by URI. Bounded by `RetrievalTopK` (default 8) and `RetrievalBudgetTokens` (default 6000).

### Snippet

```go
type Snippet struct {
    Text string // rendered text for injection
    URI  string // matrix://cortex/... reference
    Type string // memory type name
}
```

---

## Procedural patterns

Procedural memory is reusable how-to knowledge — the nursery for MCL skills.

```go
func (p *Pager) Procedural(ctx context.Context, goal string) ([]Pattern, error)
```

Returns proven patterns whose trigger matches the goal, gated by `MinPatternSuccesses`. Sorted by confidence descending.

### PatternSpec

The structured schema encoded onto cortex's flat Statement field:

```go
type PatternSpec struct {
    Name            string   // human label
    Trigger         string   // when to apply
    Preconditions   []string // check BEFORE applying
    Steps           []string // proven tool sequence
    Gotchas         []string // learned failure modes
    SuccessCriteria []string // verify AFTER applying
}
```

Encoded as `neo.pattern.v1:` + JSON. Legacy plain statements decode as a single freeform step.

### Pattern lifecycle

1. **Experience** — an episodic event ("this worked")
2. **Distill** — background write-back abstracts the sequence into a candidate
3. **Reinforce** — `ReinforcePattern` increments coverage and nudges strength up on each repeat success
4. **Promote** — a proven, rigorous pattern graduates into an authored MCL `.mtx` skill

---

## Write-back helpers

The pager provides typed write methods used by the background consolidation pass:

```go
func (p *Pager) RememberFact(ctx, statement string) (string, error)       // semantic memory
func (p *Pager) RememberUserFact(ctx, statement string) (string, error)    // user profile (pinned)
func (p *Pager) RecordOutcome(ctx, summary string, outcome Outcome, intentRef string) (string, error) // episodic
func (p *Pager) WritePattern(ctx, spec PatternSpec, strength float32, coverage int, derivedFrom []string) (string, error) // procedural
func (p *Pager) ReinforcePattern(ctx, spec PatternSpec, derivedFrom []string) (string, error) // distill+reinforce
```

All write with `SourceObserved` provenance and the actor's scope. Facts use subject `matrix://knowledge/neo` (general) or `matrix://knowledge/user` (profile). User facts are deduped by normalized statement before writing.

---

## Embedding backend

```go
func pickEmbedder(cfg config.Config) embed.Embedder
```

Selection order:

1. **Matrix gateway** — `${GatewayURL}/v1/embeddings` with `MATRIX_GATEWAY_TOKEN` + actor DID. Spend is attributed under slot "neo".
2. **Direct provider** — Fireworks API key (`FIREWORKS_API_KEY`)
3. **Hash fallback** — deterministic hash embedder. Retrieval degrades to pseudo-lexical, but nothing breaks.

A boot-time probe (`probeEmbedder`) issues one tiny embed to verify the backend accepts credentials. Without this, a misconfigured gateway would be selected and every page-fault would return nothing.

---

## Token estimation

```go
func EstimateTokens(s string) int
```

Uses cortex's bytes/4 heuristic. Deterministic and dependency-free. Matches the budget math everywhere.

```go
func truncateTokens(s string, maxTokens int) string
```

Truncates to `maxTokens * 4` bytes, appending a truncation marker.

---

## Modifying memory behavior

| What to change | Where |
|---|---|
| Pinned block composition | `memory/pager.go` — `Pinned()` |
| Inviolable rules | `agent/prompt.go` — `invariantRules` |
| Retrieval type filter | `memory/pager.go` — `Retrieve()` salience lane |
| Pattern schema | `memory/pattern.go` — `PatternSpec` |
| Anti-overfit threshold | `config/config.go` — `MinPatternSuccesses` |
| Embedding backend priority | `memory/embedder.go` — `pickEmbedder()` |
| Write-back fact subject | `memory/writeback.go` — `factSubject` / `userFactSubject` |
