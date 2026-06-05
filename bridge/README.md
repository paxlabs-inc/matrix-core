# bridge — MCL ↔ cortex glue

The `bridge` package wires the MCL interpreter (`matrix/mcl/mtx/interpreter`)
to a live cortex (`matrix/cortex`). The two core modules deliberately don't
import each other, so this is the third top-level Go module that links them.

## Why a separate module

Architectural rules (see `knowledge/matrix.kvx`):

- `matrix/mcl` defines a narrow `interpreter.Cortex` interface (Find /
  Resolve / Context). It must not depend on cortex implementation
  details.
- `matrix/cortex` is the typed memory graph. It must not know about MCL.
- Linking them in a third module keeps both dep graphs closed and lets
  this glue layer be opt-in.

## What's here

| File | Purpose |
| --- | --- |
| `bridge.go` | `Adapter` type implementing `interpreter.Cortex` over `*cortex.Cortex`. |
| `args.go` | Maps SKILL.mtx arg dicts → `query.Query` / `cortex.ContextOpts`. |
| `bundle.go` | Formats `*cortex.Bundle` into the text shape for `{cortex.bundle}` prompt interpolation. |
| `bridge_test.go` | Integration tests against a real Pebble-backed cortex. |
| `cmd/mclc-cortex/` | `mclc compile`-equivalent CLI that wires the bridge end-to-end. |

## SKILL.mtx → bridge call mapping

`resolve slot.X <- cortex.find(type=Fact, near="...", limit=5)`
→ `Adapter.Find(ctx, {"type":"Fact","near":"...","limit":"5"})`

Supported `cortex.find` args:

| Key | Value | Notes |
| --- | --- | --- |
| `type` | `Identity|Fact|Preference|Belief|Event|Goal|Constraint|Capability|Pattern` | Single type. |
| `tag` | string | Single tag; repeats not yet supported. |
| `near` | NL phrase | Requires embedder running on the cortex. |
| `limit` | positive int | Defaults to 10 (override via `WithDefaultLimit`). |
| `form` | `short|medium|full` | Defaults to medium. |
| `late` | bool | Override Adapter's LateBinding default. |
| `include_tombstoned` | bool | Default false. |

`resolve slot.X <- cortex.resolve(<expr>)`
→ `Adapter.Resolve(ctx, expr)`

- If `expr` begins with `matrix://cortex/`: exact URI resolve.
- Otherwise: top-1 near-search fallback (requires embedder).

Supported `cortex.context` args:

| Key | Value |
| --- | --- |
| `verb` | closed D7 verb name |
| `objects` | `kind:ref,kind:ref` or `kind:ref;kind:ref` |
| `budget_tokens` | int |
| `outcome_limit` | int |
| `form` | `short|medium|full` |

## Invariants enforced

- **Compile-time discipline (D13).** `Adapter.Find` defaults
  `LateBinding=false`, so the cortex journal does not grow during
  `mclc compile`. Verified by `TestFindCompileTimeDoesNotJournal`.
- **Context purity (research/04 §12.1).** `Adapter.Context` is a pure
  read; `OverallRoot()` is unchanged across the call. Verified by
  `TestContextPureReadDoesNotMutateRoot`.
- **No silent typos.** Unknown arg keys, unknown type names, unknown
  verbs, and unknown obj kinds return errors so SKILL.mtx mistakes
  surface at compile.
- **Embedder gating.** Near-based queries return cortex's own
  "requires StartEmbedder" error verbatim when no embedder is running.

## CLI: `mclc-cortex`

```
go run ./cmd/mclc-cortex \
  -skill ../skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb build \
  -cortex-root /tmp/mclc-cortex-demo \
  -dry-run
```

Add `-with-embedder` (hash-stub) or `-with-fireworks-embedder` (real
nomic-embed-text-v1.5) to exercise `cortex.find(near=...)` resolution.

## Module layout

```
matrix/bridge   (this module)
   ├── go.mod   (replaces ../cortex and ../MCL)
   ├── *.go     (library)
   └── cmd/mclc-cortex/  (driver binary)

matrix/cortex   (sibling; no import of matrix/mcl or matrix/bridge)
matrix/mcl      (sibling; no import of matrix/cortex or matrix/bridge)
```

The `replace` directives in `go.mod` point at sibling working trees in
the same repo. Production build pipelines can publish each module under
its own VCS path and replace the replaces with explicit versions.
