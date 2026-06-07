# 11 — Modules & Package Breakdown

Deus is one Go module (`github.com/paxlabs-inc/deus`) plus a Node execution
layer, a Foundry contracts project, a Next.js console, and the `tools/deus` MCP
proxy. This doc defines responsibilities; [`12-file-by-file.md`](./12-file-by-file.md)
defines every file.

## 11.1 Go module layout (standard project layout)

```text
deus/
  go.mod                      module github.com/paxlabs-inc/deus  (go 1.22)
  cmd/
    deusd/                    control-plane server binary
    deus-indexer/             (optional standalone) chain->db indexer
    deus-settler/             (optional standalone) settlement worker
    deusctl/                  operator CLI (migrate, replay, settle, deploy-svc)
  internal/                   private packages (not importable outside deus)
    config/                   env + file config loader, typed Config
    server/                   HTTP server, routing, middleware, route groups
    registry/                 listing CRUD, manifest validation, on-chain register
    discovery/                search: embed, vector KNN, lexical, rank
    gateway/                  invoke pipeline: auth->quote->policy->meter->route->receipt
    metering/                 ledger writes, reserve/finalize/void; atomic channel decrement
    pricing/                  pure pricing math (units->wei), versioned   [see pkg note]
    channels/                 per-window payment channels + caller-co-signed vouchers
    settlement/               batch selection, merkle, direct/net/stream rails, voucher redemption
    quality/                  PoFQ sampling + rolling-score update + on-chain write
    indexer/                  event tailer, idempotent upserts, cursor
    hosting/                  Paxeer Cloud (Appwrite) orchestrator: deploy, lifecycle, budget
    chain/                    go-ethereum client, contract bindings, precompile calls
    wallet/                   embedded-wallet agent-auth + spend authorization client
    store/                    postgres access (pgx), queries, migrations runner
    objstore/                 S3/MinIO client (artifacts, receipts, bodies, logs)
    auth/                     caller/dev/internal authN + DID verify
    receipts/                 EIP-712 quote+receipt+voucher build/sign/verify, merkle
    telemetry/                logging, metrics, tracing
  pkg/                        public, importable (by tools, tests, sdk)
    manifest/                 manifest types, canonicalization, JSON Schema, hashing
    deusclient/               Go client for the Deus API (used by deusctl, tests)
    pricingmath/              shared pure pricing functions (if exported)
    types/                    shared wire types (Service, Quote, Receipt, ...)
  api/
    openapi.yaml              canonical REST contract
  configs/
    deus.<env>.yaml           server config
    chain.<env>.json          contract addresses + RPC
    limits.<env>.yaml         resource/quota limits
    ranking.yaml              discovery ranking weights
  migrations/
    001_init.sql ...          forward-only SQL migrations
  test/
    e2e/                      end-to-end harness (list->discover->quote->invoke->settle)
    fixtures/                 sample manifests, golden receipts
  contracts/                  Foundry project (see 04-onchain §4.8)
  docs/                       this spec
  README.md
```

> **pricing note:** keep the *pure* pricing functions in `pkg/pricingmath` (or
> `pkg/manifest`) so the quote path, gateway charge, and settlement all import
> the identical code — this guarantees quote == charge == settled.

## 11.2 Package responsibilities

