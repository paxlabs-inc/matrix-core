<!--
parent:
  order: false
-->
<p align="center">
<img src="MATRIX.gif" alt="Matrix" >
</p>

<p align="center">
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Project-Matrix-0A0A0A?style=flat-square&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIxNiIgaGVpZ2h0PSIxNiIgdmlld0JveD0iMCAwIDI0IDI0IiBmaWxsPSJub25lIiBzdHJva2U9IiNmZmZmZmYiIHN0cm9rZS13aWR0aD0iMiIgc3Ryb2tlLWxpbmVjYXA9InJvdW5kIiBzdHJva2UtbGluZWpvaW49InJvdW5kIj48Y2lyY2xlIGN4PSIxMiIgY3k9IjEyIiByPSIxMCIvPjxwYXRoIGQ9Ik0xMiAxNnYtNCIvPjxwYXRoIGQ9Ik0xMiA4aC4wMSIvPjwvc3ZnPg==&logoColor=white" alt="Project: Matrix" /></a>
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Built%20by-PaxLabs-0A0A0A?style=flat-square&logoColor=white" alt="Built by PaxLabs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Matrix--Protocol-0A0A0A?style=flat-square" alt="License: Matrix-Protocol" /></a>
  <a href="CHANGELOG.md"><img src="https://img.shields.io/badge/Version-1.0.0-0A0A0A?style=flat-square" alt="Version: 1.0.0" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Status-Active-0A0A0A?style=flat-square" alt="Status: Active" /></a>
  <a href="https://paxeer.app"><img src="https://img.shields.io/badge/Layer-Paxeer%20Network-0A0A0A?style=flat-square" alt="Paxeer Network" /></a>
</p>

<p align="center">
  <a href="https://github.com/paxlabs-inc/matrix-core/stargazers"><img src="https://img.shields.io/github/stars/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Stars" /></a>
  <a href="https://github.com/paxlabs-inc/matrix-core/network/members"><img src="https://img.shields.io/github/forks/paxlabs-inc/matrix-core?style=flat-square&color=0A0A0A" alt="GitHub Forks" /></a>
  <a href="https://docs.matrixmcl.com"><img src="https://img.shields.io/badge/Docs-docs.matrixmcl.com-0A0A0A?style=flat-square" alt="Documentation" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-38.7%25-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go" />
  <img src="https://img.shields.io/badge/Solidity-26.3%25-363636?style=flat-square&logo=solidity&logoColor=white" alt="Solidity" />
  <img src="https://img.shields.io/badge/JavaScript-16.9%25-F7DF1E?style=flat-square&logo=javascript&logoColor=black" alt="JavaScript" />
  <img src="https://img.shields.io/badge/TypeScript-11.1%25-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript" />
  <img src="https://img.shields.io/badge/HTML-5.5%25-E34F26?style=flat-square&logo=html5&logoColor=white" alt="HTML" />
  <img src="https://img.shields.io/badge/Python-0.5%25-3776AB?style=flat-square&logo=python&logoColor=white" alt="Python" />
</p>

---

<h2 align="center">An agent framework and cognition layer for LLMs.</h2>

<p align="center">
  Matrix takes an LLM past chat and into execution across the digital realm <br/>
  and lets humans and machines coordinate on the work that has to be exact.
</p>

---

## What is Matrix?

Matrix is an agent framework, built by PaxLabs, that takes an LLM past chat and into real execution: files, code, web, on-chain operations, payments, and smart contracts, all driven by natural language.

Two things sit at the **core of the system**: **Neo**, the default agent you talk to, and **Neocortex**, the deterministic evidence engine that preserves what Neo saw, did, and still owes the user. The existing Go Cortex remains available as a compatibility and rollback substrate until the explicit cutover gate. Everything else — the MCL rigor pipeline, the executor, settlement, scheduling, and the tool ecosystem — hangs off that core.

Most agent stacks break on consequential work because they carry natural language all the way down, and prose is a leaky channel: fine for chat, not fine when an agent is moving funds, performing an irreversible write, or holding something confidential. Matrix keeps everyday reversible work in the Neo loop and escalates — the moment work becomes monetary, irreversible, or compliance-sensitive — to the MCL pipeline, where intents are compiled, typed, signed, and replayed, so ambiguity never reaches the parts that must be exact.

