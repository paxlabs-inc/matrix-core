# Envelope & Wire Protocol

Package `matrix/mcl/envelope` is the canonical wire codec for all 15 MCL message kinds. Every message Matrix sends or receives rides inside an `Envelope`: a typed header, an opaque CBOR body, and an ed25519 signature over the canonical encoding.

Source files: `MCL/envelope/envelope.go`, `MCL/envelope/kinds.go`, `MCL/envelope/body.go`, `MCL/envelope/json.go`, `MCL/envelope/keyresolver.go`.

---

## Design decisions

**CBOR, not JSON, on the wire.** The body and envelope headers are encoded with `github.com/fxamacker/cbor/v2` using `CoreDetEncOptions()` — the canonical deterministic mode. This gives compact binary encoding with byte-identical output for the same input, which is required for signature stability.

**Body is opaque.** The `Body` field is `cbor.RawMessage` — the envelope codec doesn't know or care what's inside. This allows a single round-trip to preserve body bytes exactly, which is what the D11 replay invariant requires.

**Integer keyasint CBOR tags.** Every field on `Envelope` uses integer CBOR keys (0, 1, 2, ...). Adding a new field at an unused integer tag is non-breaking. Deleting a field requires a `SchemaVersion` bump.

**SchemaVersion in signed bytes.** The `SchemaVersion` field is included in the unsigned bytes (the thing that gets signed). This means a signature from schema v1 cannot be verified as valid under schema v2 — replay attacks across schema versions are blocked at the cryptographic level.

**JSON on disk.** For journal/logs readability, envelopes are written to disk as JSON via `EnvelopeJSON` (in `json.go`). The JSON representation is a thin wrapper that re-encodes fields — it's never the canonical form used for signatures.

---

## The 15 message kinds

There is no `chat.message` kind — that would be the bug MCL is designed to prevent. Every user input is an `intent.draft` (new goal) or an `intent.answer`/`intent.correct` (continuing an existing one).

| Kind | Direction | Purpose |
|---|---|---|
| `intent.draft` | User → Agent | Initial NL goal + optional slot pre-fills |
| `intent.compiled` | Agent → User | Typed Intent IR for user review |
| `intent.clarify` | Agent → User | Structured questions for unknowns |
| `intent.answer` | User → Agent | Slot patches answering clarify questions |
| `intent.accept` | User → Agent | Signed sign-off — transitions to `accepted` |
| `plan.proposed` | Agent → User | Decomposition into steps before execution |
| `plan.step` | Agent → Agent/Tool | Single step execution (executor-internal) |
| `plan.output` | Agent → User | Streaming intermediate output |
| `intent.correct` | User → Agent | Patch an Intent or plan mid-flight |
| `intent.dispatch` | Agent → Agent | Sub-intent to a delegated agent |
| `intent.attest` | Agent → User/Chain | Signed completion receipt |
| `intent.fail` | Agent → User | Typed failure |
| `intent.cancel` | User → Agent | Revoke before completion |
| `policy.gate` | Agent → User | Human-in-loop checkpoint |
| `policy.gate.resolve` | User → Agent | Approve or deny a gate |

---

## Envelope structure

```go
type Envelope struct {
    SchemaVersion   uint8          // field 0 — schema version for replay protection
    ProtocolVersion string         // field 1 — "mcl/0.1"
    Kind            string         // field 2 — one of the 15 kinds
    ID              string         // field 3 — ULID for this message
    At              string         // field 4 — ISO-8601 timestamp
    From            string         // field 5 — sender principal (matrix://agent/<did>)
    To              string         // field 6 — recipient principal (omitempty)
    Intent          string         // field 7 — matrix://intent/<id>
    CorrelationID   string         // field 8 — request/response correlation (omitempty)
    CausationID     string         // field 9 — causation trace (omitempty)
    Body            cbor.RawMessage // field 10 — kind-specific payload
    Signature       []byte         // field 11 — ed25519 sig over UnsignedBytes (omitempty)
}
```

Every message belongs to exactly one Intent (`Intent` field required). This is how the cortex journal maintains per-intent event streams.

