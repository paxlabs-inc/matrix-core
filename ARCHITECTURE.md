# Matrix Architecture

This is the high-level map. The canonical detailed source-of-truth is split:

- **Design decisions** live in [`research/`](./research) (chapters 00-decisions
  through 06-agents).
- **Project state**, including phase status, locked Q-decisions, invariants,
  and session history, lives in
  [`knowledge/matrix.kvx`](./knowledge/matrix.kvx).
- **This file** is a navigation index for new contributors.

If anything below contradicts `matrix.kvx`, the kvx wins.

## Layered model

```text
                                  user prose
                                      |
                                      v
              +-----------------------+------------------------+
              |                 MCL compiler                   |
              |  lexer -> parser -> validator -> canonical     |
              |    \                                  /        |
              |     +-> interpreter <- LLM <- grammar          |
              |              |                                 |
              |              v                                 |
              |          Intent IR  (closed verb, closed kind) |
              +-----------------+------------------------------+
                                |
                                v
                          +-----+------+
                          |   bridge   |
                          | MCL.Cortex |   (adapter)
                          +-----+------+
                                |
                                v
   +------------------+    +----+----+    +-----------------+
   |  agent manifest  |--->| cortex  |<---| executor walker |
   |  (DID-bound)     |    | (Pebble)|    | + MCP dispatch  |
   +------------------+    +----+----+    +-----------------+
                                |                  |
                                |                  v
                                |          +---------------+
                                |          |  MCP servers  |
                                |          |  (subprocess) |
                                |          +-------+-------+
                                |                  |
                                +------ events ----+
                                          |
                                          v
                                  attest + EMA loop
```

## The four Go modules

Each is independently `go build`/`go test`able with its own `go.mod`.

### `cortex/`

Per-actor typed memory graph on Pebble. **Authoritative**, byte-deterministic,
replay-rebuildable.

Key surfaces:

| Package        | Role                                                                |
| -------------- | ------------------------------------------------------------------- |
| `cortex.go`    | Facade (`Write`, `Resolve`, `Update`, `UpdateHead`, `Tombstone`, `Find`, `Context`, `Compact`, `Attest`, `AddEdge`, `RemoveEdge`, `Snapshot`, `OverallRoot`, `Rebuild`) |
| `store/`       | Pebble shell + `BeginWrite` atomic batch + `JournalHook`            |
| `journal/`     | Append-only write log (canonical CBOR entries)                      |
| `memory/`      | 9-type taxonomy + canonical CBOR + validate                         |
| `keys/`        | Key-prefix encoding (memory namespaces)                             |
| `query/`       | Predicate AST + planner + `Find` with `OrderBy`/`Form`/`Budget`     |
| `forms/`       | Per-type deterministic `Render` for `short`/`medium`/`full`         |
| `salience/`    | 5-factor cold score + EMA weight learner                            |
| `embed/`       | Embedder interface + Hash stub + real Fireworks API client          |
| `vector/`      | Pure-Go HNSW + persistence                                          |
| `snapshot/`    | MMR + SMT-256 + `SnapshotManifest` + `OverallRoot`                  |
| `scope/`       | Sub-agent `CortexScope` (Merkle-proof-bounded reads)                |
| `replay/`      | `DropDerived` + `Rebuild` + §13.4 verifier                          |
| `compact.go`   | Summarise-and-link checkpoints                                      |
| `attest.go`    | `cortex.Attest` + `KindAttest` + EMA training emission              |
| `ratelimit.go` | Token-bucket gates for scope-violation + attest                     |

**Load-bearing invariants** (all enforced in code; never relax without a
schema bump):