## Neo — where everything starts

Neo is the entry point. You talk to Neo; Neo does the work.

- **Recursive tool-calling loop.** The model emits text and tool-call intents; the harness dispatches tools (shell, code, web search, files, browser, chain reads, media) and feeds results back. Per-turn step budgets, stall detection, and honest partials on exhaustion — never fabricated success.
- **Continuous evidence via Neocortex.** A per-user `cortexd` process records typed conversation, intent, work, tool, and memory events in one append-only log. Deterministic projections reconstruct the active thread, open loops, work ledger, beliefs, exact entity/vector/lexical recall, and temporal descent after a crash or respawn.
- **Agent swarms.** Neo can spawn concurrent sub-agents for parallel work, each with an isolated context window, a restricted tool surface, and a bounded timeout. No recursion, no fork-bombs.
- **Automatrix.** Autonomous execution: Neo can schedule proactive tasks, wake on timers, and run background work on a restricted surface with no value-moving tools.
- **Tool transparency.** Every tool call is surfaced as a ToolEvent with its name, arguments, and result — users see the real evidence behind an answer, not just a synthesized paragraph.

## Neocortex — the evidence core

Neocortex was built because Neo's reliability problem was not simply model intelligence: conversation, intent, tool evidence, and unfinished work could be fragmented or distorted across context-window assembly, overflow, crash, and respawn boundaries. Neocortex makes those boundaries deterministic. Its C++23 single-writer engine treats one typed event log per actor as the only ground truth; LMDB projections rebuild the conversation, intent frame, work ledger, beliefs, indexes, and checkpoints from that log. A BLAKE3 MMR provides tamper evidence, XChaCha20-Poly1305 seals records below the hash boundary, and the activation composer guarantees that resident identity, current intent, and the work-ledger tail are never trimmed.

Neo connects through the Go `cortexclient` seam over a capability-scoped Unix socket. `cortexd` has no model and no network client. It is supervised independently, and acknowledged writes, checkpoints, citations, and recovery survive process restart. The Go Cortex remains intact as the default compatibility path until the owner-approved production cutover; select Neocortex explicitly with `NEO_MEMORY_SUBSTRATE=neocortex`.

## Two rails, one substrate

Matrix runs two execution rails over the shared evidence and tool substrate:

- **Neo (conversational).** The default agent. A recursive tool-calling loop with persistent memory, permissive on reversible work — shell, code, web search, file operations, browser, media — that delegates high-stakes work to the MCL rail automatically. The conversation transcript *is* the state; the harness is the only effector.
- **MCL (rigorous).** Compiles natural language into a typed Intent IR, synthesizes a plan, and walks it deterministically. Every step is signed, journaled, and replayable. Used for on-chain transactions, irreversible operations, and monetary work.

Escalation is a tool call: when a task crosses into the consequential, Neo invokes `core_execute`, which hands it to the co-located MCL daemon and returns a signed, journaled result — then control returns to the conversation. And the loop never fabricates completion: on a stalled or budget-exhausted turn it surfaces an honest partial, and a supervisor respawns a fresh agent over the durable transcript to keep going rather than reporting a fake "done".

## How It Fits Together

```
                         +-------------------+
                         |       User        |
                         +---------+---------+
                                   |
                                   v
                     +-------------+-------------+
                     |            Neo            |
                     |   recursive tool loop     |
                     |   swarms · Automatrix      |
                     +----+---------+--------+----+
                          |         |        |
                    reversible    memory   escalation
                       work         |        |
                          v         v        v
                     +--------+ +--------+ +-----------+
                     | Tools  | |Neocortex| |    MCL    |
                     | shell  | | memory | | typed IR  |
                     | code   | | graph  | | signed    |
                     | web/fs | | (the   | | replayed  |
                     | chain  | |  core) | |           |
                     +--------+ +--------+ +-----------+
```

