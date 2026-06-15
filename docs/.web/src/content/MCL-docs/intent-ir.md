# Intent IR

Package `matrix/mcl/ir` defines the Go types for the Intent IR — the central typed artifact that Matrix produces and operates on. Everything the MCL compiler produces ends up here. Everything the executor consumes comes from here.

The IR is canonical JSON with sorted keys for deterministic hashing (D11). CBOR encoding is layered on top for the wire protocol via the `envelope` package. When you see `IntentCompiledBody.IntentJSON`, it's the canonical JSON bytes of one of these types.

Source files: `MCL/ir/intent.go`, `MCL/ir/plan.go`, `MCL/ir/plan_validate.go`, `MCL/ir/encode.go`.

---

## Intent

The top-level type. Every interaction produces one.

```go
type Intent struct {
    ID      string // ULID
    Version string // "mcl/0.1"
    Parent  string // parent IntentRef for sub-intents (omitempty)
    Actor   string // who wants this (UserRef or AgentRef DID)
    Agent   string // who will execute (AgentRef DID)

    Prose string // original NL goal — display only, never authoritative

    Frame Frame // typed source of truth — what gets signed and executed

    Unknowns   []Unknown   // registered gaps
    References []Reference // grounding URIs

    State      string  // IntentState constant
    Confidence float64 // 0..1
    Budget     *Budget
    Deadline   string  // ISO8601

    GoalID string // links to a parent standing Goal (omitempty)

    SignedBy string // actor's public key (hex-encoded ed25519)
    Hash     string // sha256 self-hash for content addressing

    CompileMetadata *CompileMetadata // D11 compilation trace
}
```

The `Prose` field is purely for display. It goes in the journal and shows up in UIs. The `Frame` is the source of truth — it's what the executor uses, what the user signs, and what gets content-addressed.

`GoalID` links a one-shot chat intent to a standing Goal memory in cortex, so per-goal cost telemetry works. Empty for standalone intents.

---

## Frame

The typed source-of-truth surface. Five named fields.

```go
type Frame struct {
    Verb            string       // D7 closed vocab
    Objects         []SlotEntry  // typed named referents
    Constraints     []Constraint // what must hold
    SuccessCriteria []Predicate  // what defines completion
    Preferences     []Preference // soft tie-breakers
}
```

### SlotEntry

```go
type SlotEntry struct {
    Name  string // slot name from §INPUTS
    Value string // resolved value or NL text (pre-D13)
    URI   string // resolved matrix:// URI (post D13) — omitempty
    Type  string // type annotation — omitempty
}
```

After stage 5 (entity resolution), `URI` is populated for all referents that were successfully resolved. Slots that couldn't be resolved become `Unknown` entries instead.

### Constraint

Typed predicates that must hold throughout execution:

```go
type Constraint struct {
    Type string // "budget", "deadline", "jurisdiction", "quality", "rule", "policy", "x:*"
    Hard bool   // hard=true → fail the intent if violated

    // Type-specific fields (only the relevant ones populated)
    Max    *AssetAmount // budget: max spend
    By     string       // deadline: ISO8601 deadline
    Allow  []string     // jurisdiction: allowed regions
    Deny   []string     // jurisdiction: denied regions
    Metric string       // quality: metric name
    Min    float64      // quality: minimum value
    Rule   string       // rule: RuleRef URI
    Policy string       // policy: Argus policy ref
    Schema string       // x:custom: schema URI
    Data   string       // x:custom: opaque JSON
}
```

The closed type set: `budget`, `deadline`, `jurisdiction`, `quality`, `rule`, `policy`. `x:` prefix for custom types. The grammar constraint on the LLM forces output into this schema.

### Predicate

Checkable completion criteria:

```go
type Predicate struct {
    Type     string // "delivered", "signed_off", "external", "attestation", "x:*"
    Artifact string // delivered: artifact URI
    By       string // signed_off: UserRef
    URL      string // external: check URL
    Check    string // external: check string
    Source   string // attestation: AgentRef
    Topic    string // attestation: topic
    Schema   string // x:custom schema
    Data     string // x:custom opaque JSON
}
```

All predicates must evaluate to `true` before the executor emits `intent.attest`.

---

## Unknown

