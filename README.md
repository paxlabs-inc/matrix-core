<p align="center">
  <img src="https://cdn.redixusercontent.ocfstudio.com/matrix.png" alt="Matrix" />
</p>

<p align="center">
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Project-Matrix-0A0A0A?style=flat-square&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIxNiIgaGVpZ2h0PSIxNiIgdmlld0JveD0iMCAwIDI0IDI0IiBmaWxsPSJub25lIiBzdHJva2U9IiNmZmZmZmYiIHN0cm9rZS13aWR0aD0iMiIgc3Ryb2tlLWxpbmVjYXA9InJvdW5kIiBzdHJva2UtbGluZWpvaW49InJvdW5kIj48Y2lyY2xlIGN4PSIxMiIgY3k9IjEyIiByPSIxMCIvPjxwYXRoIGQ9Ik0xMiAxNnYtNCIvPjxwYXRoIGQ9Ik0xMiA4aC4wMSIvPjwvc3ZnPg==&logoColor=white" alt="Project: Matrix" /></a>
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Built%20by-PaxLabs-0A0A0A?style=flat-square&logoColor=white" alt="Built by PaxLabs" /></a>
  <a href="LICENSE.md"><img src="https://img.shields.io/badge/License-Matrix--Protocol-0A0A0A?style=flat-square" alt="License: Matrix-Protocol" /></a>
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

Matrix is a cognition layer built for the Machine Economy Vision of the Paxeer Network it extends a language model beyond conversation into real execution: agent-to-agent on-chain financial coordination, high-risk task execution, and the safe handling of critical, confidential work.

The reason most agent stacks break on that class of work is that they carry natural language all the way down. Human language is a leaky channel the biological way we reason, perceive, and approximate bleeds into every sentence we produce. That's fine for chat. It is not fine when an agent is moving funds, performing an irreversible write, or holding something confidential. Matrix gives humans and machines a way to coordinate on exactly that work *without* the ambiguity of human reasoning and human language reaching the parts that must not be ambiguous.

It does this with three layers.

## The Three Layers

### 1 — The Matrix Compiler (MCL)

**The top boss of the stack.** MCL is a cohort of three rigorous agents that communicate, plan, and act among themselves over a closed-verb protocol free of the constraints and ambiguity of human language and human input. They coordinate with the exactness of a machine and apply it to real-world, high-risk, sensitive tasks: money, irreversible operations, confidential handling.

When a task crosses the line into the consequential, it goes here. The three agents calculate the outcome space, confirm they hold every input the work requires, ask for clarity when they don't, and execute once, to spec. Nothing runs on a guess.

### 2 — The Cortex

**A full memory, context, and immutable state engine.** Cortex gives every agent persistent, durable memory: a per-actor timeline of events, active attention, and typed state, append-only and byte-deterministically replayable. Continuity stops being an illusion the model has to fake each session it is real for the user and unbreakable for the agent. An agent that runs on Matrix does not wake up empty.

### 3 — The Loop Manager

**A per-agent loop engine.** For each agent, the Loop Manager coordinates the constant inflow and exchange between the user, the LLM, and Cortex and escalates to the MCL pipeline the moment work becomes consequential. It is the runtime that keeps an agent coherent across turns, tools, and time, and knows exactly when to hand a decision up rather than improvise past it.

## How It Fits Together

```
                        +-----------------------------+
                        |            User             |
                        +--------------+--------------+
                                       |
                                       v
                    +------------------+------------------+
                    |           Loop Manager              |
                    |     per-agent coordination loop     |
                    |     user  <->  LLM  <->  Cortex     |
                    +----+---------------+-----------+----+
                         |               |           |
                 reversible work         |       escalation
                         |               |           |
                         v               v           v
                    +---------+    +-----------+  +------------------+
                    |   LLM   |    |  Cortex   |  |  Matrix Compiler |
                    | (chat,  |    |  memory   |  |  (MCL)           |
                    |  tools) |    |  context  |  |  3 rigorous      |
                    +---------+    |  immutable|  |  closed-verb     |
                                   +-----------+  |  agents          |
                                                  +------------------+
                                                    money / on-chain /
                                                    irreversible /
                                                    confidential
```

The default conversational agent (**Neo**) runs inside the Loop Manager with shell, code, fetch, and web tools available on reversible work. The instant stakes rise, the Loop Manager escalates to MCL, and control returns to Neo once rigor is no longer required.

## The Modules

The root Makefile drives nine sibling Go modules — each independently `go build` / `go test` able with its own `go.mod`. The three layers above map onto them: **MCL** is the compiler cohort, **cortex** is the memory engine, and the **executor** realises the Loop Manager.


  <img src="https://www.readmecodegen.com/api/file-tree-embed?repo=paxlabs-inc%2Fmatrix-core&branch=main&maxDepth=1&foldersOnly=true&transparentBg=true&showHeader=true" alt="Dynamic File Tree" />