Neo runs the conversation and reversible tools, records its active thread and evidence through the selected memory seam, and escalates to MCL when work becomes consequential. Neocortex is the new deterministic substrate; the Go Cortex remains the rollback-compatible path pending the explicit production cutover.

## The Modules

The root Makefile drives its sibling Go modules — each independently `go build` / `go test` able with its own `go.mod`. At the center are **neo** (the agent) and **cortex** (its memory); **MCL** is the rigor pipeline and the **executor** realises the loop that binds them.


```
📁 matrix
├── 📁 agents        agent manifests (neo.json, default.json)
├── 📁 apps          client   the web app
├── 📁 bridge
│   └── _..._
├── 📁 cassandra
│   └── _..._
├── 📁 chronos
│   └── _..._
├── 📁 codegraph
│   └── _..._
├── 📁 construct
│   └── _..._
├── 📁 cortex
│   └── _..._
├── 📁 deploy        railway (per-user daemon), router, gateway, chronos
├── 📁 dojo
│   └── _..._
├── 📁 executor
│   └── _..._
├── 📁 gateway
│   └── _..._
├── 📁 MCL
│   └── _..._
├── 📁 neo
│   └── _..._
├── 📁 neocortex    C++23 deterministic evidence engine and cortexd
├── 📁 cortexclient Go client, migration, and resurrection-loop seam
├── 📁 router
│   └── _..._
├── 📁 sandboxd
│   └── _..._
├── 📁 skills        SKILL.mtx + SKILL.md capability manifests
├── 📁 tools         MCP server bridges
└── 📁 vault
    └── _..._
```

| Module | Role |
|--------|------|
| **neo** | **The core agent** — the entry point you talk to. Recursive tool-calling loop, paged Cortex memory, conversational recall, swarms, Automatrix, writeback consolidation, and a full-duplex voice mode (MiMo-native hearing + streaming synthesis over LiveKit). Escalates to MCL for consequential operations. |
| **neocortex** | **The deterministic evidence core.** C++23 `cortexd`, one typed append-only log per actor, BLAKE3 MMR checkpoints, sealed records, replay-built LMDB projections, exact deterministic-first recall, intent/work continuity, and a capability-scoped local protocol. |
| **cortexclient** | Go client for `cortexd`: protocol framing, activation/recording/evidence/checkpoint interfaces, bounded reconnect semantics, and legacy Cortex export/import tooling. |
| **cortex** | Compatibility and rollback memory substrate on Pebble. It remains operational until the separately approved Neocortex cutover and supplies the legacy export source. |
| **MCL** | The Matrix Compiler cohort. Three rigorous closed-verb agents that plan and act on high-risk, sensitive tasks with machine exactness. |
| **executor** | The Loop Manager. Per-agent loop engine, lifecycle state machine, MCP dispatch, per-user daemon, Liaison narrator, end-to-end test harness. |
| **bridge** | MCL-to-cortex adapter. Separate Go module for clean interface boundaries. |
| **gateway** | Metered LLM proxy with PAX credit ledger, free-tier whitelist, and rate card enforcement. |
| **router** | The only public listener. Supabase JWT auth, per-user provisioning and wake on Railway, reverse proxy, machine env injection. |
| **vault** | Envelope encryption for all user data at rest: platform KEK → per-user key → per-object DEKs, AES-256-GCM, fail-closed. |
| **cassandra** | Verdict adjudicator. Runs as a silent-voice controller in-process in Neo, and as the executor's completeness critic. |
| **construct** | Typed screen surfaces the agent renders onto the client, with an Ask back-channel. |
| **codegraph** | Agent-native code graph — the structural half of the agent's self-model. |
| **chronos** | Centralised agent scheduler and wake-up system. |
| **sandboxd** | Railway sandbox and branded preview plane; the substrate the dojo disposable desktop boots on. |
| **dojo** | Disposable desktop: a pinned bytebot-desktop image the agent drives by sight, plus the AGON benchmark corpus. |
| **skills** | SKILL.mtx capability manifests and SKILL.md prose capability descriptions. |
| **tools** | MCP server bridges: Paxeer RPC and wallet operations, browser, web-search, machine-mail, exec, media, chronos, desktop, finance, kindle, layerx, deus, sandbox; plus the per-user LiveKit voice worker. |
| **agents** | DID-bound agent manifests (default.json, neo.json) plus MCP server templates. |
| **apps/client** | The Next.js web app: chat over SSE, workspace trace, workbench, settings, wallet. |
| **deploy** | Per-user daemon image (`railway/`) plus the router, gateway and chronos control-plane images. |

