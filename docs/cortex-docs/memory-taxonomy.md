# Memory Taxonomy

Package `matrix/cortex/memory` defines the typed record schema for every memory that lives in the cortex. Nine types, a shared `Head` + `Version` structure, auto-generated forms at three granularities, and write-time validation. This is the type system everything else operates on.

Source files: `cortex/memory/types.go`, `cortex/memory/data.go`, `cortex/memory/codec.go`, `cortex/memory/validate.go`, `cortex/memory/verb.go`, `cortex/memory/edge.go`.

---

## Design decisions

**Nine types, closed vocabulary.** The type discriminator is a 1-byte integer. Adding a tenth type is a schema migration, not a code change — existing persisted bytes are tied to the byte values and must not be renumbered.

**Two-record layout per memory.** `Head` is the mutable record (tags, frames, visibility, tombstone, salience pointer). `Version` is the immutable data record (one per `Write` or `Update`). They share an ID but are stored under separate key prefixes (`m/` and `mv/`) so `UpdateHead` can rewrite `m/<id>` without touching `mv/<id>/v/<n>`.

**Forms are computed at write time and stored.** Short and medium forms are rendered once by `forms.Render`, persisted in both `Head.Forms` and `Version.Forms`, and read on every `Find` without a live recompute. Full form is only rendered on demand. This keeps query latency predictable.

---

## The 9 memory types

| Code | Type | Description |
|---|---|---|
| `0x01` | `Identity` | Who the actor is — name, DID, profile fields |
| `0x02` | `Fact` | A stated, believed, or observed true thing about the world |
| `0x03` | `Preference` | A stated like/dislike — topic, polarity, strength |
| `0x04` | `Belief` | An uncertain or probabilistic claim — stance, confidence |
| `0x05` | `Event` | A timestamped thing that happened — kind, outcome, counterparty, cost |
| `0x06` | `Goal` | An objective to pursue — statement, status, horizon |
| `0x07` | `Constraint` | A standing rule or guardrail — polarity, strength, source |
| `0x08` | `Capability` | A proven or declared ability — subject, description, verified flag |
| `0x09` | `Pattern` | A reusable how-to recipe — statement, strength, coverage, predecessors |

All nine implement the `TypedData` interface. `memory.TypeOf(data)` returns the type byte for a concrete value.

---

## Head

`Head` is stored at `m/<id:16>` and rewritten on every `Write`, `Update`, `Tombstone`, and `UpdateHead`.

```go
type Head struct {
    ID                 ID
    Type               Type
    ActorScope         string
    Visibility         Visibility        // VisPrivate | VisScoped | VisActorPublic
    Tags               []Tag
    Frames             []FrameRef        // idx/frame entries — Frame-tier routing
    DeclaredImportance uint8             // 0..10; feeds salience.D factor
    CurrentVersion     uint64
    LastUpdatedAt      time.Time
    Tombstoned         *Tombstone        // nil = live
    EmbeddingRef       *VectorRef        // nil until async embedder runs
    Forms              Forms             // latest short + medium, mirrored from Version
}
```

### Mutable fields

Only `Tags`, `Frames`, `DeclaredImportance`, and `Visibility` are mutable via `UpdateHead`. All other fields are auto-managed. Attempting to set `ID`, `Type`, `CurrentVersion`, `Tombstoned`, `EmbeddingRef`, or `Forms` via `UpdateHead` is rejected at the API boundary.

### Visibility

| Value | Meaning |
|---|---|
| `VisPrivate` | Only the actor can read it |
| `VisScoped` | Readable by sub-agents carrying a valid `CortexScope` that `Allows` this head |
| `VisActorPublic` | Readable by any agent that can reach this cortex |

---

## Version

`Version` is stored at `mv/<id:16>/v/<n:8>` and is immutable once written.

```go
type Version struct {
    ID         ID
    Version    uint64
    Type       Type
    Data       []byte      // canonical CBOR-encoded typed Data
    CreatedAt  time.Time
    CreatedBy  string
    Confidence float32     // 0..1; 1.0 default
    Provenance Provenance
    Hash       [32]byte    // sha256("matrix.cortex.memory.v1" || Type || Data)
    Forms      Forms
    FormsOverride bool
}
```

Old versions remain readable after `Update` — they are never deleted. The `Resolve(uri)` method requires an explicit version number; `ResolveLatest(id)` is provided for convenience but is discouraged in compiler paths (D13).

---

## Data schemas

Each of the 9 types has its own `*Data` struct encoded as canonical CBOR into `Version.Data`.

### Key schemas

**FactData**
```go
type FactData struct {
    SchemaVersion uint8
    Subject       string
    Predicate     string
    Statement     string
    Confidence    float32
    Source        SourceKind
    ObservedAt    time.Time
}
```

