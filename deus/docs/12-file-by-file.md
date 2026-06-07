# 12 — File-by-File Overview

Every file Deus will contain, with **what goes in it**, **the syntax/conventions
to use**, and **its key contract**. Group order follows the build order in
[`14-roadmap.md`](./14-roadmap.md). `[NEW]` = create; `[EDIT]` = modify an
existing repo file.

> Global syntax: Go = `gofumpt` + root `.golangci.yml`, context-first, errors
> `%w`-wrapped, table tests. TS/Node = ESM, strict `tsc`, ESLint/Prettier
> `--max-warnings 0`. Solidity = `0.8.27`, `evm_version="shanghai"`, NatSpec.
> SQL = forward-only idempotent migrations. YAML/JSON config = typed + validated
> at load. Money = string big-int (PAX wei). No emojis/gradients/glow in UI.

---

## 12.1 Module root

### `deus/go.mod` `[EDIT]` (currently empty)
- `module github.com/paxlabs-inc/deus`, `go 1.22`.
- Deps: `pgx/v5`, `go-ethereum`, `pgvector-go`, an S3 SDK (minio-go), a JSON
  Schema lib (`santhosh-tekuri/jsonschema`), `prometheus/client_golang`,
  `spf13/cobra` (CLI), a router (`chi` or stdlib `net/http` mux), `rs/zerolog`.

### `deus/README.md` `[EDIT]` (currently empty)
- Short product line + "see `docs/00-index.md`" + quick-start (build, migrate,
  run, list a service, invoke). Markdown. No emojis.

### `deus/Makefile` `[NEW]`
- Targets from [`11-modules.md`](./11-modules.md) §11.6. Plain Make; mirror the
  root `Makefile` style.

---

## 12.2 `cmd/` (binaries — `package main`, thin)

### `cmd/deusd/main.go` `[NEW]`
- Loads config, opens Postgres + objstore + chain client, runs migrations,
  constructs all `internal/*` services, mounts `internal/server`, starts HTTP,
  handles SIGTERM graceful shutdown.
- Pattern: mirror `paxeer-embeded-wallets/src/index.ts` boot order (DB connect →
  migrate → register routes → listen) and matrix daemon shutdown.
- No business logic here — wiring only.

### `cmd/deus-indexer/main.go` `[NEW]` (optional standalone)
- Runs `internal/indexer` as its own process for ops isolation. Can also run
  in-process inside `deusd` (config flag).