`CorrelationID` links request/response pairs — `intent.clarify.ID` → `intent.answer.CorrelationID`. `CausationID` traces multi-hop causation chains for audit.

Required header fields: `ID`, `At`, `From`, `Intent`. Missing any of these causes `NewEnvelope` / `Sign` / `Verify` to return an error.

---

## Creating and signing an envelope

```go
// Create an unsigned envelope from a typed body
env, err := envelope.NewEnvelope(envelope.KindIntentDraft, envelope.IntentDraftBody{
    Prose:      "Build a deployment pipeline for my Node.js app",
    SlotValues: map[string]string{"target": "my-app"},
})

// Populate header fields
env.ID        = ulid.Make().String()
env.At        = time.Now().UTC().Format(time.RFC3339)
env.From      = "matrix://agent/" + actorDID
env.To        = "matrix://agent/" + executorDID
env.Intent    = "matrix://intent/" + intentID

// Sign with actor's ed25519 private key
err = envelope.Sign(env, privateKey)
```

`NewEnvelope` validates that the body type matches the kind via the `kindBodyType` map. Mismatched types return `ErrBodyTypeMismatch` immediately.

---

## Verifying an envelope

```go
err := envelope.Verify(env, keyResolver)
```

`Verify` runs the full chain:
1. `SchemaVersion` matches the package constant
2. Required header fields are populated
3. `Kind` is in the closed 15-kind set
4. `KeyResolver.ResolveKey(env.From)` returns a public key
5. `ed25519.Verify(pub, UnsignedBytes(env), env.Signature)` passes

Body shape is not validated by `Verify` — that's `ValidateBody`'s job after you decode. `Verify` only checks "this was sent by `env.From` and hasn't been tampered with."

### KeyResolver interface

```go
type KeyResolver interface {
    ResolveKey(principal string) (ed25519.PublicKey, error)
}
```

Implementations typically look up the DID document for the principal and return the public key. For tests, `envelope.StaticKeyResolver(map[string]ed25519.PublicKey{...})` works.

---

## Decoding the body

```go
// Decode into the matching typed struct
var body envelope.IntentDraftBody
err := env.DecodeBody(&body)

// Or use ValidateBody for strict kind↔type checking in one step
typed, err := envelope.ValidateBody(env)
if draft, ok := typed.(*envelope.IntentDraftBody); ok {
    // ...
}

// Or allocate the right type dynamically
out := envelope.NewTypedBody(env.Kind) // returns *IntentDraftBody etc.
err = env.DecodeBody(out)
```

---

## Body types reference

### IntentDraftBody

```go
type IntentDraftBody struct {
    Prose          string            // NL goal
    SlotValues     map[string]string // pre-filled slots from UI form
    PreferredSkill string            // optional skill hint (matrix://skill/... ref)
}
```

### IntentCompiledBody

```go
type IntentCompiledBody struct {
    IntentJSON       []byte // canonical JSON of ir.Intent
    CompileLatencyMs int64  // compilation time (for display)
}
```

The `IntentJSON` is the canonical JSON encoding of the `ir.Intent` struct. Receivers decode with `json.Unmarshal` into `ir.Intent`. This is not re-encoded in CBOR because the canonical JSON IS the content address — re-encoding would lose that property.

### IntentClarifyBody

```go
type IntentClarifyBody struct {
    Questions []ClarifyQuestion // one per unmet unknown
}

type ClarifyQuestion struct {
    UnknownID string   // matches Intent.Unknowns[].ID
    Field     string   // SlotPath the answer patches
    Prompt    string   // user-facing question text
    Type      string   // expected answer type
    Required  bool     // must the user answer this?
    Options   []string // enum-like suggestions
    Default   string   // suggested default
}
```

### IntentAnswerBody

```go
type IntentAnswerBody struct {
    Patches  []byte // RFC 6902 JSON Patch bytes
    AnswerOf string // correlation_id of the intent.clarify being answered
}
```

Patches are RFC 6902 applied against the Intent IR. The `patch/` package handles the typed `SlotPatch → RFC 6902` compilation (D8).

### IntentAcceptBody

```go
type IntentAcceptBody struct {
    IntentHash      string // sha256 of canonical-JSON Intent
    AcceptedAt      string // ISO-8601
    AnchorRequested bool   // opt-in chain anchoring
}
```

