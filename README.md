999<!--
parent:
  order: false
-->
<p align="center">
  <img src="https://zezsqawedbikldiedlse.supabase.co/storage/v1/object/public/cdn.deus.paxeer.app/bcd6e442-788a-4377-b605-42ce2896d32e.png" alt="Paxeer Network" width="1200">
</p>
<p align="center">
<p align="center">
  <img src="https://img.shields.io/badge/Project-Matrix-FFFFFF?style=for-the-badge&labelColor=004CED" alt="Project: Matrix" />
  <img src="https://img.shields.io/badge/Built_by-PaxLabs-004CED?style=for-the-badge&labelColor=000000" alt="Built by PaxLabs" />
  <img src="https://img.shields.io/badge/License-Matrix--Protocol-004CED?style=for-the-badge&labelColor=000000" alt="License: Matrix-Protocol" />
  <img src="https://img.shields.io/badge/Status-Active-00C896?style=for-the-badge&labelColor=000000" alt="Status: Active" />
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Chain-HyperPaxeer-004CED?style=for-the-badge&labelColor=000000" alt="Chain: HyperPaxeer" />
  <img src="https://img.shields.io/badge/Chain_ID-125-FFFFFF?style=for-the-badge&labelColor=004CED" alt="Chain ID: 125" />
  <img src="https://img.shields.io/badge/Block_Time-400ms-00C896?style=for-the-badge&labelColor=000000" alt="Block Time: 400ms" />
  <img src="https://img.shields.io/badge/Finality-400ms-00C896?style=for-the-badge&labelColor=000000" alt="Finality: 400ms" />
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/ci.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/ci.yml/badge.svg?branch=main" alt="ci" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/lint.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/lint.yml/badge.svg?branch=main" alt="lint" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/actions/workflows/docker.yml"><img src="https://github.com/paxlabs-inc/matrix-core/actions/workflows/docker.yml/badge.svg?branch=main" alt="docker" /></a>
  <img src="https://img.shields.io/badge/Go-1.21-004CED?logo=go&logoColor=white" alt="Go 1.21" />
  <img src="https://img.shields.io/badge/Modules-4-004CED" alt="4 Go modules" />
</p>

---

## What is Matrix?