LayerX (settlement and custody) and Deus (agent-service registry) are live
services that ship on their own infrastructure; their source lives outside this
repo. Neo reaches both through the `layerx` and `deus` MCP bridges in `tools/`.

## Key Design Decisions

- **Closed-verb coordination (D7)**: The MCL agents coordinate over 10 closed verbs — `find`, `acquire`, `build`, `modify`, `deliver`, `analyze`, `negotiate`, `schedule`, `monitor`, `delegate` — so intent between agents is exact, never inferred at runtime from prose.

- **8 closed object kinds**: `service`, `model`, `agent`, `knowledge`, `intent`, `asset`, `plan`, `capability`. Every operand is one of these. No unstructured blobs cross the line into consequential execution.

- **Replay invariant (section 13.4)**: Derived state can always be rebuilt byte-identically from the journal. Enforced on every pull request via `make ci`. Nothing an agent did is unaccounted for, and nothing it didn't do is hidden.

- **Immutable memory**: Cortex is append-only and content-addressed, so an agent's continuity cannot be silently rewritten — durable for the agent, trustworthy for the user.

- **Signed receipts**: Every consequential run terminates in an EIP-712 receipt — inputs, outputs, cost, hash — that anyone can verify after the fact.

## Deployment shape

In production Matrix runs **one agent per user**, each in its own isolated machine.

- **One image, supervised processes.** The per-user container (`deploy/railway`) boots the MCL daemon and `neo serve`; when Neocortex is selected it also starts and supervises `cortexd` before Neo. Neo owns `POST /chat` and the SSE event stream; every other route is reverse-proxied to the daemon. A required-process exit tears down the group so the platform can restart it cleanly.
- **Neo owns coding.** The former private AgentCore Build worker is packaged only as dormant compatibility utility and is disabled by default. It is not wired into Neo's model-facing tool inventory; Neo uses its native filesystem, bounded shell, durable service, read-only Git, task-list, and coding-checkpoint paths for project work.
- **A real dev environment, baked in.** The image ships the toolchain Neo actually uses through its exec tool: git, Node 22 + pnpm, Go, Python 3.12 + uv, Rust, and Foundry; local PostgreSQL / Redis / SQLite as native binaries; the quality toolchain Neo verifies with (golangci-lint, ruff, eslint, prettier, tsc, vitest, clippy); and a per-user headless Chromium (Playwright) for browsing and screenshot filmstrips.
- **The router is the front door.** `matrix/router` authenticates each request (JWT / Supabase bearer), looks up the caller's machine, wakes it if asleep, and reverse-proxies over the private network. It also mints LiveKit join tokens for voice sessions.
- **Cost-neutral while idle.** On wake-on-request platforms the machine sleeps when network-quiet; the periodic snapshot ticker is disabled so nothing keeps it awake, and durable state (the Cortex volume) is snapshotted to object storage on boot and shutdown. An agent wakes on the next request with its full memory intact — it never starts empty.

## Quickstart

### Prerequisites

- Go 1.25+ (the `neo` module pins 1.25; the rest build on 1.21+)
- GNU Make 4.x
- Node.js 20+
- Python 3.11+
- Docker with Buildx

### Build

```bash
# Clone the repository
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core

# Build all Go modules
make build

# Install runnable CLIs into ./bin
make install

# Run tests (go test -count=1 -race ./... per module)
make test

# Full CI check (gofmt + vet + tests; mirrors GitHub Actions)
make ci
```

### Configure

```bash
# Copy the example environment file
cp .env.example .env

# LLM access — either a direct provider key or the metered gateway:
#   MATRIX_GATEWAY_URL + MATRIX_GATEWAY_TOKEN   (metered, PAX credit ledger)
#   or a provider key (e.g. FIREWORKS_API_KEY)  (direct)
#
# Required for authenticated daemon mode:
#   MATRIX_DAEMON_TOKEN
```