**PreferenceData**
```go
type PreferenceData struct {
    SchemaVersion uint8
    Topic         string
    Polarity      Polarity   // prefer | avoid | neutral | do | dont
    StrengthVal   float32    // 0..1
    Rationale     string
}
```

**ConstraintData**
```go
type ConstraintData struct {
    SchemaVersion uint8
    Statement     string
    Polarity      Polarity
    StrengthVal   Strength   // soft | firm | hard
    Source        ConstraintSource
}
```

**GoalData**
```go
type GoalData struct {
    SchemaVersion uint8
    Statement     string
    Status        GoalStatus   // active | paused | completed | abandoned
    Horizon       time.Time
}
```

**PatternData**
```go
type PatternData struct {
    SchemaVersion uint8
    Statement     string      // "neo.pattern.v1:" + JSON PatternSpec
    Strength      float32
    Coverage      int
    DerivedFrom   []string
}
```

---

## Forms

Three render granularities, computed by `forms.Render`:

| Form | Budget | Source |
|---|---|---|
| `Short` | 50 tokens max | Persisted in Head.Forms + Version.Forms at write time |
| `Medium` | 200 tokens max | Persisted in Head.Forms + Version.Forms at write time |
| `Full` | Unbounded | Rendered live on demand by `forms.RenderFull` |

Token budget uses the bytes/4 heuristic (`memory.CountTokens`). Switching to a real BPE tokenizer would change the snapshot hash — it's a spec-level migration.

Per-type short scaffold templates:

| Type | Short form |
|---|---|
| Identity | `{name}` or `{name} ({did})` |
| Fact | `{predicate}({subject})={statement}` |
| Preference | `prefers {topic} ({polarity}, strength={s:.2f})` |
| Belief | `{stance} {statement}` |
| Event | `{outcome} {kind} with {counterparty} cost={...}` |
| Goal | `[{status}] {statement}` |
| Constraint | `[{strength}] {polarity} {statement}` |
| Capability | `{subject} can {capability} ({verified?})` |
| Pattern | `{statement} (strength={s:.2f}, coverage={c})` |

Medium adds detail: rationale, evidence counts, confidence, durations, horizon dates, counterparty, asset amounts.

### FormsOverride

Callers that want a custom rendered form can set `WriteMeta.FormsOverride=true` and supply their own `Forms`. The override bypasses auto-generation but is still validated against the token budgets by `memory.ValidateMemory`. Exceeding budget returns `ErrFormTooLong`.

---

## Tombstone

```go
type Tombstone struct {
    Reason string
    At     time.Time
    By     string
}
```

Tombstoned memories keep all their version records (audit trail). `Find` excludes tombstoned memories by default; `Query.IncludeTombstoned=true` opts in. `Update` on a tombstoned memory returns `ErrTombstoned`. Salience is zeroed at tombstone time (factor inputs are preserved for a hypothetical un-tombstone path).

---

## Verbs and FrameRefs

`Verb` is the closed 10-value enum from D7, stored as a 1-byte integer:

| Verb | Byte |
|---|---|
| `find` | 0x01 |
| `acquire` | 0x02 |
| `build` | 0x03 |
| `modify` | 0x04 |
| `deliver` | 0x05 |
| `analyze` | 0x06 |
| `negotiate` | 0x07 |
| `schedule` | 0x08 |
| `monitor` | 0x09 |
| `delegate` | 0x0A |

`FrameRef` binds a memory to a (verb, object-kind, object-ref) triple. These are indexed under `idx/frame` and drive the Frame-relevant tier in `Context`.

```go
type FrameRef struct {
    Verb    Verb
    ObjKind ObjKind
    ObjRef  string
}

func (fr FrameRef) Hash() [ObjHashSize]byte // sha256(ObjRef)[:16]
```

`ObjKind` is also a closed 1-byte enum — the set of object-kind labels the Frame tier recognizes (token, address, skill, tool, etc.).

---

## Modifying the taxonomy

| What to change | Where |
|---|---|
| Add or rename a memory type | `memory/types.go` — new `Type` const; `memory/data.go` — new `*Data` struct; `memory/codec.go` — encode/decode switch; `memory/validate.go` — schema rules; `forms/forms.go` — render templates. Requires a journal migration. |
| Change a data field | `memory/data.go` — bump `SchemaVersion`; update encode/decode; update validate and forms. |
| Change token budget caps | `memory/types.go` — `MaxShortTokens`, `MaxMediumTokens` |
| Add a verb | `memory/verb.go` — `Verb` const (append only — never reorder); update idx/frame key scheme. Requires a journal migration. |
| Add an ObjKind | `memory/verb.go` — `ObjKind` const (append only); update idx/frame key scheme. Requires a migration. |