- Byte-sort == numeric-sort (BE uint64 keys).
- One Pebble DB per actor.
- Journal `seq` monotonic and gap-free.
- Every store mutation journals (`ErrBatchNoJournal` enforces).
- Atomic batch: journal + head + version + idx/* + salience commit-or-abort.
- `#latest` rejected at URI parse (D13).
- Tombstoned blocks `Update`; old versions still resolvable (audit trail).
- Find rejects unbounded (need `Limit` OR `BudgetTokens`) and too-broad
  (need `Type` OR `HasTag`).
- Compile-time `Find` does NOT journal; `LateBinding=true` does.
- §13.4: drop derived, walk `j/`, expect byte-identical `OverallRoot`.

Read `research/04-cortex.md` for the full spec.

### `MCL/`

MatrixScript compiler. Turns prose + a `SKILL.mtx` + a verb hint into an
Intent IR.

| Package         | Role                                                            |
| --------------- | --------------------------------------------------------------- |
| `mtx/token`     | Token kinds (~120)                                              |
| `mtx/lexer`     | CRLF→LF normalised, `§SECTION`, 2-space INDENT, `matrix://` URIs |
| `mtx/ast`       | `File` / `Section` / `OnBlock` / `PromptBlock` / etc.           |
| `mtx/parser`    | Recursive-descent over the EBNF in `mtx/grammar.bnf`            |
| `mtx/validator` | 10 spec rules (`spec.md` §11)                                   |
| `mtx/canonical` | AST sha256 hash (excludes comments + blank + `§HASH`)           |
| `mtx/interpreter` | Walks `§PROCEDURE` on-blocks first-match-wins                 |
| `ir/`           | `Intent` + `PlanTree` types + canonical JSON + hash             |
| `envelope/`     | 15 typed body structs + canonical CBOR + ed25519 + JSON-on-disk |
| `llm/`          | OpenAI-compat client + provider detection + grammar pinning     |
| `cmd/mclc/`     | `compile`/`validate`/`hash`/`parse` CLI                         |

Pinned design:

- 10-verb closed vocab (D7): `find acquire build modify deliver analyze
  negotiate schedule monitor delegate`.
- 8-`obj_kind` closed v1 vocab: `service model agent knowledge intent asset
  plan capability`.
- Canonical AST hashing — comments and whitespace do not affect the
  digest, so reformat-safe.
- D11 seed: `sha256(intent.id || actor || cortex_snapshot_hash ||
  mtx_digest || model_digest)`.

### `bridge/`

Three-line summary: wires the MCL compiler's `Cortex` interface to a live
`*cortex.Cortex`. Lives as a separate Go module so MCL and cortex stay
closed under their own dep graphs.

- `bridge.go` — adapter + `Find` / `Resolve` / `Context` + `WithDefaultLimit`
  / `WithDefaultForm` / `WithLateBinding`
- `args.go` — query/`ContextOpts` builders, closed-type/closed-kind
  validation
- `bundle.go` — deterministic `FormatBundle` for `{cortex.bundle}` prompt
  interpolation
- `cmd/mclc-cortex/` — `mclc compile` with a live cortex actor

### `executor/`

Where intents become work.

| Package         | Role                                                          |
| --------------- | ------------------------------------------------------------- |
| `lifecycle/`    | 8-state machine (drafting → completed/failed/cancelled)       |
| `mcp/`          | JSON-RPC 2.0 client + stdio + streamable-HTTP transports + `Manager` |
| `tool/`         | `Tool` interface + `Registry` + URI scheme + capability gate  |
| `runtime/`      | Plan walker (DFS, goroutine-per-Parallel) + skill loader      |
| `materiality/`  | D9 §18.1 classifier (8 rules)                                 |
| `cmd/mcl-tools/`   | `verify`/`list`/`describe`/`call` subcommands              |
| `cmd/mcl-execute/` | `walk`/`classify`/`loader`/`daemon` subcommands            |
| `cmd/mcl-e2e/`     | Live end-to-end harness (3-run sweep, 75 assertions)       |

The walker is pluggable: `StepHandler` / `SubDispatchHandler` /
`GateHandler` are interfaces with sane defaults (Noop / NotImplemented /
Noop). Production wires the LLM `StepHandler` from `cmd/mcl-execute`.

The daemon is single-flight (one user per process, `sync.Mutex.TryLock`
returns 409 Busy on concurrent `/messages`). SSE broker per-subscriber
buffered channels with drop-on-backpressure.

## Cross-cutting flows

### Compile (cold-start)

1. `mclc compile` reads `SKILL.mtx` and prose.
2. Lexer + parser + validator + canonical hash on the AST.
3. Interpreter walks `§PROCEDURE`:
   - `on verb=...` matches first-true-wins.
   - `prompt { ... }` is interpolated with `{prose}` / `{verb}` /
     `{cortex.bundle}` / `{slot.X}`.
   - `resolve slot.X <- cortex.find(...)` calls into the bridge.
   - `unknown slot.X` registers blocking gaps if the slot is still empty.
4. Frame is extracted under a grammar-constrained decode against the
   compiler model (default `accounts/fireworks/models/deepseek-v4-flash`,
   temp=0, seed=42, `intent_frame@1` JSON schema).
5. Intent IR is hashed deterministically (canonical JSON + sha256).

### Execute

1. `mcl-execute walk` synthesises a `PlanTree` from the Intent under the
   executor model (default `deepseek-v4-pro`, `plan_tree@1` JSON schema).
2. The walker DFS-walks the tree:
   - `sequential` / `parallel` branch nodes.
   - `tool_call` -> `Registry.Get(uri)` -> `Tool.Call(args)`.
   - `step` -> `StepHandler.HandleStep(prompt)` -> executor LLM.
   - `sub_dispatch` -> opt-in via `-allow-sub-dispatch`.
   - `gate` -> `GateHandler.HandleGate(question)` (stdin in CLI mode).
3. Each step journals a `cortex.Event` memory.
4. Lifecycle envelopes are signed (ed25519) and written under
   `journal/<intent_id>/<seq>-<kind>.json`.
5. On terminal state, `cortex.Attest(IntentID, Outcome, Reason, Cited[],
   CreatedBy)` writes `KindAttest` + `KindLearnWeights` in one atomic
   Pebble batch. EMA pulls per-actor weights toward (or away from)
   the cited memories' factor profile.

### Replay (the §13.4 invariant)

1. `cortex-shell rebuild -verify-only`:
   - Capture `Pre = OverallRoot()`.
   - `DropDerived` (deletes `vec/`, `idx/`, `salience/`, `accum/`, plus
     two `meta/embed_*` cursors).
   - Walk `m/` heads, `e/from/` edges, `j/<seq>` journal entries in
     order, re-emit every derived index and SMT/MMR entry.
   - `Post = OverallRoot()`.
   - Require `Pre == Post`. Any drift is a bug.

This is run on every PR via the `replay-invariant` CI job.

## Deployment surface (`deploy/`)

```text
deploy/
├── daemon/
│   ├── Dockerfile         multi-stage: golang:1.22-bookworm builder -> debian:bookworm-slim
│   ├── entrypoint.sh      idempotent /data layout + MinIO pull + mcl-execute daemon
│   ├── .dockerignore
│   ├── fly.toml.tmpl      per-user Fly Machine template (auto_stop_machines=suspend)
│   └── README.md
└── box/
    ├── bootstrap.sh       Ubuntu 24.04 one-shot setup (idempotent)
    ├── minio/             MinIO server + client config
    ├── postgres/          per-user state metadata
    ├── router/            matrix-router systemd unit (binary not yet built)
    ├── wireguard/         WG mesh config notes
    └── README.md
```

Production topology (lock S25Q1-Q9):

- **Compute**: one Fly Machine per user, auto-suspended when idle, per-Machine
  10gb Volume mounted at `/data`.
- **State**: dedicated Paxeer Ubuntu box hosts MinIO (per-user state
  snapshots) + Postgres (user → machine-id mapping) + `matrix-router`.
- **Network**: WireGuard mesh between Fly Machines and the box; only
  matrix-router's `:443` is public.
- **Auth**: Supabase Auth -> JWT -> matrix-router validates -> wakes the
  user's Machine via Fly Machines API -> reverse-proxies.
- **LLMs**: daemon Machines call Fireworks/Together directly; central
  LLM gateway deferred.

## What is NOT in the architecture (yet)

- **Chain anchoring**. `tools/attest`, `tools/argus`, `tools/orob`,
  `tools/plv`, `tools/pofq`, `tools/registry`, `tools/payments`,
  `tools/chain` all deferred to v1.1 (chain explicitly dropped from v1
  scope, sess#20).
- **Cross-agent sub-dispatch**. v1 only does in-process sub-dispatch under
  the same agent. Cross-agent with `CortexScope` Merkle-proof handoff is
  v1.1 (Q6).
- **Policy gate async/timeout race**. v1 ships synchronous wait (Q10).
- **Dynamic MCP tool discovery**. v1 uses static manifest pinning (Q21).
- **Streaming progress over SSE/HTTP**. v1 ships JSONL transcript + cortex
  Event memory; SSE replay-from-cursor is v1.1 (Q14).
- **Phase 13 V-weight gating**. Real embedder is live, but `Find`
  vector-ranking is not yet on the primary loop.

## Where to start reading

- New to the project? `README.md` -> `research/01-foundations.md` ->
  `research/02-protocol.md` -> `research/04-cortex.md`.
- New to cortex? `research/04-cortex.md` -> `cortex/cortex.go` ->
  `cortex/store/store.go` -> `cortex/replay/replay.go`.
- New to MCL? `MCL/mtx/spec.md` -> `MCL/mtx/grammar.bnf` ->
  `MCL/core/*.mtx` -> `MCL/mtx/interpreter/interpreter.go`.
- New to executor? `executor/runtime/walker.go` ->
  `executor/cmd/mcl-execute/walk_cmd.go` ->
  `executor/cmd/mcl-e2e/README.md`.
- New to deploy? `deploy/daemon/README.md` -> `deploy/box/README.md`.
