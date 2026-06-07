# Deus — Master Specification

> **Deus** is the marketplace and registry for the agent economy: developers
> list APIs and AI services, and anyone — especially AI agents — discovers and
> calls them, paying only for what they use. Developers keep **100%** of
> earnings; Paxeer hosts services for free and monetizes the on-chain activity,
> never the developer.

This is the canonical, authoritative specification for the Deus product. It is
written to be implementable file-by-file. If any downstream code disagrees with
this spec, **the spec wins until it is amended here**.

- **Repo path:** `/root/matrix/deus`
- **Status:** Specification (pre-implementation). Scaffold exists, no code yet.
- **Owner:** PaxLabs Inc. / Paxeer Network
- **Audience:** Matrix/Paxeer engineers implementing Deus.

---

## How to read this spec

Read in order for a full picture; jump to a section for a specific task.

| #  | Document | What it covers |
| -- | -------- | -------------- |
| 00 | [`00-index.md`](./00-index.md) | This file — index, glossary, decision log, conventions |
| 01 | [`01-overview.md`](./01-overview.md) | Product vision, personas, value props, scope & non-goals |
| 02 | [`02-architecture.md`](./02-architecture.md) | System design, component map, runtime topology, request flows |
| 03 | [`03-data-model.md`](./03-data-model.md) | Postgres schema, pgvector index, on-chain state, entities |
| 04 | [`04-onchain.md`](./04-onchain.md) | `ServiceRegistry` contract + Paxeer precompile integration |
| 05 | [`05-api.md`](./05-api.md) | REST API surface, request/response schemas, error model |
| 06 | [`06-execution-hosting.md`](./06-execution-hosting.md) | Where it runs (Fly), proxy vs hosted listings, sandbox, invocation gateway |
| 07 | [`07-discovery.md`](./07-discovery.md) | Plain-language semantic search, embeddings, ranking |
| 08 | [`08-payments-billing.md`](./08-payments-billing.md) | Per-call micro-payments, streaming, net settlement, economics |
| 09 | [`09-security.md`](./09-security.md) | Threat model, auth, isolation, confidential (TEE) services, abuse |
| 10 | [`10-integration.md`](./10-integration.md) | Matrix MCP proxy, agent registry, embedded-wallet, agent fee lane |
| 11 | [`11-modules.md`](./11-modules.md) | Go module + package responsibility breakdown |
| 12 | [`12-file-by-file.md`](./12-file-by-file.md) | Every file, its content direction, and the syntax to use |
| 13 | [`13-deployment.md`](./13-deployment.md) | `deploy/deus` Fly apps, env vars, install & ops |
| 14 | [`14-roadmap.md`](./14-roadmap.md) | Phased milestones, MVP scope, exit criteria |

---

## One-paragraph architecture

Deus has a **Go control plane** (`github.com/paxlabs-inc/deus`) that owns the
registry, discovery, metering, settlement, and the public/agent APIs; a
**Node execution layer** that hosts uploaded services and proxies external
ones on Fly Machines; an **on-chain layer** — a `ServiceRegistry` Solidity
contract plus the Paxeer agent precompiles (PoFQ `0x0904` for quality,
PaymentStreams `0x0906` for streaming pay, EIP-712 `0x0908` for signed
receipts, TEEAttestor `0x0907` for confidential services, Scheduler `0x0905`
for recurring calls); a **Postgres + pgvector** store for the searchable index
and metering ledger; and a **Next.js console** for humans. AI agents reach Deus
either over its HTTP agent API or through a **`deus.mjs` MCP proxy** baked into
the Matrix per-user daemon, so every Deus service is also a callable agent tool.
Payments settle through the caller's **Paxeer embedded agent wallet** under its
existing spend policy; the platform takes **zero** fee.

---

## Glossary

| Term | Meaning |
| ---- | ------- |
| **Service** | A callable unit listed on Deus. Either a **data API** (returns information) or an **agent service** (performs work). |
| **Listing** | A service's registry entry: manifest, pricing, endpoints, owner, quality score. |
| **Proxy listing** | Deus forwards calls to the developer's own HTTPS endpoint; Deus meters + settles. |
| **Hosted listing** | Developer uploads code/container; Deus builds and runs it on Fly (free hosting). |
| **Manifest** | The machine-readable description of a service: schema, pricing, capabilities, auth, attestation method. |
| **Invocation** | A single billable call to a service. |
| **Call receipt** | An EIP-712-signed record of an invocation (caller, service, price, result hash, outcome). |
| **PoFQ** | Proof-of-Fill-Quality — Paxeer precompile `0x0904`; here repurposed as the **service quality score**. |
| **Quality score** | A `0..1e18` reputation value per service derived from delivery outcomes, fed by PoFQ. |
| **Caller** | The entity invoking a service — a human (via console) or, primarily, an AI agent. |
| **Agent wallet** | The caller's Paxeer embedded wallet with on-chain spend policy (see `protocol/paxeer-embeded-wallets`). |
| **Net settlement** | Batched on-chain settlement of many small invocations (lazy, ~10s windows). |
| **Stream** | A PaymentStreams (`0x0906`) rate-based payment for long/continuous service use. |
| **Confidential service** | A service running in a TEE whose execution is provable via TEEAttestor (`0x0907`). |
| **Gateway** | The Deus component that authenticates, meters, rate-limits, and routes invocations. |
| **Indexer** | The component mirroring on-chain `ServiceRegistry` events into Postgres. |
| **Console** | The Next.js web UI for developers (list/manage) and humans (browse/spend). |
| **MCP proxy** | `deus.mjs` — exposes Deus services to Matrix agents as MCP tools. |

