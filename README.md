<!--
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
  <img src="https://img.shields.io/badge/Go-1.22-004CED?logo=go&logoColor=white" alt="Go 1.22" />
  <img src="https://img.shields.io/badge/Modules-9-004CED" alt="9 Go modules" />
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

Matrix ships **two agent rails** over one shared memory + execution substrate:

- **Neo** — the *default* conversational tool-calling agent: familiar, robust, fully
  permissive on reversible work (shell, code, fetch, web). It delegates anything
  monetary or irreversible to the rigorous rail.
- **MCL pipeline** — the *rigorous* rail: natural language → typed Intent IR →
  plan → replayable walk, for high-stakes / on-chain / irreversible work.

The stack is layered:

| Layer        | Role                                                                                          |
| ------------ | --------------------------------------------------------------------------------------------- |
| **MCL**      | Protocol turning NL → typed Intent IR. Verbs are closed (10), objects are closed (8 kinds).   |
| **cortex**   | Per-actor typed memory graph on Pebble. Append-only journal, Merkle-anchored snapshots.       |
| **bridge**   | Glue wiring the MCL compiler's `Cortex` interface to a live cortex instance.                  |
| **executor** | Plan walker, lifecycle machine, MCP tool dispatch, per-user daemon, Liaison narrator, e2e harness. |
| **neo**      | The default conversational agent — tool-calling loop with paged cortex memory.                |
| **gateway**  | Metered LLM proxy + PAX credit ledger (free-tier whitelist + rate card).                       |
| **router**   | Per-user Fly Machine provisioning + wake-then-reverse-proxy front door.                        |
| **deus**     | Agent-service marketplace: registry, discovery, metered invoke, EIP-712 receipts, hosting.    |
| **uwac**     | Universal Web Agent Connector — OAuth vault → per-user MCP tools.                              |
| **tachyon**  | Agent-native Solidity/EVM engine — compile / test / simulate / deploy.                         |
| **agents**   | DID-bound manifests. Protocol, not personality.                                               |
| **tools**    | MCP servers the agents call (the chain-touching ones included).                               |


---

## Repository layout

```text
matrix/
├── cortex/        Typed per-actor memory graph (Pebble) + replay invariant + Merkle snapshots
├── MCL/           MatrixScript compiler — lexer/parser/validator/canonical/interpreter + Intent IR + envelopes + LLM client
├── bridge/        MCL ↔ cortex adapter (separate Go module; replace-directive linked)
├── executor/      Lifecycle machine, runtime walker, MCP client + tool registry, per-user daemon (+ Liaison narrator), e2e harness
├── neo/           Neo — the default conversational tool-calling agent (delegates monetary/irreversible work to MCL)
├── gateway/       Metered LLM proxy + PAX credit ledger (free-tier whitelist + rate card)
├── router/        Per-user Fly Machine provisioning + wake-then-reverse-proxy front door
├── deus/          Agent-service marketplace: registry, discovery, metered invoke, EIP-712 receipts, hosted execution
├── uwac/          Universal Web Agent Connector — OAuth vault → per-user MCP tools (building)
├── tachyon/       Agent-native Solidity/EVM engine — compile/test/simulate/deploy (git submodule)
├── chronos/       Centralized agent scheduler / wake-up system (design frozen)
├── agents/        DID-bound agent manifests (default.json, neo.json) + MCP server templates
├── tools/         MCP servers — paxeer, browser, tachyon, deus, uwac, web-search, media, cortex
├── skills/        SKILL.mtx capability manifests + SKILL.md prose bodies
├── client/        Matrix consumer app (Next.js / React)
├── marketplace/   Deus marketplace + developer dashboard (React Router on Cloudflare Workers)
├── deploy/        Daemon image, Fly Machine deploy, shared-service images, box install scripts
├── rules/         Identity + per-language coding rules
├── knowledge/     Canonical refs (matrix.kvx project state, models)
└── runs/          Transient harness output (gitignored)
```

### The Go modules

The root `Makefile` drives **nine** sibling Go modules — MCL, bridge, executor, gateway,
router, cortex, tachyon, deus, neo — alongside **uwac** (and **chronos**, in progress).
Each is independently `go build`/`go test`able with its own `go.mod`; cross-module imports
use `replace` directives during development and explicit versions on publish.

```text
cortex   → matrix/cortex                    typed memory graph, replay invariant, snapshots/Merkle
MCL      → matrix/mcl                       compiler + Intent IR + envelopes + LLM client
bridge   → matrix/bridge                    MCL ↔ cortex adapter
executor → matrix/executor                  plan walker, lifecycle, MCP dispatch, daemon, Liaison
neo      → matrix/neo                       default conversational agent
gateway  → matrix/gateway                   metered LLM proxy + PAX credit ledger
router   → matrix/router                    per-user Fly Machine routing
deus     → github.com/paxlabs-inc/deus      agent-service marketplace
uwac     → github.com/paxlabs-inc/uwac      external app connectors
tachyon  → github.com/paxlabs-inc/tachyon-tools   Solidity/EVM engine (submodule)
```

---

## Quickstart

### Prerequisites

- Go **1.22+** (toolchain pinned across every module).
- `make` (GNU make 4.x).
- For the MCP-server-driven flows: `node` ≥ 20, `npx`, `python3` ≥ 3.11, `uv`.
- For the daemon image: `docker` with buildx.

### Clone and build

```bash
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core
make build           # builds all nine Go modules
make install         # drops the runnable CLIs into ./bin
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

| Method | Path             | Purpose                                  |
| ------ | ---------------- | ---------------------------------------- |
| GET    | `/healthz`       | Liveness + SSE broker stats              |
| POST   | `/chat`          | Converse with the agent (Neo front door) |
| GET    | `/events`        | Server-Sent Events tail (transcript)     |
| POST   | `/messages`      | Submit a prose message (rigorous rail)   |
| GET    | `/intents/{id}`  | Read intent envelope chain by ID         |
| GET    | `/me`            | Per-user settings + identity             |
| POST   | `/shutdown`      | Graceful drain                           |

---

## Documentation

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — system map, module boundaries, key invariants
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — dev setup, test discipline, commit style
- [`SECURITY.md`](./SECURITY.md) — vulnerability disclosure
- [`CHANGELOG.md`](./CHANGELOG.md) — Keep-a-Changelog release notes
- [`deploy/daemon/README.md`](./deploy/daemon/README.md) — image build + Fly deploy
- [`docs/MCL-docs/README.md`](./docs/MCL-docs/index.md) — MCL documentation
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
