# Scope

Package `matrix/cortex/scope` is the cryptographic privacy boundary for cortex reads and writes issued by sub-agents. A `CortexScope` binds a sub-agent's identity to a pinned snapshot root, a set of allowed memories, a Merkle multi-proof, and an optional writable bit. The cortex verifies the scope on every scoped call — signature, expiry, snapshot resolvability, and proof validity — before applying per-candidate `Allows` checks.

Source files: `cortex/scope/scope.go`, `cortex/scope/verify.go`, `cortex/scope/match.go`, `cortex/scope/errors.go`, `cortex/scope_enforce.go`.

---

## Design decisions

**Cortex never signs.** Scope creation and signing lives in the agent runtime or sub-dispatch executor. Cortex only verifies. Key material never touches the cortex layer (D4).

**Merkle proofs, not API trust.** The sub-agent receives a multi-proof against the pinned snapshot root alongside the scope. It can verify its allowed keys independently — no need to trust an API call. This is the "why Merkle proofs not API trust" principle from research/06-agents.md §7.

**Verify once, filter per-candidate.** `VerifyScope` runs the full chain (signature, expiry, snapshot, proofs) once at the start of `Find`, `Context`, or `ResolveScoped`. Per-candidate filtering then calls `Scope.Allows(&head)` cheaply without re-running the crypto chain.

**Default deny for writes.** A scope without `Writable=true` returns `ErrNotWritable` on any `UpdateHead` attempt, regardless of `Allows`. Sub-agents are read-only by default.

---

## The Scope struct

```go
type Scope struct {
    SchemaVersion uint8
    Actor         string           // whose cortex
    SnapshotHash  [32]byte         // OverallRoot at scope creation time
    Include       Selector         // what the sub-agent MAY read
    Exclude       Selector         // belt-and-suspenders deny list
    Proofs        *MultiProof      // nil for Type/Tag/Frame-only scopes
    GrantedTo     string           // sub-agent ref
    GrantedBy     string           // parent agent ref (who signed this scope)
    ExpiresAt     time.Time        // zero = never expires
    BudgetTokens  int              // hard cap on cortex.Context budget; 0 = uncapped
    Writable      bool             // default false; required for UpdateHead
    Signature     []byte           // ed25519 sig by GrantedBy over UnsignedBytes(s)
}
```

### Selector

```go
type Selector struct {
    IDs    []memory.ID     // specific memory IDs — requires Proofs
    Types  []memory.Type   // all memories of these types
    Tags   []memory.Tag    // all memories having any of these tags
    Frames []memory.FrameRef // all memories indexed on these FrameRefs
}
```

`Include.IsEmpty()` — an empty Include grants nothing. `Exclude` is applied after `Include` matches.

---

## Creating and signing a scope

Scope creation lives in the agent runtime. The cortex package provides the helpers:

```go
unsigned := scope.UnsignedBytes(s)  // canonical CBOR of all fields except Signature
sig := ed25519.Sign(privKey, unsigned)
s.Signature = sig
```

Encoding:
```go
bytes, err := scope.Encode(s)   // canonical CBOR
s, err := scope.Decode(bytes)   // parse + validate shape
```

---

## Verification chain

`scope.Verify(s, snapState, resolver, opts)` checks:

1. `SchemaVersion` matches the package constant.
2. `Include` is non-empty.
3. `now <= ExpiresAt` (or `ExpiresAt` is zero).
4. `GrantedBy`'s public key is resolved via `KeyResolver`.
5. `Signature` is a valid ed25519 signature over `UnsignedBytes(s)`.
6. `SnapshotHash` is resolvable — there exists a `snap/<seq>` manifest with that `OverallRoot`.
7. If `Proofs` is non-nil: `len(Proofs.Proofs) == len(Include.IDs)`, each proof's `KeyHash` matches `snapshot.HashMemoryKey(id)`, and the multi-proof verifies against the resolved manifest.

```go
err := c.VerifyScope(s, time.Now())  // cortex facade
```

### KeyResolver