### `cmd/deus-settler/main.go` `[NEW]` (optional standalone)
- Runs `internal/settlement` windows on a ticker; advisory-lock so only one
  settler acts (like the funded evaluator's pg advisory lock).

### `cmd/deusctl/main.go` + `cmd/deusctl/cmd_*.go` `[NEW]`
- Cobra CLI. Subcommands: `migrate`, `index replay --from <block>`,
  `settle run --developer <id>`, `svc deploy <id>`, `svc logs <id>`,
  `manifest validate <file>`, `chain addresses`.
- Each subcommand in its own `cmd_<name>.go`. Output: human table + `--json`.

---

## 12.3 `internal/config`

### `internal/config/config.go` `[NEW]`
- Typed `Config` struct; `Load() (*Config, error)` from env + `configs/*.yaml`.
- Required env (fail fast if missing in prod): `DEUS_POSTGRES_URI`,
  `PAXEER_RPC_URL`, `DEUS_OBJSTORE_*`, `DEUS_GATEWAY_SIGNING_KEY_REF`,
  `DEUS_SETTLER_KEY_REF`, `DEUS_SERVICE_REGISTRY_ADDR`, `MATRIX_WALLET_API_URL`,
  `DEUS_EMBED_*`. Hosted-exec (Phase 3): `DEUS_APPWRITE_ENDPOINT`,
  `DEUS_APPWRITE_PROJECT`, `DEUS_APPWRITE_API_KEY` (secret ref). Optional:
  `DEUS_HOSTING_BUDGET_*` (free-hosting ceiling/kill-switch), worker counts,
  ports. (`DEUS_FLY_*` only if the control plane runs on Fly.)
- Syntax: env via `os.Getenv` + a small `envOr`/`mustEnv` helper (mirror the
  router's `envOr`). Validate ranges. Never log secret values.

---

## 12.4 `internal/server`

### `internal/server/server.go` `[NEW]`
- `New(deps) *Server`; `Mount() http.Handler`. Route groups: `/v1/services`,
  `/v1/discover`, `/v1/quote`, `/v1/invoke`, `/v1/streams`, `/v1/me`,
  `/internal/*`, `/internal/healthz`, `/internal/metrics`.
- Middleware chain: request-id, logging, recover, CORS (console origins),
  rate-limit, auth resolver.

### `internal/server/middleware.go` `[NEW]`
- `RequestID`, `Recover` (never leak internals → uniform error envelope),
  `RateLimit` (token bucket per DID/IP), `AuthResolver` (populates caller/dev
  context). Uniform error encoder matching [`05-api.md`](./05-api.md) §5.2.

### `internal/server/handlers_registry.go` / `_discovery.go` / `_invoke.go` / `_streams.go` / `_me.go` / `_internal.go` `[NEW]`
- One handler file per route group. Handlers are thin: decode → validate →
  call `internal/<service>` → encode. No business logic in handlers.
- Each request/response type has a struct in `pkg/types` with stable JSON tags.

---

## 12.5 `internal/registry`

### `internal/registry/registry.go` `[NEW]`
- `Create`, `Get`, `Update`, `Publish`, `Pause`, `Delist`, `SetPayout`,
  `Analytics`.
- `Publish` = validate manifest → submit `ServiceRegistry.register` via
  `internal/chain` → wait for `ServiceRegistered` → upsert mirror →
  enqueue embedding.

### `internal/registry/validate.go` `[NEW]`
- Validate manifest against JSON Schema (`pkg/manifest`), pricing sanity
  (positive wei, floor ≤ price), URL reachability probe (proxy mode), schema
  validity for operations. Returns structured warnings/errors.

---

## 12.6 `internal/discovery`

### `internal/discovery/discovery.go` `[NEW]`
- `Search(ctx, Request) (Results, error)`: constraint extraction → embed →
  vector KNN + lexical union → rank → shape.

### `internal/discovery/extract.go` `[NEW]`
- Plain-language → filters. Regex for money/percent/latency phrasings;
  optional LLM normalizer behind an interface (off the critical path).

### `internal/discovery/rank.go` `[NEW]`
- Pure blended-score function (weights from `configs/ranking.yaml`). Unit-tested
  with golden cases. Deterministic given inputs.

### `internal/discovery/embed.go` `[NEW]`
- Thin wrapper over the embedder (cortex Fireworks client or gateway route).
  Interface so it's swappable + mockable; failure → caller falls back to lexical.

---

## 12.7 `internal/gateway`

### `internal/gateway/gateway.go` `[NEW]`
- `Invoke(ctx, req) (Result, error)` — the pipeline in
  [`06-execution-hosting.md`](./06-execution-hosting.md) §6.2, orchestrating
  auth → quote validate → policy → meter reserve → route → receipt → finalize.
- Each stage returns a typed error mapped to an API error code.

### `internal/gateway/route.go` `[NEW]`
- Dispatch to proxy egress vs hosted runner vs confidential runner. Applies
  `timeout_ms`, `max_response_bytes`, retry policy. Captures latency/outcome.

### `internal/gateway/quote.go` `[NEW]`
- `Quote(ctx, req)`: compute price via `internal/pricing`, build + sign EIP-712
  quote via `internal/receipts`, persist `quotes` row, return.
- `ValidateQuote`: not expired, signature valid, `pricing_version` matches a
  registered `pricingHash`.

---

## 12.8 `internal/metering`

### `internal/metering/ledger.go` `[NEW]`
- `Reserve`, `Finalize`, `Void` — the state machine; idempotent by
  `idempotency_key`. Append-only; never UPDATE history except the state column.
- `Unsettled(developerID, window)` for the settler.

---

## 12.9 `internal/pricing` + `pkg/pricingmath`

### `pkg/pricingmath/pricing.go` `[NEW]`
- Pure functions: `Charge(plan, units) (wei *big.Int)`, with floor; big.Int
  only, no float. Versioned by `plan.Version`. Exhaustive unit tests.

### `internal/pricing/pricing.go` `[NEW]`
- Loads plans, calls `pkg/pricingmath`, resolves the active version. Thin.

---

## 12.10 `internal/settlement`

### `internal/settlement/settler.go` `[NEW]`
- `RunWindow(ctx, developerID)`: select finalized unsettled rows → sum → build
  merkle (`internal/receipts`) → pay via rail → anchor → mark settled. Idempotent
  + retry-safe; advisory lock.

### `internal/settlement/rails.go` `[NEW]`
- `directSettle` (MVP: inline `agent/send` per call), `netSettle` (redeems the
  highest caller-co-signed voucher from a channel; one transfer/developer/window
  + `SettlementAnchor.anchor`), `streamSettle` (calls `0x0906 settle/close`).
  Each returns a `tx_hash`. Reuses `internal/chain` + `internal/channels`.

---

## 12.10b `internal/channels` (Phase 2.5)

### `internal/channels/channels.go` `[NEW]`
- `Open(callerDID, windowCap)` (funds the per-window escrow, one chain write),
  `Reserve(channelID, maxWei)` (the **atomic decrement**, the load-bearing
  invariant from §6.2 — single transactional `UPDATE` / row lock), `Finalize`,
  `Void`, `Close` (refund remainder).

### `internal/channels/voucher.go` `[NEW]`
- Build the `DeusVoucher` EIP-712 struct, return it for the caller to co-sign,
  verify the caller signature (`recoverTypedSigner` on `0x0908`), persist the
  monotonic `(cumulative_wei, nonce)`. The voucher is the bilateral charge proof
  ([`08-payments-billing.md`](./08-payments-billing.md) §8.3) and carries the
  `outcome` bit that makes the quality sample bilateral (§4.3). Reject
  non-monotonic nonces.

---

## 12.11 `internal/quality`

### `internal/quality/quality.go` `[NEW]`
- `Sample(invocation) Score` (objective delivery score 0..1e18).
- `FoldWindow(serviceID)`: batch samples → `0x0904 updateRollingScore` →
  persist new `(score, weight)` → update `services.quality_score`. Reproducible.

---

## 12.12 `internal/indexer`

### `internal/indexer/indexer.go` `[NEW]`
- Tail `ServiceRegistry` (+ `SettlementAnchor`) logs from `index_cursor`;
  idempotent upserts; advance cursor atomically with the upsert. Replay-safe:
  re-running from any block yields the same mirror.

### `internal/indexer/decode.go` `[NEW]`
- ABI event decoders (reuse generated bindings from `internal/chain`).

---

## 12.13 `internal/hosting`

### `internal/hosting/orchestrator.go` `[NEW]`
- `Deploy(serviceID, artifact)`, `Redeploy`, `Logs`, `Delete`. Talks the
  **Paxeer Cloud (Appwrite) Server API**: create/update a Function (node20
  source) or container Site, set function variables (secrets), set resource
  caps, read the execution endpoint/domain into `deployments.exec_endpoint`.
  No machine lifecycle loop — Appwrite owns scale-to-zero. **Budget-aware**:
  enforce the free-hosting aggregate budget + kill-switch ([`06-execution-hosting.md`](./06-execution-hosting.md) §6.7);
  refuse new always-warm/dedicated allocations past the ceiling.

### `internal/hosting/appwrite.go` `[NEW]`
- Thin Appwrite Server API client (project + API key from config): functions,
  deployments, variables, executions. Keep it isolated so the hosting backend is
  swappable.

---

## 12.14 `internal/chain`

### `internal/chain/client.go` `[NEW]`
- go-ethereum `ethclient` over `PAXEER_RPC_URL`; chain id 125; nonce mgmt;
  gas (agent fee lane price when relaying).

### `internal/chain/bindings/` `[NEW]`
- `abigen`-generated bindings for `ServiceRegistry` + `SettlementAnchor` from
  `contracts/out/*.abi`. Regenerate via `deusctl`/Make target.

### `internal/chain/precompiles.go` `[NEW]`
- Go callers for `0x0904/0x0906/0x0907/0x0908/0x0905`, using the **same
  `abi.json`** as `knowledge/HyperPax-OS/precompiles/*` and
  `precompiles.ts`. Mirror the TS builders' semantics exactly.

---

## 12.15 `internal/wallet`

### `internal/wallet/client.go` `[NEW]`
- Client for `protocol/paxeer-embeded-wallets`: agent-auth (ed25519 challenge/
  verify), `AuthorizeSpend`, `OpenStream`, `Send`, `ReadPolicy`. Deus holds **no**
  caller keys; this client forwards bearer tokens. Reuse `tools/paxeer/lib/
  agentauth.mjs` semantics on the Go side.

---

## 12.16 `internal/store`

### `internal/store/store.go` `[NEW]`
- pgx pool; `New(uri)`; `Migrate()`; transaction helpers.

### `internal/store/services.go` / `invocations.go` / `quotes.go` / `receipts.go` / `settlements.go` / `embeddings.go` / `grants.go` / `deployments.go` / `cursor.go` `[NEW]`
- One file per table group; typed CRUD with prepared statements. Big-int as
  `text`/`numeric`. pgvector via `pgvector-go`.

---

## 12.17 `internal/objstore`, `internal/auth`, `internal/receipts`, `internal/telemetry`

### `internal/objstore/objstore.go` `[NEW]`
- minio-go client; `Put(hash, r)`, `Get(key)`, `URL(key)`. Content-addressed.

### `internal/auth/auth.go` `[NEW]`
- `ResolveCaller` (agent bearer → DID + wallet), `ResolveDeveloper` (JWT/sig),
  `RequireRole`. DID verify mirrors gateway `auth.go`/wallet handshake.

### `internal/receipts/eip712.go` `[NEW]`
- Build domain separators + struct hashes for `DeusQuote` / `DeusReceipt`; sign;
  `recoverTypedSigner` to verify. Use `0x0908` for parity. Pure + tested.

### `internal/receipts/merkle.go` `[NEW]`
- Deterministic merkle over receipt digests; root + inclusion proofs.

### `internal/telemetry/{log.go,metrics.go}` `[NEW]`
- zerolog + Prometheus. Secret redaction. Standard metric names.

---

## 12.18 `pkg/` (public)

### `pkg/manifest/{manifest.go,canonical.go,schema.go,schema.json}` `[NEW]`
- Manifest Go types; canonical JSON encoder (stable key order, versioned);
  `Hash()` = keccak256(canonical); embedded JSON Schema + `Validate()`.
  `schema.json` is the embedded JSON Schema (the single source of manifest truth,
  shared with the console + `deusctl manifest validate`).

### `pkg/types/*.go` `[NEW]`
- Wire types (`Service`, `Endpoint`, `PricingPlan`, `Quote`, `Receipt`,
  `Invocation`, `DiscoverRequest/Result`). Stable JSON tags.

### `pkg/deusclient/client.go` `[NEW]`
- Go HTTP client mirroring `openapi.yaml`; used by `deusctl` + `test/e2e`.

---

## 12.19 `api/`, `configs/`, `migrations/`, `test/`

### `api/openapi.yaml` `[NEW]`
- Canonical REST contract for all `/v1` endpoints + schemas + error model.
  Source of truth for `pkg/deusclient`, the console client, and `deus-tools.json`.

### `configs/deus.<env>.yaml` `[NEW]`
- Ports, worker counts, window sizes, timeouts, feature flags. Typed-loaded.

### `configs/chain.<env>.json` `[NEW]`
- `{ "chainId": 125, "rpcUrl": "${PAXEER_RPC_URL}", "serviceRegistry": "0x..",
  "settlementAnchor": "0x..", "precompiles": {...} }`.

### `configs/limits.<env>.yaml` / `configs/ranking.yaml` `[NEW]`
- Resource caps / ranking weights.

### `migrations/001_init.sql` … `[NEW]`
- Tables from [`03-data-model.md`](./03-data-model.md) §3.2 + `create extension
  if not exists vector;`. Forward-only; one concern per migration.

### `test/e2e/flow_test.go` `[NEW]`
- list → discover → quote → invoke → settle against a fixture chain + sandbox
  runner. Golden receipt assertions. Mirror `tools/e2e` / `mcl-e2e` harness style.

### `test/fixtures/*.json` `[NEW]`
- Sample manifests (data + agent, proxy + hosted, confidential), golden quotes/
  receipts.

---

## 12.20 `contracts/` (Foundry)

### `contracts/foundry.toml` `[NEW]`
- `solc = "0.8.27"`, **`evm_version = "shanghai"`**, `src/`, `test/`, `lib/`,
  `[rpc_endpoints] paxeer = "${PAXEER_RPC_URL}"`.

### `contracts/src/ServiceRegistry.sol` `[NEW]`
- Per [`04-onchain.md`](./04-onchain.md) §4.2. NatSpec every external fn. Custom
  errors (no string reverts). Events exactly as specced (indexer contract).

### `contracts/src/SettlementAnchor.sol` `[NEW]`
- `anchor(...)` + `SettlementAnchored` event; role-gated to the settler.

### `contracts/src/interfaces/IServiceRegistry.sol` `[NEW]`
- External ABI interface; imported by consumers + binding generation.

### `contracts/test/*.t.sol` `[NEW]`
- forge tests: register/update/status/owner-transfer + event + access-control
  asserts.

### `contracts/script/Deploy.s.sol` `[NEW]`
- Broadcast deploy to chain 125; write addresses to `configs/chain.<env>.json`.

---

## 12.21 `runner/` (Node execution layer) — see [`11-modules.md`](./11-modules.md) §11.3
- `runner/src/harness.ts` `[NEW]` — wraps `handle()`, enforces caps, co-signs
  receipt, reports units/outcome. TS, ESM, strict.
- `runner/src/server.ts` `[NEW]` — private HTTP server the gateway calls.
- `runner/src/{egress.ts,secrets.ts,units.ts,sign.ts}` `[NEW]`.
- `runner/runtimes/{node20,container}/` `[NEW]` — runtime adapters.
- `runner/package.json` `[NEW]` — `"type":"module"`, pinned deps, scripts.

---

## 12.22 `console/` (Next.js) — mirror `client/`
- `console/app/{page.tsx, discover/page.tsx, develop/page.tsx, develop/[id]/page.tsx, spend/page.tsx, login/page.tsx}` `[NEW]`.
- `console/components/*` `[NEW]` — listing form, manifest editor (validates via
  `pkg/manifest` schema), analytics, search UI, spend dashboard.
- `console/lib/{deus.ts, auth.ts, wallet.ts, format.ts}` `[NEW]` — API client,
  Supabase auth, wallet link, formatters (PAX wei → display; ES6-safe big-int).
- `console/hooks/api/*.ts` `[NEW]` — react-query hooks.
- Style tokens, i18n deep-merge, and lint rules mirror `client/`. **No emojis/
  gradients/glow; depth via surface tone.**

---

## 12.23 MCP proxy + deploy (monorepo-level) — see [`10-integration.md`](./10-integration.md), [`13-deployment.md`](./13-deployment.md)

### `tools/deus/deus.mjs` `[NEW]`
- Node stdio MCP proxy. Local `initialize`/`tools/list` from
  `deus-tools.json`; forward `tools/call` to `MATRIX_DEUS_URL`; mint+forward
  agent wallet bearer for `deus_invoke`. `--selftest` drift guard. Mirror
  `tools/tachyon/tachyon.mjs` + `tools/browser/browser.mjs` byte-for-byte in
  structure.

### `tools/deus/deus-tools.json` `[NEW]`
- Exactly the tool set in [`10-integration.md`](./10-integration.md) §10.2.
  MUST stay in bijection with `agents/default.json`.

### `agents/default.json` `[EDIT]`
- Add the `deus` server + its tools. Exact name match (verifyTools is fatal on
  drift).

### `router/cmd/matrix-router/main.go` `[EDIT]`
- `MachineEnv`: inject `MATRIX_DEUS_URL` (+ token) like
  `MATRIX_BROWSER_URL`/`MATRIX_TACHYON_URL`.

### `deploy/daemon/Dockerfile` `[EDIT]`
- `COPY tools/deus` into the daemon image.

### `deploy/deus/{Dockerfile,fly.toml,README.md}` `[NEW]` (control plane only)
### `deploy/deus/runner/{node20,container}/` `[NEW]` (Paxeer Cloud function/Site templates — NOT a Fly app)
### `deploy/deus/deploy.sh` `[NEW]` (org-capable flyctl deploy for `deus-control`; mirror deploy/tachyon)
### `deploy/deus/install.sh` `[NEW]` (box: binary + systemd + migrations + env)

---

## 12.24 Cross-references & invariants to enforce in code

- `manifest_hash` (Go) == on-chain `manifestHash` == console-computed hash.
- Quote price == gateway charge == settled amount (shared `pkg/pricingmath`).
- `deus-tools.json` ⇆ `agents/default.json` bijection (daemon boot fatal else).
- EIP-712 digests identical between Go (`internal/receipts`) and the `0x0908`
  precompile.
- Indexer replay yields byte-identical mirror (CI `mirror-rebuild`).
- No caller key ever in Deus; all spend authorized by the embedded wallet.
