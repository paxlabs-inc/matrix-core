# 14 — Roadmap & Milestones

Phased so each milestone is independently shippable and demonstrably valuable.
Build order matches the dependency graph: chain + data first, then the invoke
loop, then hosting, then the trust/streaming features, then UI polish.

## Phase 0 — Foundations (scaffold → buildable)
**Goal:** the module compiles, migrates, and connects to chain + DB.
- `go.mod`, `internal/config`, `internal/store` + `migrations/001_init.sql`,
  `internal/chain/client.go`, `internal/objstore`, `cmd/deusd` boot skeleton,
  `cmd/deusctl migrate`.
- `pkg/manifest` (types + canonical hash + JSON Schema) + `pkg/types`.
- **Exit:** `deus-build` + `deus-test` green; `deusctl migrate` creates schema;
  `deusctl manifest validate` works.

## Phase 1 — Registry (proxy listings, on-chain truth)
**Goal:** a developer can list a proxy service and it's on-chain + discoverable.
- `contracts/ServiceRegistry.sol` + tests + deploy to chain 125.
- `internal/registry` (+ validate), `internal/indexer`, registry handlers.
- `internal/discovery` minimal (lexical + filters; embeddings stubbed).
- **Exit:** `POST /v1/services` → publish → on-chain `ServiceRegistered` →
  indexer mirrors → `GET /v1/discover` returns it. Mirror-rebuild test passes.

## Phase 2 — The invoke loop (proxy, net settlement) — **the hero MVP**
**Goal:** an agent discovers, quotes, invokes, and pays for a proxy service.
- `internal/pricing` + `pkg/pricingmath`, `internal/gateway` (+ quote, route),
  `internal/metering`, `internal/receipts` (EIP-712 + merkle),
  `internal/wallet` (agent-auth + authorize spend), `internal/settlement`
  (net rail) + `SettlementAnchor.sol`, `internal/quality` (PoFQ).
- `tools/deus/deus.mjs` + `deus-tools.json`; `agents/default.json` + router env.
- **Exit:** end-to-end `test/e2e/flow_test.go`: list → discover → quote →
  invoke → signed receipt → net settlement → quality updated. A Matrix agent can
  do it via `deus_invoke` under wallet policy. **This is the launchable MVP.**

## Phase 3 — Hosting (free hosting / hosted listings)
**Goal:** "list your API and Paxeer runs it for you."
- `runner/` (harness + node20 runtime + container shim), `internal/hosting`
  (build + deploy + scale-to-zero + EnsureStarted), artifact upload endpoints,
  `deploy/deus/runner`.
- **Exit:** upload a node20 function → hosted on Fly → invoked through the
  gateway → settled. Cold-start within budget.

## Phase 4 — Discovery quality (plain-language search)
**Goal:** "describe what you need, get the right service."
- `internal/discovery/embed.go` (real embedder) + pgvector HNSW +
  `extract.go` (constraint extraction) + `rank.go` (blended score) +
  `configs/ranking.yaml`.
- **Exit:** plain-language query returns correctly ranked results; quality drives
  visibility; graceful degradation verified.

## Phase 5 — Console
**Goal:** humans can list, manage, and watch spend.
- `console/` (Next.js): develop (list/manage/analytics), discover, spend
  dashboard, login + wallet link.
- **Exit:** a developer lists + earns from the console in < 10 min; a human can
  browse + try + see spend.

## Phase 6 — Streaming & direct rails
**Goal:** continuous and high-value calls.
- `internal/settlement/rails.go` stream (`0x0906`) + direct; `/v1/streams`
  endpoints; gateway stream metering.
- **Exit:** a long-running agent service bills by the second via a stream;
  refund on close verified.

## Phase 7 — Trust: confidential services (TEE)
**Goal:** regulated/enterprise + verifiable compute, first-class.
- Confidential runner + `0x0907` `verifyAndExpect` gating in the gateway;
  attestation stored on receipts.
- **Exit:** a confidential service's payment releases only on a passing
  attestation bound to the result hash.

## Phase 8 — Recurring & ecosystem
**Goal:** scheduled calls + register the existing toolset as services.
- Scheduler (`0x0905`) recurring invocations via an invoke-relay.
- List `matrix-browser`, `matrix-tachyon`, `paxeer-net` as Deus services
  (Deus becomes the canonical agent-service registry).
- HPS standard schemas (offer / payment-request / reputation-query / evidence).
- IBC cross-chain service calls (later).

---

## MVP definition (what "v1" means)

**Phases 0–2 + the proxy path of 1**, plus enough of Phase 4 to make discovery
useful and Phase 5 for developer onboarding:

- Proxy listings, on-chain registry + mirror.
- Plain-language + filtered discovery with quality ranking.
- Agent invoke loop: quote → policy-checked → metered → signed receipt → net
  settlement → PoFQ quality.
- `deus.mjs` MCP integration so Matrix agents use it natively.
- Developer console + spend dashboard.
- Take-nothing economics (0% fee), wallet-enforced spend safety.

Hosted (Phase 3), streaming (6), and confidential (7) are fast-follows that
deepen the moat ("free hosting" and "trusted/confidential first-class").

## Cross-phase quality gates (every phase)
- `deus-build` + `deus-test` + `deus-lint` green.
- `deus-contracts` (forge build/test) green when contracts change.
- `deus-mcp-selftest` green when the proxy/tool set changes.
- Mirror-rebuild test green (indexer determinism).
- No secret values committed; no UI emojis/gradients/glow; depth via surface
  tone.
