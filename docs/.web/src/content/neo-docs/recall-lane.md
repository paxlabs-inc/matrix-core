# Conversational Recall Lane

Package `matrix/neo/internal/recall` is Neo's conversational recall lane: the read-side that surfaces the most RELEVANT past turns of a (now unbounded) conversation thread — reaching PAST what the live working transcript and the resume seed already hold.

Source file: `neo/internal/recall/recall.go`.

---

## Design decisions

**Relevance over raw recency.** The live transcript (RAM) and the 16-turn resume seed cover RECENT context. Once a turn scrolls out of RAM or is compacted into the lossy summary, the verbatim original is unreachable. This lane pulls a specific, relevant OLD turn back by semantic similarity.

**In-memory, lazy, incremental.** Turns are embedded + cosine-ranked in memory, recomputed lazily from the durable conversation store on demand. Only turns appended since the last call are embedded, so steady-state cost is one embed per new turn.

**Disposable index.** The index never touches cortex or the replay chain. It is a derivable, disposable side-channel — preserving the conversation side-channel invariant.

**Best-effort degradation.** Any embed failure degrades to fewer/no hits rather than erroring. The RAM tier + recent tail still carry the turn.

---

## Recaller

```go
type Recaller struct {
    conv   *conversation.Store
    convID string
    emb    embed.Embedder
    topK   int
    budget int // token ceiling

    mu       sync.Mutex
    cache    []turnVec // embedded turns, thread order
    embedded int       // count already folded into cache
}
```

```go
recaller := recall.New(conv, convID, embedder, topK, budgetTokens)
```

A nil embedder or disabled store yields a safe no-op recaller (`Relevant` returns nil).

---

## Relevant

```go
func (r *Recaller) Relevant(ctx context.Context, queryText string) []Hit
```

Returns up to `topK` past turns most similar to `queryText`, ranked by cosine similarity and bounded by the token budget.

```go
type Hit struct {
    Role string // "user" | "assistant"
    Text string // verbatim turn text
}
```

### Algorithm

1. **Refresh** — embed any turns appended since the last call
2. **Embed query** — `emb.Embed(queryText)`
3. **Score all turns** — cosine similarity between query vector and each turn vector
4. **Sort** — descending by score
5. **Budget** — accumulate turns until `topK` or token budget exhausted (always returns ≥1 if any exist)

### Deduplication

The caller (agent) is expected to drop any hit already present in the live transcript. This lives at the agent boundary because the recaller doesn't know what's currently in the working window.

---

## Integration

Each session gets its own recaller:

```go
var recaller agent.ConvRecaller
if e.conv.Enabled() && e.pager != nil {
    if emb := e.pager.Embedder(); emb != nil {
        recaller = recall.New(e.conv, convID, emb, cfg.RecallTopK, cfg.RecallBudgetTokens)
    }
}

s.agent = agent.New(agent.Options{
    // ...
    Recaller: recaller,
})
```

The agent injects recalled turns into the system block via `renderRecall()`, deduped against the live transcript:

```
Relevant earlier in this conversation (the live exchange below is more current — it wins on any conflict):
- User: how do I deploy an ERC-20
- Neo: call paxeer-net deploy_token...
```

---

## Modifying recall

| What to change | Where |
|---|---|
| Default topK | `recall/recall.go` — constructor default |
| Default budget | `recall/recall.go` — constructor default |
| Similarity metric | `recall/recall.go` — `embed.Cosine()` |
| Budget heuristic | `recall/recall.go` — `(len(text) + 3) / 4` |