| Module | Role |
|--------|------|
| **MCL** | The Matrix Compiler cohort. Three rigorous closed-verb agents that plan and act on high-risk, sensitive tasks with machine exactness. |
| **cortex** | Per-actor typed memory engine on Pebble. Append-only journal, Merkle-anchored snapshots, byte-deterministic replay. Persistent, immutable, durable. |
| **bridge** | MCL-to-cortex adapter. Separate Go module for clean interface boundaries. |
| **executor** | The Loop Manager. Per-agent loop engine, lifecycle state machine, MCP dispatch, per-user daemon, Liaison narrator, end-to-end test harness. |
| **neo** | Default conversational agent that runs inside the loop, with automatic escalation to MCL for consequential operations. |
| **gateway** | Metered LLM proxy with PAX credit ledger, free-tier whitelist, and rate card enforcement. |
| **router** | Per-user Fly Machine provisioning with wake-then-reverse-proxy front door. |
| **deus** | Agent-service marketplace: registry, discovery, metered invocation, EIP-712 receipts, hosted execution. |
| **tachyon** | Agent-native Solidity/EVM engine — compile, test, simulate, deploy. (git submodule) |
| **uwac** | Universal Web Agent Connector — OAuth vault providing per-user MCP tools. |
| **layerx** | Settlement fabric and custody spine for agent balances. |
| **chronos** | Centralised agent scheduler and wake-up system. |
| **atlas** | Additional infrastructure orchestration layer. |
| **context** | Context management subsystem. |
| **journal** | Append-only journal subsystem for deterministic state replay. |
| **knowledge** | Canonical references: matrix.kvx project state, models, and schema definitions. |
| **skills** | SKILL.mtx capability manifests and SKILL.md prose capability descriptions. |
| **tools** | MCP servers: paxeer, browser, tachyon, deus, uwac, web-search, media, cortex. |
| **agents** | DID-bound agent manifests (default.json, neo.json) plus MCP server templates. |
| **protocol** | Protocol definitions and wire formats. |
| **marketplace** | Deus marketplace and developer dashboard (React Router on Cloudflare Workers). |
| **client** | Matrix consumer application (Next.js / React). |
| **deploy** | Daemon container image, Fly Machine deploy, shared-service images, box install scripts. |

## Key Design Decisions

- **Closed-verb coordination (D7)**: The MCL agents coordinate over 10 closed verbs — `find`, `acquire`, `build`, `modify`, `deliver`, `analyze`, `negotiate`, `schedule`, `monitor`, `delegate` — so intent between agents is exact, never inferred at runtime from prose.

- **8 closed object kinds**: `service`, `model`, `agent`, `knowledge`, `intent`, `asset`, `plan`, `capability`. Every operand is one of these. No unstructured blobs cross the line into consequential execution.

- **Replay invariant (section 13.4)**: Derived state can always be rebuilt byte-identically from the journal. Enforced on every pull request via `make ci`. Nothing an agent did is unaccounted for, and nothing it didn't do is hidden.

- **Immutable memory**: Cortex is append-only and content-addressed, so an agent's continuity cannot be silently rewritten — durable for the agent, trustworthy for the user.

- **Signed receipts**: Every consequential run terminates in an EIP-712 receipt — inputs, outputs, cost, hash — that anyone can verify after the fact.

## Quickstart

### Prerequisites

- Go 1.22+
- GNU Make 4.x
- Node.js 20+
- Python 3.11+
- Docker with Buildx

### Build

```bash
# Clone the repository
git clone https://github.com/paxlabs-inc/matrix-core.git
cd matrix-core

# Build all nine Go modules
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

# Required for consequential (non-dry-run) execution:
#   FIREWORKS_API_KEY
#   TOGETHER_API_KEY
#
# Required for authenticated daemon mode:
#   MATRIX_DAEMON_TOKEN
```

### Run an Agent Loop

```bash
./bin/mcl-execute walk \
  -prose "Summarise the README and write it to /tmp/summary.md" \
  -manifest    agents/default.json \
  -cortex-root ./runs/dev-cortex \
  -skills-root ./skills
```

### Start the Daemon

```bash
./bin/mcl-execute daemon \
  -addr        :8080 \
  -cortex-root ./runs/dev-cortex \
  -manifest    agents/default.json \
  -skills-root ./skills
```

## API Reference

The daemon exposes a lightweight HTTP API for agent interaction.

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/healthz` | Liveness probe + SSE broker statistics |
| `POST` | `/chat` | Converse with the agent (conversational loop, via Neo) |
| `GET` | `/events` | Server-Sent Events stream for real-time transcript tailing |
| `POST` | `/messages` | Submit a consequential message (escalates to the MCL cohort) |
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
| [Daemon Deploy Guide](deploy/daemon/README.md) | Production deployment, Fly Machine configuration, and operations |
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