```go
type KeyResolver interface {
    ResolveAgentKey(ref string) (ed25519.PublicKey, error)
}
```

Injected at cortex construction via `cortex.WithKeyResolver(r)`. Cortex never holds key material; the resolver lives in the agent runtime / tools/registry layer. A cortex constructed without a resolver rejects all scoped calls with `ErrNoKeyResolver`.

---

## Scope enforcement choke point

Three functions in `cortex/scope_enforce.go` are the sole choke points:

```go
// Once per call — full crypto chain
func (c *Cortex) VerifyScope(s *scope.Scope, now time.Time) error

// Per-candidate in Find / Context / ResolveScoped — cheap Allows check
func (c *Cortex) enforceRead(s *scope.Scope, h *memory.Head) error

// Per-target in UpdateHead — requires Writable + Allows
func (c *Cortex) enforceWrite(s *scope.Scope, h *memory.Head) error
```

`enforceRead` on a miss journals a `KindScopeViolation` entry and returns `scope.ErrViolation`. `Context` and `Find` (multi-target reads) filter silently without journaling per-candidate violations. `ResolveScoped` (single-target) does journal the violation.

---

## Scope violations

A violation is logged as a `KindScopeViolation` journal entry carrying:
- `GrantedTo` — the sub-agent that violated
- `GrantedBy` — the parent who issued the scope
- `MemoryID` — the memory that was accessed or attempted
- `Reason` — `"violation"` or `"not_writable"`
- `Mode` — `"read"` or `"write"`

### Rate limiting

Scope violation logging is protected by a per-(GrantedTo, GrantedBy) token bucket (10/sec, burst 20). Over-rate violations still return `scope.ErrViolation` to the caller, but the journal write and its MMR cascade are suppressed. This bounds the `OverallRoot`-moving + Pebble-sync cost a malicious sub-agent can impose by looping violations.

---

## Using a scope

Scoped reads pass the scope on the query or resolve call:

```go
// Single-target read
mem, err := c.ResolveScoped(uri, scope, time.Now())

// Multi-target Find
result, err := c.Find(query.Query{
    Type:  []memory.Type{memory.TypeFact},
    Scope: scope,
    Limit: 10,
})

// Context bundle
bundle, err := c.Context(cortex.ContextOpts{
    Verb:  memory.VerbFind,
    Scope: scope,
})

// Head-only write (requires Writable=true)
_, err = c.UpdateHead(uri, patch, cortex.UpdateHeadMeta{Scope: scope})
```

---

## Error reference

| Error | Cause |
|---|---|
| `ErrViolation` | Memory outside `Include` or inside `Exclude` |
| `ErrNotWritable` | UpdateHead attempted with `Scope.Writable=false` |
| `ErrScopeExpired` | `now > ExpiresAt` |
| `ErrSchemaVersion` | Scope `SchemaVersion` doesn't match package constant |
| `ErrSnapshotUnresolved` | `SnapshotHash` not found in any `snap/<seq>` manifest |
| `ErrProofMismatch` | Proof count/key-hash mismatch vs `Include.IDs` |
| `ErrEmptyInclude` | `Include.IsEmpty()` — nothing is allowed |
| `ErrActorMismatch` | `Scope.Actor` != store actor |
| `ErrUnknownAgent` | `KeyResolver.ResolveAgentKey` returned unknown ref |
| `ErrNoKeyResolver` | Scoped call on a cortex without `WithKeyResolver` |
| `ErrBudgetExceeded` | `Context` request exceeds `Scope.BudgetTokens` |

---

## Modifying scope

| What to change | Where |
|---|---|
| Selector membership criteria | `scope/match.go` — `Allows` |
| Verification chain steps | `scope/verify.go` — `Verify` |
| Scope wire format | `scope/scope.go` — `Scope` struct; bump `SchemaVersion` |
| Scope violation rate limits | `cortex/ratelimit.go` — `DefaultRateLimits().ScopeViolation` |
| Key resolver implementation | Agent runtime — implement `scope.KeyResolver` |