### Talk to Neo (CLI)

```bash
# One-shot turn (Neo's recursive tool-calling loop with cortex memory)
./bin/neo -prompt "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/neo.json \
  -cortex-root ./runs/dev-cortex

# …or an interactive REPL: ./bin/neo
```

### Run the full stack (Neo front + MCL daemon)

```bash
# The MCL plumbing daemon (core_execute, memory/profile stores) on :8081
./bin/mcl-execute daemon -addr :8081 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills

# Neo as the conversational front on :8080, proxying the rest to the daemon
./bin/neo serve -addr :8080 -backend http://127.0.0.1:8081 \
  -manifest agents/neo.json -cortex-root ./runs/dev-cortex -actor neo
```

## API Reference

Neo owns `/chat` and `/events`; every other route is served by the co-located
MCL daemon and reachable through the same front.

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/chat` | Send a user message — the reply streams back over the SSE event stream (the only way to talk to Neo) |
| `GET` | `/events` | Server-Sent Events stream: live tokens, tool events, and transcript tailing |
| `GET` | `/healthz` | Liveness probe + SSE broker statistics |
| `POST` | `/messages` | Submit a consequential message directly to the MCL cohort |
| `GET` | `/intents/{id}` | Read intent envelope chain by intent ID |
| `GET` | `/me` | Per-user settings and identity |
| `POST` | `/shutdown` | Graceful drain and shutdown |

## Documentation

| Resource | Description |
|----------|-------------|
| [Architecture Guide](ARCHITECTURE.md) | System map, module boundaries, key invariants, and design rationale |
| [Contributing Guide](CONTRIBUTING.md) | Development setup, test discipline, commit style, and PR process |
| [Security Policy](SECURITY.md) | Vulnerability disclosure and responsible reporting |
| [Changelog](CHANGELOG.md) | Keep-a-Changelog format release notes |
| [MCL Documentation](docs/MCL-docs/index.md) | MCL language reference, closed-verb grammar, and agent internals |
| [Daemon Image](deploy/railway/Dockerfile) | The per-user daemon container: baked binaries, tool bridges, and dev toolchain |
| [Full Documentation](https://docs.matrixmcl.com) | Complete documentation site at docs.matrixmcl.com |

## Contributing

Matrix Core is open source and you are free to **fork and modify it**. The `main` branch, however, is developed strictly by the core team: unsolicited pull requests are generally not merged, and outside changes are accepted only after we have worked directly with the contributor.

Before opening anything, read the contribution policy at the top of the [Contributing Guide](CONTRIBUTING.md). Issues, bug reports, and security disclosures are always welcome.

Contributors:
- dev-paxeer
- Andrew
- paxlabs-inc
- cursoragent
- Sidiora-Technologies

## License

Matrix Core is source-available under the [Matrix-Protocol License](LICENSE.md).

You may read, use, deploy, and integrate Matrix Core freely. If you modify and redistribute the software, you must release your changes under the same license. A commercial license from PaxLabs Inc. is required once you cross the commercial trigger thresholds:

- Charged fees exceeding **USD 100,000** in any 12-month period; or
- Liquidity under control exceeding **USD 10,000,000**.

See [LICENSE.md](LICENSE.md) for full terms.

## International READMEs

- [Espanol](README.es.md)
- [Nihongo / Japanese](README.ja.md)
- [Portugues](README.pt-BR.md)
- [Russkiy / Russian](README.ru.md)
- [Zhongwen / Chinese (Simplified)](README.zh-CN.md)

## Related

- [Paxeer Network](https://paxeer.app) — The L1 blockchain Matrix Core is built on. 400ms blocks, 400ms finality, purpose-built for agentic workloads.
- [PaxLabs](https://labs.paxeer.app) — Building the future of human-agent collaboration.

---

<p align="center">
  Built by <a href="https://labs.paxeer.app"><strong>PaxLabs Inc.</strong></a>
</p>

<p align="center">
  <sub>SPDX-License-Identifier: Matrix-Protocol</sub>
</p>
