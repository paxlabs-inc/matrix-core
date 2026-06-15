# Context Bundle

`cortex.Context` is the cold-start memory composer — the entry point an agent calls to hydrate its working memory at the beginning of a task. It returns a structured `Bundle` carrying memories from three tiers, ranked by salience, rendered to a token budget, with overflow as `ReachableURIs` the agent can lazily resolve later.

Source file: `cortex/context.go`.

---

## Design decisions

**No vector search at cold start.** `ContextOpts` has no `Near` / `NearVector` field. Cold-start with vector recall is a forbidden combination at the API level — eliminating the field at compile time is the strongest possible enforcement. Vector recall requires `Find Near`.

**Pure read path.** `Context` emits no journal entries, no salience bumps, no index writes. The `OverallRoot` is identical before and after a `Context` call.

**Three tiers, flexible ratios.** All three tiers are fully loaded, globally trimmed to `BudgetTokens` by salience. The "30% pinned, 50% frame-relevant, 20% outcomes" spec ratio is implemented as a salience floor on Pinned-tier members — they survive budget pressure preferentially, not via hard per-tier token quotas.

**Dedup priority: Pinned > Outcomes > FrameRelevant.** A memory that appears in multiple tiers lands in the earlier-priority tier only.

---

## Tiers

### TierPinned

The identity + hard rules + active goals tier.

Members:
- Every `TypeIdentity` memory
- Every `TypeConstraint` with `StrengthVal == StrengthHard`
- Every `TypeGoal` with `Status == GoalActive`

Pinned members receive a salience floor of `0.7` (`salience.PinnedFloor`) so they survive budget pressure even if their raw score is low.

### TierFrameRelevant

Memories indexed under `idx/frame` for the supplied (verb, object-kind, object-ref) tuples. Ordered by salience descending.

The Frame-relevant tier scans `idx/frame/<verb>/<obj_kind>/<hash(ref)>/*` for each entry in `ContextOpts.Objects`. Objects is a `map[string]string` of ObjKind name → ref string. Unknown kind names return `ErrInvalidObjKind`.

### TierOutcomes

Prior Event memories indexed under `idx/actor_obj` for the same (verb, object-ref) tuples. Ordered by creation timestamp descending (most recent first), salience descending as tiebreak. Capped at `OutcomeLimit` (default 3).

Only `TypeEvent` memories appear in this tier — the spec describes Outcomes as "1–3 prior similar intents and their outcomes".

---

## Calling Context

```go
bundle, err := c.Context(cortex.ContextOpts{
    Verb: memory.VerbBuild,
    Objects: map[string]string{
        "token":   "matrix://token/MTX@0.1.0",
        "address": "0xB73C043506bEFD3351B3188f4CB8aE338900Ae1d",
    },
    BudgetTokens: 3000,
    Form:         query.FormMedium,
})
```

`Verb` is required to activate the Frame-relevant and Outcomes tiers. If `Verb` is zero (the zero value of `memory.Verb`) only `TierPinned` runs — Frame and Outcomes return empty.

### ContextOpts

```go
type ContextOpts struct {
    Verb         memory.Verb          // D7 verb; zero = Pinned-only mode
    Objects      map[string]string    // kind name → ref string
    BudgetTokens int                  // token cap; default 3000, max 4000
    IncludeTiers []Tier               // nil = all three tiers
    OutcomeLimit int                  // default 3
    Form         query.FormKind       // default FormMedium
    Scope        *scope.Scope         // optional sub-agent gating
    Now          time.Time            // for scope expiry; zero = time.Now()
}
```

---

## Bundle

```go
type Bundle struct {
    Pinned        []*memory.Memory   // salience desc
    FrameRelevant []*memory.Memory   // salience desc
    Outcomes      []*memory.Memory   // created desc, salience tiebreak

    Rendered      map[memory.ID]string   // rendered form per memory
    Tokens        map[memory.ID]int      // token count per memory (pre-trim)
    Scores        map[memory.ID]float32  // live salience score (with floor for Pinned)

    ReachableURIs []memory.URI       // trimmed-but-scanned IDs, capped at 64
    TotalTokens   int                // post-trim sum
    Trimmed       int                // count dropped by budget enforcement
    LatencyMS     int64              // wall-clock from Context entry to return
    Form          query.FormKind
}
```

`ReachableURIs` are memories that matched a tier scan but were dropped by the budget trim. The agent can `Resolve` them lazily if needed.

---

## Algorithm walkthrough

```
1. Scope verification (once, if Scope non-nil)
2. Normalize opts — clamp BudgetTokens, apply Scope.BudgetTokens cap
3. Parse Objects map → (kind, ref, hash) tuples
4. Load per-actor learned salience Weights

5. Tier scans (sequential):
   a. TierPinned    → Identity + Constraint{Hard} + Goal{Active}
   b. TierOutcomes  → idx/actor_obj scan, top-N created desc
   c. TierFrameRelevant → idx/frame scan, salience desc

6. Cross-tier dedup (Pinned > Outcomes > FrameRelevant)

7. Load Head + Version + salience score for every surviving ID
   - Apply PinnedFloor to Pinned members
   - Apply Scope.Allows per-candidate filter (silently — no violation log)
   - Filter tombstoned memories

8. Render to Form (short/medium from persisted Forms; full live)

9. Global salience-ascending trim to BudgetTokens
   - Always retain at least 1 memory
   - Overflow IDs become ReachableURIs (capped at 64)

10. Split survivors back into per-tier slices, apply per-tier ordering
11. Return Bundle with LatencyMS
```

---

## Token budget

| Constant | Value | Meaning |
|---|---|---|
| `DefaultBudgetTokens` | 3000 | Applied when `BudgetTokens` is zero |
| `MaxBudgetTokens` | 4000 | Upper clamp; requests above this are silently capped |
| `DefaultOutcomeLimit` | 3 | Max Outcomes-tier memories |
| `MaxReachableURIs` | 64 | Max overflow pointers in `Bundle.ReachableURIs` |

---

## Latency target

p50 < 80 ms, hard ceiling < 250 ms. All three tier scans are sequential point-reads and bounded prefix-scans over Pebble (10–20 µs warm). Parallelizing the tiers is a future optimization if profiling demands it.

---

## Modifying Context

| What to change | Where |
|---|---|
| Pinned tier membership criteria | `cortex/context.go` — `tierPinned()` |
| Frame-relevant index scan | `cortex/context.go` — `tierFrameRelevant()` |
| Outcomes tier index scan | `cortex/context.go` — `tierOutcomes()` |
| Budget defaults and caps | `cortex/context.go` — `DefaultBudgetTokens`, `MaxBudgetTokens`, `MaxReachableURIs` |
| Tier dedup priority order | `cortex/context.go` — dedup map construction (change which tier runs first) |
| Pinned salience floor | `cortex/salience` — `PinnedFloor` constant |
| Outcome limit default | `cortex/context.go` — `DefaultOutcomeLimit` |