Matrix is the cognition and UX layer on top of [Paxeer Network](https://paxeer.app).
It turns natural-language requests from non-developers into a typed, inspectable, correctable
**Intent IR** that an agent can actually execute — without the four classic failure modes
that break non-dev ↔ agent workflows today:

1. **Prompt fragility** — small rewordings yield wildly different outputs.
2. **Intent loss** — natural language doesn't survive multi-step execution.
3. **No shared ontology** — user and agent disagree about which entity is being referred to.
4. **No structured correction** — drift forces the user to rewrite from scratch.

The solution is layered:

| Layer       | Role                                                                                          |
| ----------- | --------------------------------------------------------------------------------------------- |
| **MCL**     | Protocol turning NL → typed Intent IR. Verbs are closed (10), objects are closed (8 kinds).   |
| **cortex**  | Per-actor typed memory graph on Pebble. Append-only journal, Merkle-anchored snapshots.       |
| **bridge**  | Glue wiring the MCL compiler's `Cortex` interface to a live cortex instance.                  |
| **executor**| Plan walker, lifecycle machine, MCP tool dispatch, long-running daemon, end-to-end harness.   |
| **agents**  | DID-bound manifests. Protocol, not personality.                                               |
| **tools**   | Only layer permitted to touch the Paxeer chain.                                               |


---

## Repository layout

```text
matrix/
├── cortex/        Pebble-backed typed memory graph + replay invariant + snapshots/Merkle
├── MCL/           MatrixScript compiler — lexer/parser/validator/canonical/interpreter + IR + envelope + LLM client
├── bridge/        MCL ↔ cortex adapter (separate Go module; replace-directive linked)
├── executor/      Lifecycle, runtime walker, MCP client + tool registry, mcl-execute CLI, daemon, e2e harness
├── deploy/        Dockerfile (matrix-daemon), Fly.io Machine template, Paxeer storage box bootstrap
├── skills/        159 SKILL.mtx capability manifests + SKILL.md prose bodies
├── agents/        Agent manifests (default.json + MCP server templates)
├── rules/         Identity + per-language coding rules (Andrew profile + 14 language sets)
├── tools/         Chain-touching wrappers (deferred to v1.1) + skill corpus utilities
├── research/      Design docs (00-decisions through 06-agents)
├── knowledge/     Canonical refs (matrix.kvx — load-bearing project state, models.kvx, whitepaper)
├── journal/       Unified temporal store (plan/, notes/, thoughts/, logs/)
├── workflows/     Multi-step orchestrations
├── router/        Per-user Fly Machine routing (Phase 4 of deploy)
└── runs/          Transient harness output (gitignored)
```

### The four Go modules

Each top-level module is independently `go build`/`go test`able and has its own `go.mod`.
Cross-module imports use `replace` directives during development; production publish swaps
them for explicit versions.

```text
cortex   → matrix/cortex   (~18.7k prod LOC, 16 packages, 365+ tests)
MCL      → matrix/mcl      (~6.3k  prod LOC, 11 packages, 174+ tests)
bridge   → matrix/bridge   (~1.0k  prod LOC,  2 packages,  23  tests)
executor → matrix/executor (~12.5k prod LOC,  8 packages,  ~100 tests)
```

---

## Quickstart

### Prerequisites

- Go **1.21** (toolchain pinned across every module).
- `make` (GNU make 4.x).
- For the MCP-server-driven flows: `node` ≥ 20, `npx`, `python3` ≥ 3.11, `uv`.
- For the daemon image: `docker` with buildx.

### Clone and build

```bash
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix
make build           # builds all 4 modules
make install         # drops the 8 user-facing CLIs into ./bin
make test            # `go test -count=1 -race ./...` per module
make vet             # `go vet ./...` per module
make ci              # gofmt-check + vet + tests (mirrors GitHub Actions)
```

Need `golangci-lint` locally:

```bash
make lint-install    # pinned to v1.61.0
make lint
```

### Configure secrets

```bash
cp .env.example .env
# fill in FIREWORKS_API_KEY / TOGETHER_API_KEY for any non-dry-run compile
# fill in MATRIX_DAEMON_TOKEN if running the daemon with auth
```

`.env` is gitignored; `.env.example` documents every variable Matrix reads.

### Compile your first intent

```bash
./bin/mclc compile \
  -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb  build
```

With `FIREWORKS_API_KEY` set the compiler emits a real Intent Frame (verb, typed objects,
blocking unknowns). Without keys it falls back to dry-run mode and prints the
fully-interpolated prompt structure.

### Drive an end-to-end walk

```bash
./bin/mcl-execute walk \
  -prose       "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

This loads the agent manifest, spawns the MCP servers it declares, compiles the prose into
an Intent + PlanTree, walks the plan, journals every step as a cortex Event, and ends with
`cortex.Attest` writing `KindAttest` + `KindLearnWeights` atomically.

### Run the daemon

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

Routes:

| Method | Path             | Purpose                              |
| ------ | ---------------- | ------------------------------------ |
| GET    | `/healthz`       | Liveness + SSE broker stats          |
| GET    | `/events`        | Server-Sent Events tail (transcript) |
| POST   | `/messages`      | Submit a prose message               |
| GET    | `/intents/{id}`  | Read intent envelope chain by ID     |
| POST   | `/shutdown`      | Graceful drain                       |

---

## Status

- **Cortex**: v1 functionally complete. Phases 1–14 done (Pebble shell, typed memory,
  replay invariant, snapshots/Merkle, salience EMA, rate-limiting, real embedder).
- **MCL**: Go runtime complete (lexer, parser, validator, canonical, interpreter, IR,
  envelope), real LLM wired via OpenAI-compat (Fireworks/Together).
- **bridge**: Adapter live. Compile-time `cortex.Find`/`Resolve`/`Context` works against
  a live cortex actor.
- **executor**: Plan walker, MCP client + tool registry, materiality classifier (D9),
  daemon HTTP+SSE server, mcl-execute CLI, mcl-e2e live harness — all in.
- **deploy**: Daemon Dockerfile, Fly Machines template, Paxeer storage box bootstrap
  authored. Live deploy execution = next session.

Full per-phase status table lives in `knowledge/matrix.kvx`.

---

## Documentation

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — system map, module boundaries, key invariants
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — dev setup, test discipline, commit style
- [`SECURITY.md`](./SECURITY.md) — vulnerability disclosure
- [`CHANGELOG.md`](./CHANGELOG.md) — Keep-a-Changelog release notes
- [`deploy/daemon/README.md`](./deploy/daemon/README.md) — image build + Fly deploy
---

## License

Source-available under the **Matrix-Protocol License** — see [`LICENSE.md`](./LICENSE.md).

Short version: read, use, deploy, integrate freely. If you **modify and redistribute**,
release your changes under the same license. A commercial licence is required once you
cross the commercial trigger thresholds (Charged Fees > USD 100k / 12 months **or**
Liquidity Under Control > USD 10M). The non-binding summary in `LICENSE.md` is for
convenience; the license body is authoritative.

---

<p align="center">
  Built by <a href="https://labs.paxeer.app">Paxlabs Inc.</a> · <code>SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol</code>
</p>