---

## Decision log (locked)

These are the load-bearing decisions every doc below assumes. Each has a short
rationale. Change them **here first**, then propagate.

| ID | Decision | Rationale |
| -- | -------- | --------- |
| **D-1** | Control plane in **Go** (`github.com/paxlabs-inc/deus`). | Matches matrix-core (`executor/`, `router/`, `gateway/` all Go); shares conventions, build, and deploy tooling. Scaffold already a Go layout. |
| **D-2** | Per-service runners + service adapters in **Node 20**. | Most listed APIs/services are JS/HTTP; Node sandbox + the existing `*.mjs` MCP-proxy idiom (`tools/browser`, `tools/tachyon`). |
| **D-3** | Web console in **Next.js 16 / React 19 / Tailwind / shadcn**. | Mirrors `client/`; reuses design tokens, auth (Supabase), and component lib. |
| **D-4** | The registry is **on-chain first** via a `ServiceRegistry` contract; Postgres is a **read-optimized mirror**. | The whitepaper L3 registry + agent fee lane `Registry` field imply on-chain truth; off-chain index for search speed. |
| **D-5** | One product: the **agent registry == the API marketplace**. | Per the brief: "one product, not two." A single `services` entity serves both. |
| **D-6** | Payments via the caller's **Paxeer embedded agent wallet**; **platform fee = 0**. | "Take-nothing economics." Paxeer earns from gas/activity, not a cut. |
| **D-7** | Three payment rails: **per-call net settlement** (default), **PaymentStreams `0x0906`** (continuous), **direct transfer** (high-value one-shot). | Match call shape to cost: micro-calls batch, long sessions stream. |
| **D-8** | Quality via **PoFQ `0x0904`**, not a comment section. | Objective, on-chain, portable reputation. |
| **D-9** | Confidential/trusted services via **TEEAttestor `0x0907`** + signed outputs. | Enables regulated/enterprise + verifiable compute. |
| **D-10** | Hosting on **Fly Machines**, scale-to-zero, shared private apps (6PN `.internal`). | Reuses the proven `deploy/browser` + `deploy/tachyon` pattern. |
| **D-11** | Discovery = **plain-language semantic search** (pgvector) + structured filters. | "Describe what you need and get the right service back." |
| **D-12** | Matrix integration via a **`deus.mjs` stdio MCP proxy** baked into the daemon image. | Same idiom as `tools/tachyon/tachyon.mjs`; makes every service an agent tool. |
| **D-13** | Spend safety delegated to the **embedded-wallet policy plane + Argus VM**, never re-implemented in Deus. | Single source of truth for agent spend limits/rules. |
| **D-14** | Call receipts are **EIP-712 `0x0908`-signed**; result hashes anchored. | Disputes, evidence, and cross-platform trust. |

---

## Cross-cutting conventions (apply to every file)

- **Go:** `gofumpt`, `golangci-lint` (reuse `/root/matrix/.golangci.yml`), package
  comments, table-driven tests, no naked `panic` in request paths. Errors wrapped
  with `%w`. Context-first function signatures.
- **TypeScript/Node:** ESM (`.mjs`/`.ts` with `"type":"module"`), `tsc --noEmit`
  clean, Prettier + ESLint `--max-warnings 0` (matches `client/` pre-commit hook).
- **Solidity:** `solc 0.8.27`, Foundry layout, `evm_version = "shanghai"`
  (Paxeer chain 125 is pre-Cancun — see the open MCOPY bug note), NatSpec on every
  external function.
- **Chain facts:** EVM chain id `125`, RPC via `PAXEER_RPC_URL`, native asset PAX
  (`ahpx`, 1e18). Precompile addresses are fixed (see [`04-onchain.md`](./04-onchain.md)).
- **Secrets:** never hardcode keys; read from env / `/etc/matrix/*.env`. Never
  store secret VALUES in docs — only where they live.
- **Comments:** do not add/remove comments in unrelated code when implementing.
- **No emojis, no purple gradients, no glow** in any UI (house style).
- **UI depth via surface tone, not border strokes** (house style).

See [`12-file-by-file.md`](./12-file-by-file.md) for per-file syntax direction.
