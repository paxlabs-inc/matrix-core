# 14 — Roadmap & Milestones

Phased so each milestone is independently shippable and demonstrably valuable.
Build order matches the dependency graph: chain + data first, then the invoke
loop **on the simplest payment rail**, then the harder money machinery, then
hosting, then the trust/streaming features, then UI polish.

> **Sequencing principle (why direct-rail first).** The escrow / payment-channel
> / net-settlement machinery is the hardest, highest-risk money code and carries
> the three load-bearing problems (caller co-signed vouchers, per-window funding,
> atomic reserve decrement). It is deliberately pulled **out of the launchable
> MVP**. Phase 2 proves the full loop on the **direct-transfer rail** (one inline
> wallet transfer per call — no escrow, no float, none of those three problems);
> Phase 2.5 adds the channel/net-settlement as the immediate fast-follow once the
> loop is validated. Caveat owned: the direct rail proves the *loop*, not the
> *sub-cent micro-payment economics* — the "fractions of a cent" headline is
> gated on Phase 2.5.

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

## Phase 2 — The invoke loop (proxy, **direct rail**) — **the hero MVP**
**Goal:** an agent discovers, quotes, invokes, and pays for a proxy service on
the simplest rail, end to end, under wallet policy.
- `internal/pricing` + `pkg/pricingmath`, `internal/gateway` (+ quote, route),
  `internal/metering` (reserve/finalize/void), `internal/receipts` (EIP-712 +
  merkle), `internal/wallet` (agent-auth + authorize spend), `internal/quality`
  (PoFQ).
- **Direct-transfer rail only:** one inline `agent/send` per call before result
  release. **No escrow, no payment channel, no net settlement, no caller
  voucher** — so none of the three hard money problems are in launch.
- `tools/deus/deus.mjs` + `deus-tools.json`; `agents/default.json` + router env.
- **Exit:** end-to-end `test/e2e/flow_test.go`: list → discover → quote →
  invoke → pay (direct) → signed receipt → quality updated. A Matrix agent can
  do it via `deus_invoke` under wallet policy. **This is the launchable MVP.**

## Phase 2.5 — Payment channel + net settlement (immediate fast-follow)
**Goal:** sub-cent economics — batch many tiny calls without a chain write each.
- `internal/settlement` (net rail) + `SettlementAnchor.sol`; per-window
  **payment channel** escrow contract; **caller co-signed cumulative vouchers**
  (`DeusVoucher` EIP-712); **atomic channel-balance reserve decrement**
  (`channels` row lock); highest-voucher redemption at settlement.
- Solves, in one design, the three coupled problems: caller-in-the-signing-loop
  (#1), per-window funding not per-reserve (#2), concurrent-reserve safety (#3).
- **Exit:** thousands of sub-cent calls settle in one tx/developer/window; caller
  holds a co-signed voucher proving the exact charge; oversell under concurrency
  is impossible (load test). Now the "fractions of a cent" claim is real.

## Phase 3 — Hosting (free hosting / hosted listings, on Paxeer Cloud)
**Goal:** "list your API and Paxeer runs it for you."
- `runner/` (harness + node20 runtime + container shim), `internal/hosting`
  (drives the **Paxeer Cloud / Appwrite Server API**: create function/Site,
  deploy, set variables, scale-to-zero), artifact upload endpoints, function
  templates under `deploy/deus/runner`.
- Free-hosting **budget + kill-switch** wired ([`06-execution-hosting.md`](./06-execution-hosting.md) §6.7).
- **Exit:** upload a node20 function → deployed as a Paxeer Cloud Function →
  invoked through the gateway → settled. Cold-start within budget; budget
  ceiling enforced.

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

## Phase 6 — Streaming rail
**Goal:** continuous / long-running calls (direct + net already shipped).
- `internal/settlement/rails.go` stream (`0x0906`); `/v1/streams` endpoints;
  gateway stream metering against `accrued()`.
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
- Agent invoke loop: quote → policy-checked → metered → **direct-rail pay** →
  signed receipt → PoFQ quality.
- `deus.mjs` MCP integration so Matrix agents use it natively.
- Developer console + spend dashboard.
- Take-nothing economics (0% fee), wallet-enforced spend safety.

**Explicitly *not* in the launch MVP:** escrow / payment channel / net
settlement (Phase 2.5), which is the immediate fast-follow that unlocks the
sub-cent economics. Hosted on Paxeer Cloud (Phase 3), streaming (6), and
confidential (7) deepen the moat ("free hosting" and "trusted/confidential
first-class").

## Cross-phase quality gates (every phase)
- `deus-build` + `deus-test` + `deus-lint` green.
- `deus-contracts` (forge build/test) green when contracts change.
- `deus-mcp-selftest` green when the proxy/tool set changes.
- Mirror-rebuild test green (indexer determinism).
- No secret values committed; no UI emojis/gradients/glow; depth via surface
  tone.