The outer `Envelope.Signature` is the acceptance signature. The body pins `IntentHash` so receivers can verify the signed hash matches the local IR before treating the acceptance as valid.

### PlanProposedBody

```go
type PlanProposedBody struct {
    PlanJSON []byte // canonical JSON of ir.PlanTree
}
```

Same posture as `IntentCompiledBody` — the canonical JSON is the content address.

### PlanStepBody (executor-internal)

```go
type PlanStepBody struct {
    PlanID    string
    NodeID    string
    Status    string // "started", "completed", "failed", "cancelled"
    Result    []byte // opaque JSON step output
    Error     string
    LatencyMs int64
}
```

Rarely user-visible. Used for inter-component messaging within the executor.

### PlanOutputBody (streaming)

```go
type PlanOutputBody struct {
    PlanID   string
    NodeID   string
    Sequence uint64 // monotonic counter within (PlanID, NodeID)
    Chunk    []byte
    Channel  string // "stdout", "stderr", "result", "progress"
    Final    bool   // marks last chunk
}
```

The only streaming kind. Multiple `plan.output` messages may share the same PlanID + NodeID and are distinguished by `Sequence`. `Final=true` marks the last chunk in the stream.

### IntentCorrectBody

```go
type IntentCorrectBody struct {
    Target    string // "intent" or "plan"
    Patches   []byte // RFC 6902 JSON Patch bytes
    Reason    string // structured reason code
    RetryFrom string // PlanNode.ID to resume from (empty = restart from root)
}
```

### IntentDispatchBody

```go
type IntentDispatchBody struct {
    SubIntentJSON  []byte // canonical JSON of child ir.Intent
    ScopeURI       string // CortexScope grant (empty for in-process)
    PaymentChannel string // payment stream for external dispatch (empty for in-process)
}
```

### IntentAttestBody

```go
type IntentAttestBody struct {
    Outcome     string   // "success", "failure", "partial"
    CitedURIs   []string // load-bearing cortex URIs → feeds salience EMA
    EvidenceJSON []byte  // structured evidence
    CompletedAt string
    AnchorTx    string   // chain tx hash (empty if not anchored)
}
```

`CitedURIs` are the `matrix://cortex/...` URIs that were load-bearing during execution. They feed into `cortex.Attest()` for salience EMA updates — memories that were useful get higher salience for future retrievals.

### IntentFailBody

```go
type IntentFailBody struct {
    Reason       string   // structured failure reason
    Message      string   // human-readable elaboration
    EvidenceJSON []byte
    FailedAt     string
    PartialURIs  []string // work products that landed before failure
}
```

Failure reasons: `blocked_by_constraint`, `tool_error`, `policy_denied`, `deadline_exceeded`, `budget_exceeded`, `subagent_failed`, `ambiguous_after_clarify`, `correction_invalid`, `x:custom`.

### PolicyGateBody / PolicyGateResolveBody

```go
type PolicyGateBody struct {
    RuleRef   string   // matrix://rule/<id>
    PlanID    string
    NodeID    string
    Question  string
    Options   []string // empty = free text
    ExpiresAt string   // auto-deny deadline
}

type PolicyGateResolveBody struct {
    GateOf     string // correlation_id of the policy.gate
    Decision   string // "approve" or "deny"
    Answer     string // chosen option or free-text answer
    ResolvedAt string
}
```

---

## Self-hash and content addressing

```go
hash, err := envelope.SelfHash(env)
```

Returns `sha256(UnsignedBytes(env))` as a hex string. Works before or after signing. Used as the content-address for journal storage and as the Merkle anchoring input when the agent posts an `intent.attest` on-chain.

---

## Encoding round-trip

```go
// Encode to wire bytes (CBOR, including signature)
wire, err := envelope.Encode(env)

// Decode from wire bytes
var env2 envelope.Envelope
err = envelope.Decode(wire, &env2)
```

The canonical enc/dec modes are `cbor.CoreDetEncOptions()` — the same options used in the cortex scope codec. They guarantee byte-identical encoding for the same logical value.
