# Salience

Package `matrix/cortex/salience` computes and persists the per-memory ranking signal. Every memory has a `Score` record stored at `salience/<id>` that tracks the factor inputs. The live ranking value is computed on demand by `ColdScoreWith(score, weights, now)` using the actor's current learned `Weights`.

Source file: `cortex/salience/salience.go`.

---

## Design decisions

**Five factors, weighted sum.** Recency (R), Access (A), Citations (C), Declared importance (D), and Vector similarity (V). V is only active when a `Near` query is in progress; without it, the four remaining weights are renormalized to sum to 1.

**`Cached` is a snapshot, not the live ranking.** `sc.Cached` is written at write-time and bumped by the `BumpFor*` helpers. Authoritative live ranking uses `ColdScoreWith(sc, weights, now)` with the actor's current learned Weights. Query and Context paths always call `ColdScoreWith` — not `sc.Cached` directly.

**Salience is NOT in OverallRoot.** The `salience/<id>` and `meta/salience_weights` keys are derived state. The `Rebuild` operation drops and re-derives them from the journal. This is intentional — the recency component `R(m)` depends on wall-clock time, so the same salience cache could not be reproduced byte-identically on replay anyway.

---

## Formula

```
salience(m) = w1·R(m) + w2·A(m) + w3·C(m) + w4·D(m) + w5·V(m, q)

R(m) = exp(-Δt(now, last_used) / half_life)           // recency; half-life 90d
A(m) = log(1 + access_count) / log(1 + 1000)          // normalized access
C(m) = log(1 + cite_in_successful_plans) / log(1+1000) // normalized citations
D(m) = declared_importance / 10.0                      // 0..1
V(m, q) = cosine(embedding(m), embedding(q))           // 0 when no Near query
```

When `q.near` is unset, `w5` is 0 and the remaining weights are renormalized by dividing by their sum (0.90 at cold-start weights), equivalent to "redistribute w5 proportionally".

### Cold weights (§8.2)

| Weight | Symbol | Default |
|---|---|---|
| Recency | WR | 0.25 |
| Access | WA | 0.15 |
| Citations | WC | 0.30 |
| Declared importance | WD | 0.20 |
| Vector similarity | WV | 0.10 |

These weights are the cold start. Per-actor learned weights override them after `cortex.Attest` calls accumulate enough signal.

### Pinned floor

Memories in the Pinned tier (Identity, hard Constraints, active Goals) receive a salience floor of `PinnedFloor = 0.7`. Applied in `Context` and `Compact` after computing the live score.

---

## Score record

```go
type Score struct {
    SchemaVersion uint8
    LastUsed      int64    // UnixNano; updated by Write, Update, UpdateHead, Attest
    Importance    uint8    // DeclaredImportance at last bump; 0..10
    AccessCount   uint64   // incremented by successful Attest
    Citations     uint64   // citation count in successful plans
    Cached        float32  // last computed cold score (snapshot; not live ranking)
    ComputedAt    int64    // UnixNano of last Cached recompute
}
```

---

## Bump helpers

| Helper | When called | What it does |
|---|---|---|
| `NewForWrite(importance, now)` | `cortex.Write` | Seeds a fresh Score at cold-start values |
| `BumpForUpdate(sc, importance, now)` | `cortex.Update`, `UpdateHead` | Advances `LastUsed`, refreshes `Cached` |
| `BumpForCitation(sc, now)` | `cortex.Attest` (success) | Increments `Citations` + `AccessCount`, refreshes `Cached` |
| `DecrementCitation(sc, now)` | `cortex.Attest` (failure, factual_error or wrong_assumption) | Decrements `Citations` (floor 0), refreshes `Cached` |
| `ZeroForTombstone(sc, now)` | `cortex.Tombstone` | Sets `Cached = 0`; factor inputs preserved |

---

## Per-actor weight learning

After enough `Attest` calls, the actor's weights drift away from the cold-start defaults to reflect which factors best predict success on this actor's workload.

```go
type Weights struct {
    SchemaVersion uint8
    WR, WA, WC, WD, WV float32  // must sum to 1.0
    UpdatedAt int64
    Updates   uint64
}
```

Weights are persisted at `meta/salience_weights` (one key per actor). The `Rebuild` operation drops and re-derives this key by replaying `KindLearnWeights` journal entries.

### EMA update (§8.3)

```go
const EMARate float32 = 0.05  // α

// Called from cortex.Attest
salience.UpdateWeightsEMA(&weights, postBumpScores, EMARate, decrementOnFailure, now)
```

The EMA pulls `Weights` toward the factor profile of the cited memories:
- **Success** or **non-decrement failure** — pulls TOWARD the cited profile.
- **Decrement-reason failure** (factual_error, wrong_assumption) — pulls AWAY.

The direction matches the bandit-lite interpretation: success reinforces the weighting that ranked those memories highly; a factual-error failure penalizes it.

`alpha=0.05` is intentionally slow — roughly 14 intents to move the weight by half a standard deviation from its initial value.

---

## Live cold score

```go
score := salience.ColdScoreWith(sc, weights, now)
```

Computes the live salience using the actor's current learned weights and the current wall clock. This is what `query.Run` and `cortex.Context` use for ranking and trim decisions.

`ColdScore(sc, now)` is the version that uses hard-coded cold weights — used as a fallback when no learned weights exist yet.

---

## Read / encode / decode

```go
// Read from store
sc, ok, err := salience.Read(s, memoryID)

// Encode/decode
bytes, err := salience.Encode(&sc)
err = salience.Decode(bytes, &sc)

// Weights
weights, ok, err := salience.ReadWeights(s)
bytes, err := salience.EncodeWeights(&weights)
```

---

## Modifying salience

| What to change | Where |
|---|---|
| Formula factors or weights | `cortex/salience/salience.go` — `ColdScore`, `ColdScoreWith`, cold weight constants |
| Half-life decay constant | `cortex/salience/salience.go` — the time division in `R(m)` |
| Pinned floor value | `cortex/salience/salience.go` — `PinnedFloor` constant |
| EMA learning rate | `cortex/salience/salience.go` — `EMARate` constant (also stamped in `KindLearnWeights` journal entries for replay determinism) |
| Add a new factor | Extend `Score` struct; add corresponding `Weights` field; bump `WeightsSchemaVersion`; update `ColdScoreWith`; update `UpdateWeightsEMA` |