| Package | Owns | Depends on | Key invariant |
| ------- | ---- | ---------- | ------------- |
| `internal/config` | typed config from env + files | — | fail fast on missing required env |
| `internal/server` | HTTP wiring, middleware, route groups | all internal services | stateless; graceful shutdown |
| `internal/registry` | listing lifecycle + validation + register tx | chain, store, manifest | manifest_hash matches chain |
| `internal/discovery` | search + rank | store(pgvector), embed | degrade to filters on embed failure |
| `internal/gateway` | invoke pipeline | metering, pricing, wallet, chain, hosting, receipts | no charge without delivery |
| `internal/metering` | append-only ledger + state machine + **atomic channel reserve** | store | idempotent; never edits history; reserve = transactional channel decrement |
| `internal/pricing` | unit→wei (wraps pkg/pricingmath) | — | pure, versioned, deterministic |
| `internal/channels` | per-window payment channels + **caller-co-signed vouchers** | store, chain, receipts, wallet | fund per window not per reserve; voucher monotonic |
| `internal/settlement` | batching + rails (direct/net/stream) + anchor | store, chain, channels, receipts | sum(finalized) == paid; redeem highest voucher; never double-pay |
| `internal/quality` | sample + PoFQ rolling update | chain, store | scores reproducible; samples bilateral once caller co-signs |
| `internal/indexer` | chain→db mirror | chain, store | idempotent + replay-safe |
| `internal/hosting` | Paxeer Cloud (Appwrite) deploy/lifecycle | objstore, appwrite api | native scale-to-zero; free-hosting budget-aware |
| `internal/chain` | RPC + bindings + precompile calls | go-ethereum | reuse on-chain abis |
| `internal/wallet` | agent-auth + spend authz | embedded wallet api | Deus holds no caller keys |
| `internal/store` | pgx pool, queries, migrations | postgres | big-int as text |
| `internal/objstore` | blob IO by hash | S3/MinIO | content-addressed |
| `internal/auth` | authN, DID verify, roles | wallet | least privilege |
| `internal/receipts` | EIP-712 (quote/receipt/**voucher**) + merkle | chain(0x0908) | digest == on-chain digest |
| `internal/telemetry` | logs/metrics/traces | — | redact secrets |
| `pkg/manifest` | manifest schema + canonical hash | — | canonicalization is versioned + stable |
| `pkg/types` | wire types | — | JSON tags stable across versions |
| `pkg/deusclient` | Go API client | pkg/types | mirrors openapi.yaml |

## 11.3 Node execution layer

```text
deus/runner/                 (Node 20, ESM, TypeScript)
  src/
    harness.ts               wraps developer handle(); enforces timeout/size/caps; co-signs receipt
    server.ts                private HTTP server the gateway calls
    units.ts                 unit reporting helpers (per_unit)
    egress.ts                egress allowlist enforcement
    secrets.ts               sealed per-service secret access
    sign.ts                  runner receipt co-signature (reuse precompiles.ts EIP712)
  runtimes/
    node20/                  function runtime adapter
    container/               container entry shim
  package.json               "type":"module"
  tsconfig.json
```
Runtime adapters/templates are packaged as **Paxeer Cloud (Appwrite) Functions**
(node20 runtime) or container **Sites**; templates live under
`deploy/deus/runner/`. Appwrite owns build/scale-to-zero/secrets — there is no
bespoke machine image to manage.

## 11.4 Web console

```text
deus/console/                (Next.js 16 / React 19 / Tailwind / shadcn — mirror client/)
  app/                       routes: /, /discover, /develop, /develop/[id], /spend, /login
  components/                listing form, manifest editor, analytics, search, spend dash
  lib/                       deus api client (TS), supabase auth, wallet link, format
  hooks/                     react-query hooks per API group
  messages/                  i18n (en base; deep-merge fallback like client/)
```
House style: minimal, dark, single Paxeer-blue accent, Inter + JetBrains Mono,
**no emojis/gradients/glow, depth via surface tone not borders.**

## 11.5 MCP proxy + deploy (live elsewhere in the monorepo)

- `tools/deus/deus.mjs` + `tools/deus/deus-tools.json` — MCP proxy (see
  [`10-integration.md`](./10-integration.md)).
- `deploy/deus/` — Fly apps + Dockerfiles + install/deploy scripts (see
  [`13-deployment.md`](./13-deployment.md)).

## 11.6 Build & verify targets

Add to the repo `Makefile` (or a `deus/Makefile`):
- `deus-build` → `go build ./...` in `deus/`.
- `deus-test` → `go test ./...`.
- `deus-lint` → `golangci-lint run` (reuse root config).
- `deus-contracts` → `forge build && forge test` in `deus/contracts`.
- `deus-console` → `pnpm -C deus/console build`.
- `deus-mcp-selftest` → `node tools/deus/deus.mjs --selftest`.
- `deus-migrate` → `deusctl migrate`.