A typed gap blocking or delaying execution.

```go
type Unknown struct {
    ID         string   // local id "u1", "u2" etc.
    Field      string   // SlotPath into the Frame: "frame.constraints[0].max"
    Type       string   // expected type
    Severity   string   // "blocking", "preferred", "optional"
    Rationale  string   // human-readable reason
    Default    string   // suggested fill
    Options    []string // enum-like choices
    SourceHint string   // cortex location that might fill this
}
```

`blocking` severity stops execution — the intent cannot proceed until this gap is filled. `preferred` and `optional` are advisory.

The `ID` field correlates unknowns to clarify questions in the `intent.clarify` body (`ClarifyQuestion.UnknownID`).

---

## IntentState

The lifecycle state machine constants:

```go
const (
    StateDraft      = "draft"       // initial state at creation
    StateProposed   = "proposed"    // intent.compiled emitted, awaiting accept
    StateClarifying = "clarifying"  // intent.clarify sent, awaiting intent.answer
    StateAccepted   = "accepted"    // user signed via intent.accept
    StateExecuting  = "executing"   // executor is running
    StateCompleted  = "completed"   // intent.attest sent
    StateFailed     = "failed"      // intent.fail sent
    StateCancelled  = "cancelled"   // intent.cancel received
)
```

Transitions: `draft → proposed → (clarifying →)* accepted → executing → completed|failed|cancelled`.

---

## D7 verb constants

```go
const (
    VerbFind      = "find"
    VerbAcquire   = "acquire"
    VerbBuild     = "build"
    VerbModify    = "modify"
    VerbDeliver   = "deliver"
    VerbAnalyze   = "analyze"
    VerbNegotiate = "negotiate"
    VerbSchedule  = "schedule"
    VerbMonitor   = "monitor"
    VerbDelegate  = "delegate"
)
```

`ir.ValidVerb(v)` returns true for any of these or any `x:`-prefixed extension verb. Extension verbs are valid but not first-class — they get no routing table entry in `verb.mtx` and no specialist executor model.

---

## CompileMetadata

Records the full compilation trace for D11 replay-verification:

```go
type CompileMetadata struct {
    Seed               string  // sha256(intent.id || actor || snapshot_hash || mtx_digest || model_digest)
    MtxDigest          string  // sha256 of canonical SKILL.mtx + core/*.mtx ASTs
    ModelDigest        string  // digest of the compiler model weights
    ModelVersion       string  // model identifier
    Temperature        float64
    Grammar            string  // grammar constraint ID used, e.g. "intent_frame@1"
    SkillID            string  // which skill was selected
    SkillVersion       string
    CortexSnapshotHash string  // Merkle root of cortex at compile time
}
```

Anyone with access to the same SKILL.mtx version, the same model weights, and the same cortex snapshot can rerun the compilation and verify they get the same `Hash` on the resulting Intent. This is the D11 replay invariant.

---

## PlanTree

The executor produces a `PlanTree` when a skill's `§PROCEDURE` on-block contains planning steps. The plan tree is the decomposition of the Intent into an executable graph of steps.

```go
type PlanTree struct {
    ID          string     // ULID
    Version     string     // "mcl/0.1"
    IntentID    string     // back-reference to the parent Intent
    CreatedAt   string     // ISO-8601
    CreatedBy   string     // matrix://agent/<did>
    SkillRef    string     // version-pinned skill URI
    ModelDigest string     // executor model digest at plan-emission time
    Root        PlanNode   // entry point
    Budget      *Budget
    Hash        string     // sha256 self-hash
}
```

A `PlanTree` is a DAG, in practice a tree with shared leaves for citations. Walks are depth-first by default. The `Root` is the single entry point.

---

## PlanNode

A single node in the plan. `Kind` discriminates the payload.

```go
type PlanNode struct {
    ID          string     // stable within the tree, e.g. "n1", "n2"
    Kind        string     // node kind constant
    Description string     // short human-readable label
    Children    []PlanNode // child nodes (empty for terminal kinds)

    // Payload fields — exactly one is populated based on Kind
    Step        *StepPayload
    ToolCall    *ToolCallPayload
    SubDispatch *SubDispatchPayload
    Gate        *GatePayload

    ResultText  string // runtime-only, excluded from hash (json:"-")
}
```

