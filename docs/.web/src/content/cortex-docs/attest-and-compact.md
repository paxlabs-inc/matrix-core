# Attest & Compact

`cortex.Attest` records the outcome of a completed intent and feeds that outcome back into the salience of every memory the plan cited. `cortex.Compact` runs budget-aware context compaction — summarizing non-load-bearing memories into short stubs and emitting a journal checkpoint for re-entry.

Source files: `cortex/attest.go`, `cortex/compact.go`.

---

## Attest

### What it does

`Attest` is the cortex-side primitive the MCL `intent.attest` message kind triggers. It:
1. Pre-resolves every cited URI to a live (non-tombstoned) Head.
2. Applies salience bumps to every resolved memory.
3. Recomputes the per-actor learned Weights via EMA.
4. Commits both updates atomically — `KindAttest` at seq=N, `KindLearnWeights` at seq=N+1 — in a single Pebble batch.

```go
result, err := c.Attest(cortex.AttestOpts{
    IntentID:  "01JXXX...",
    Outcome:   cortex.AttestOutcomeSuccess,
    Reason:    "",
    Cited:     []memory.URI{uri1, uri2, uri3},
    CreatedBy: "paxeer-assistant",
})
```

### Outcome semantics

| Outcome | Reason | Citation delta | AccessCount delta |
|---|---|---|---|
| `AttestOutcomeSuccess` | any | +1 | +1 |
| `AttestOutcomeFailure` | `factual_error` or `wrong_assumption` | -1 (floor 0) | 0 |
| `AttestOutcomeFailure` | anything else | 0 | 0 |

Even when the citation delta is 0, `LastUsed` is refreshed and `Cached` recomputed for every affected memory — an attest is treated as a touch.

### AttestOpts

```go
type AttestOpts struct {
    IntentID  string
    Outcome   AttestOutcome
    Reason    string         // free-form; only factual_error/wrong_assumption trigger -1
    Cited     []memory.URI   // up to MaxCitedURIsPerAttest (256)
    CreatedBy string
}
```

`IntentID` is required — empty returns `ErrAttestEmptyIntentID`. An empty `Cited` list returns `ErrEmptyCitations`. The URI list is deduplicated before processing (same memory cited twice = one salience bump, not two).

### AttestResult

```go
type AttestResult struct {
    Seq            uint64          // KindAttest journal seq
    LearnSeq       uint64          // KindLearnWeights seq (always Seq+1)
    AffectedIDs    []memory.ID     // memories whose salience was bumped
    SkippedURIs    []memory.URI    // tombstoned, missing, or malformed — not an error
    CitationsDelta int             // +1 / -1 / 0
    PrevWeights    salience.Weights
    NewWeights     salience.Weights
    WeightsUpdated bool            // false when EMA step was degenerate
}
```

### The two-journal-entry batch

`Attest` commits two journal entries in a single atomic `WriteBatch`:
- **`KindAttest`** at seq=N: contains the post-skip affected IDs, outcome, and reason. The replay harness uses this to re-apply salience bumps deterministically.
- **`KindLearnWeights`** at seq=N+1: contains the before/after weight vector, the EMA alpha, and whether the step was skipped. The replay harness uses this to re-apply the EMA step.

The seq-pairing invariant (`KindAttest` immediately followed by `KindLearnWeights`) is unconditional. Even when the EMA step is a no-op (empty post-skip cited set, or all-zero factor profile), `KindLearnWeights` is still emitted with `Skipped=true` and `NewW* == PrevW*`.

### Rate limiting

`Attest` is protected by a per-(actor, intent_id) token bucket (1/sec, burst 5) to bound the Pebble-sync + dual-journal-entry cost from a malicious sub-agent. Over-rate calls return `ErrAttestRateLimited` without any side effects.

---

## Compact

### What it does

`cortex.Compact` takes a snapshot of what the agent currently has loaded (`InContext`), partitions it into **kept** (load-bearing) and **compacted** (non-load-bearing) memories, replaces compacted items with lightweight `{ref, short_form, salience}` stubs, and emits a journal checkpoint the agent can reload on re-entry.

```go
result, err := c.Compact(cortex.CompactOpts{
    InContext:    currentlyLoadedMemories,
    LoadBearing: []memory.URI{uri_i_must_keep},
    BudgetTokens: 4000,
    IntentID:    "01JXXX...",
    StepID:      "step-7",
    CheckpointDir: "/var/cortex/andrew/journal/thoughts",
})
```

