# Deus

**The marketplace and registry for the agent economy** — developers list APIs
and AI services; anyone, especially AI agents, discovers and calls them, paying
only for what they use. Developers keep **100%**; Paxeer hosts services for free
and monetizes the on-chain activity, never the developer.

> In the agent era, software calls software. An agent shouldn't need an account,
> approval, an API key, and a subscription to use a service for ten seconds. On
> Deus it discovers what it needs, pays for the call, and moves on.

Deus is also **Paxeer's registry of agent services**: the agent registry and the
API marketplace are the same product.

## What makes it different

- **Take-nothing economics** — 0% platform fee, free hosting; Paxeer earns from
  activity, not a tax.
- **Native to Paxeer** — instant settlement on Paxeer's own rails, no external
  payment network.
- **One product, not two** — registry == marketplace.
- **Trusted & confidential services are first-class** — TEE-backed, attested,
  signed outputs; opens regulated/enterprise use.

## Architecture in one line

A **Go control plane** (registry, discovery, invoke gateway, metering,
settlement, indexer) + a **Node execution layer** (hosted services on **Paxeer
Cloud**, the Appwrite fork, + proxy egress) + an **on-chain layer**
(`ServiceRegistry` contract + Paxeer agent
precompiles: PoFQ `0x0904`, PaymentStreams `0x0906`, EIP-712 `0x0908`,
TEEAttestor `0x0907`, Scheduler `0x0905`) + **Postgres/pgvector** + a **Next.js
console**, with a **`deus.mjs` MCP proxy** that makes every service a callable
Matrix agent tool. Payments settle through the caller's Paxeer embedded agent
wallet under its spend policy; the platform takes zero.

## Specification

The full, implementable spec lives in [`docs/`](./docs/00-index.md):

| # | Doc | |
| - | --- | - |
| 00 | [Index, glossary, decisions](./docs/00-index.md) | start here |
| 01 | [Product overview](./docs/01-overview.md) | vision, personas, scope |
| 02 | [Architecture](./docs/02-architecture.md) | components, topology, flows |
| 03 | [Data model](./docs/03-data-model.md) | Postgres, pgvector, on-chain, manifest |
| 04 | [On-chain](./docs/04-onchain.md) | `ServiceRegistry` + precompiles |
| 05 | [API](./docs/05-api.md) | REST surface, schemas, errors |
| 06 | [Execution & hosting](./docs/06-execution-hosting.md) | Fly, proxy vs hosted, gateway |
| 07 | [Discovery](./docs/07-discovery.md) | plain-language semantic search |
| 08 | [Payments & billing](./docs/08-payments-billing.md) | rails, economics |
| 09 | [Security](./docs/09-security.md) | threat model, isolation, TEE |
| 10 | [Integration](./docs/10-integration.md) | Matrix MCP, wallet, fee lane |
| 11 | [Modules](./docs/11-modules.md) | Go/Node/contracts/console layout |
| 12 | [File-by-file](./docs/12-file-by-file.md) | every file + syntax direction |
| 13 | [Deployment](./docs/13-deployment.md) | Fly apps, env, ops |
| 14 | [Roadmap](./docs/14-roadmap.md) | phases, MVP |

## Status

**Phase 1 (registry)** — in progress. `ServiceRegistry.sol` + registry HTTP handlers,
indexer mirror, and lexical discovery are implemented. See
[`docs/14-roadmap.md`](./docs/14-roadmap.md) for the full roadmap.

### Quick start (Phase 0)

```bash
export DEUS_POSTGRES_URI='postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable'
export DEUS_DEV=1   # relaxes optional integrations for local skeleton boot

make deus-build deus-test deus-lint
make deus-migrate
go run ./cmd/deusctl manifest validate test/fixtures/proxy-weather.json
go run ./cmd/deusd
```

> Postgres must have `pgcrypto` and `pgvector` extensions installed (superuser
> once): `CREATE EXTENSION IF NOT EXISTS pgcrypto; CREATE EXTENSION IF NOT EXISTS vector;`

## Repo layout (target)

```
deus/
  cmd/        deusd, deus-indexer, deus-settler, deusctl
  internal/   config, server, registry, discovery, gateway, metering,
              pricing, settlement, quality, indexer, hosting, chain,
              wallet, store, objstore, auth, receipts, telemetry
  pkg/        manifest, types, deusclient, pricingmath
  api/        openapi.yaml
  configs/    deus/chain/limits/ranking
  migrations/ forward-only SQL
  contracts/  Foundry: ServiceRegistry.sol, SettlementAnchor.sol
  runner/     Node 20 execution harness + runtimes
  console/    Next.js developer + spend UI
  test/       e2e + fixtures
  docs/       this specification
```
(Plus `tools/deus/` MCP proxy and `deploy/deus/` Fly apps in the monorepo.)