`ResultText` is populated at runtime by the walker with the actual tool output or LLM step text. It's excluded from canonical encoding so it never enters D11 or the signed plan — it's pure walk state that lets later steps reference upstream outputs via `${nodeID.output}`.

### Node kinds (closed)

| Kind | Payload | Semantics |
|---|---|---|
| `sequential` | — | Run Children in order; first failure halts |
| `parallel` | — | Run Children concurrently; first failure cancels siblings |
| `step` | `Step` | In-skill LLM prompt step |
| `tool_call` | `ToolCall` | Single tool invocation |
| `sub_dispatch` | `SubDispatch` | Sub-skill or sub-agent dispatch |
| `gate` | `Gate` | Human-in-loop checkpoint |

### StepPayload

```go
type StepPayload struct {
    PromptName          string            // skill prompt block to invoke (empty = default)
    Inputs              map[string]string // slot bindings for interpolation
    ExpectedOutputs     []string          // slot names this step should produce
    Kind                string            // step kind hint for model routing (omitempty)
}
```

`Kind` drives executor-tier model routing. The closed set is defined in `ir.StepKindNames`. Empty defaults to `"reason"` at routing time — this is safe because the default model handles generic reasoning.

The `omitempty` on `Kind` preserves byte-identity for plans authored before the model router (Session 31b) was added. Any plan that doesn't set `Kind` is byte-identical to what it would have been before. This is a deliberate backward-compatibility decision.

### ToolCallPayload

```go
type ToolCallPayload struct {
    ToolRef         string            // version-pinned matrix://tool/... URI
    Args            map[string]string // typed arguments
    TimeoutMs       int               // 0 = tool's manifest default
    SideEffectClass string            // "read", "write", "network", "shell", "chain"
}
```

`SideEffectClass` is cross-checked against the agent's allowed side-effect set at the executor's capability gate before dispatch. The closed set: `read`, `write`, `network`, `shell`, `chain`.

Sensitive values (API keys, tokens) must not appear inline in `Args`. Use environment variable references instead so they don't leak into the plan's content address.

### SubDispatchPayload

```go
type SubDispatchPayload struct {
    SkillRef       string  // version-pinned sub-skill URI
    AgentRef       string  // target agent (empty = in-process)
    SubIntent      *Frame  // the Frame for the sub-intent
    ScopeURI       string  // CortexScope grant for cross-agent reads
}
```

In-process dispatch (same agent, `AgentRef` empty) is the v1 default. Cross-agent dispatch requires a `ScopeURI` granting the child agent read access to the relevant cortex memories.

The parent skill must declare the sub-skill URI in its `§SUB_SKILLS` section (enforced at plan validation).

### GatePayload

```go
type GatePayload struct {
    RuleRef   string   // matrix://rule/<id> that triggered
    Question  string   // human-readable prompt
    Options   []string // allowed answers (empty = free text)
    TimeoutMs int      // 0 = no timeout (block forever)
}
```

When the executor hits a `NodeGate`, it emits `policy.gate` to the user and halts the walk until `policy.gate.resolve` comes back. If `TimeoutMs` is set and expires with no response, the gate is treated as denied.

---

## Plan validation

`ir.plan_validate.go` enforces the hard rules on a `PlanTree` before the executor runs it:

- S3: `Root` must be non-nil
- S4: All `ToolRef` and `SkillRef` URIs must be version-pinned (no bare refs)
- S5: All node `Kind` values must be in `ValidNodeKinds`
- S6: `SubDispatchPayload.SkillRef` must appear in the skill manifest's declared `§SUB_SKILLS`
- S7: `SideEffectClass` values must be in `ValidSideEffectClasses`

---

## Content addressing

Both `Intent` and `PlanTree` carry a `Hash` field. It's the `sha256` of the canonical JSON encoding of the struct with `Hash` cleared. This makes them self-describing content addresses.

Usage:
- `intent.accept` body carries `IntentHash` — the receiver verifies this matches the local IR before treating the acceptance as valid
- Journal storage uses the hash as the key
- `intent.correct` patches reference the current hash to prevent stale updates

The canonical encoding uses sorted JSON keys (not the default Go encoder order). `ir.encode.go` implements this.