### Load-bearing classification

Cortex auto-protects pinned items (Identity, hard Constraints, active Goals) present in `InContext`, in addition to the caller's explicit `LoadBearing` URI list. The full Kept set = caller-listed URIs ∪ cortex-auto-pinned URIs.

```go
// Caller-explicit
LoadBearing: []memory.URI{uriFact1, uriPattern2}

// Auto-protected (cortex-side) — any Identity, Constraint{Hard}, Goal{Active} in InContext
```

### CompactResult

```go
type CompactResult struct {
    Kept         []*memory.Memory    // full memories (kept at medium form for budget accounting)
    Compacted    []CompactedItem     // stubs: {ref, short_form, salience}
    SnapshotURI  memory.URI          // "matrix://journal/logs/<intent>/<step>"
    SnapshotPath string              // filesystem mirror path (empty if no CheckpointDir or mirror failed)
}

type CompactedItem struct {
    Ref       memory.URI
    ShortForm string    // Version.Forms.Short — ≤50 tokens
    Salience  float32
}
```

### Budget accounting

Kept items count at medium form tokens; compacted stubs count at short form tokens.

```
total = sum(CountTokens(medium)) over Kept + sum(CountTokens(short)) over Compacted
```

If `total > BudgetTokens` after full summarization, `Compact` returns `ErrBudgetUnreachable` — no stage-2 drop, no truncation. "Summarize-and-link, never truncate." The caller must shed some `InContext` items before retrying.

### CheckpointRecord

The canonical checkpoint record is stored at `chk/<intent>/<step>` as canonical deterministic CBOR. An optional human-readable JSON mirror can be written to `<CheckpointDir>/<intent>/<step>.snapshot`.

```go
type CheckpointRecord struct {
    SchemaVersion uint8
    IntentID      string
    StepID        string
    CreatedAt     int64
    BudgetTokens  uint32
    KeptURIs      []memory.URI
    Compacted     []CompactedItem
}
```

The Pebble commit is canonical. The filesystem mirror is best-effort — a failed mirror write does NOT roll back the Pebble commit. `SnapshotPath` is empty when the mirror write fails.

### Loading a checkpoint

```go
rec, err := c.LoadCheckpoint(intentID, stepID)
// err == memory.ErrNotFound if no record exists
```

Used by the re-entry path to rebuild context from the last checkpoint without re-running the full `Compact` partition decision.

### Compact is a pure read (for salience)

`Compact` reads salience scores but does NOT bump them. Compact is not listed in the spec's exhaustive list of salience triggers — calling it repeatedly is a no-op from a salience perspective.

---

## Error reference

| Error | Cause |
|---|---|
| `ErrEmptyCitations` | `Attest` called with empty `Cited` |
| `ErrTooManyCitations` | `Attest` called with > 256 `Cited` URIs |
| `ErrAttestEmptyIntentID` | `Attest` called with empty `IntentID` |
| `ErrInvalidOutcome` | `Attest` outcome is not one of the two closed-enum values |
| `ErrAttestRateLimited` | Over per-(actor, intent_id) rate limit |
| `ErrEmptyInContext` | `Compact` called with an empty or all-tombstoned `InContext` |
| `ErrEmptyIntentID` / `ErrEmptyStepID` | `Compact` missing required IDs |
| `ErrBudgetUnreachable` | Post-summarization total still exceeds `BudgetTokens` |

---

## Modifying attest and compact

| What to change | Where |
|---|---|
| Salience delta rules | `cortex/attest.go` — `citationsDelta` |
| EMA learning rate | `cortex/salience/salience.go` — `EMARate` (also stamped in journal for replay) |
| Attest rate limit | `cortex/ratelimit.go` — `DefaultRateLimits().Attest` / `AttestBurst` |
| Compact Pinned auto-protect criteria | `cortex/compact.go` — `isPinnedInCompact` |
| Checkpoint URI scheme | `cortex/compact.go` — `BuildCheckpointURI` |
| Checkpoint file path scheme | `cortex/compact.go` — `BuildCheckpointFilePath` |
| Budget accounting per tier | `cortex/compact.go` — token counting in step 5 |
