# 14 — Roadmap & Milestones

Phased so each milestone is independently shippable and demonstrably valuable.
Build order matches the dependency graph: chain + data first, then the invoke
loop on the payment layer, then hosting, then the trust features, then UI
polish.

> **Sequencing principle (superseded by the LXP re-platform).** The original
> plan phased deus-owned money machinery from simplest to hardest and carried
> the audit's HIGH-finding surface with it. That machinery is gone: every deus
> payment is now USDX between DIDs on the **LayerX ledger**, authorized in-band
> over HTTP by **LXP** ([`08-payments-billing.md`](./08-payments-billing.md)).
> Sub-cent economics come for free — a LayerX transfer is a ledger row, not a
> chain write — and the deletion landed only after the end-to-end no-fakes
> proofs were green on the new path.

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

## Phase 2 — The invoke loop (proxy, **LXP exact mode**) — **the hero MVP**
**Goal:** an agent discovers, quotes, invokes, and pays for a proxy service
end to end, USDX on LayerX, under its owner's spend leash.
- `internal/pricing` + `pkg/pricingmath` (micro-USDX), `internal/gateway`
  (+ quote, route), `internal/metering` (reserve/finalize/void),
  `internal/receipts` (EIP-712 + merkle, `layerxSeq` + `ref` cross-binding),
  `internal/layerx` (typed layerxd client + gateway DID identity),
  `pkg/lxp` (the middleware the gateway itself consumes), `internal/quality`
  (PoFQ).
- **Exact mode:** 402 challenge → payer signs the LayerX intent → settle →
  execute → `X-LayerX-Receipt`. No custody, no per-call gas, no api keys.
- `tools/deus/deus.mjs` + `deus-tools.json`; `agents/default.json` + router env.
- **Exit:** end-to-end `test/e2e/flow_test.go`: list → discover → quote →
  invoke → pay (LXP) → signed receipt → quality updated. A Matrix agent can
  do it via `deus_invoke` under the leash. **This is the launchable MVP.**

## Phase 2.5 — Hold mode + middleware kit (shipped with the re-platform)
**Goal:** pay-only-on-success + a drop-in paywall for third-party services.
- LayerX **holds** (authorize → capture/release, payer-authorized
  `captor_did`); gateway hold mode mapped onto meter reserve/finalize/void.
- `pkg/lxp` standalone Go middleware + the Node middleware (runner harness),
  one protocol implementation dogfooded by the gateway.
- **Exit:** hold lifecycle proven against the real ledger (no stranded funds,
  reserve invariant holds across interleavings); both middlewares pass the
  shared lxp/1 test vectors.

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

## Phase 6 — Continuous / long-running metering
**Goal:** bill long-running calls incrementally. The precompile streaming
plan was retired with the LXP re-platform; continuous billing composes from
LXP holds instead (authorize a ceiling → capture the metered charge, repeat
per window).
- **Exit:** a long-running agent service bills per window through holds;
  unspent remainder provably returns to the payer each capture.

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
- Agent invoke loop: quote → leash-checked → metered → **LXP pay on LayerX** →
  signed receipt → PoFQ quality.
- `deus.mjs` MCP integration so Matrix agents use it natively.
- Developer console + spend dashboard.
- Take-nothing economics (0% fee), leash-enforced spend safety, zero custody.

**Explicitly *not* in the launch MVP:** hosted on Paxeer Cloud (Phase 3),
continuous metering (6), and confidential (7) deepen the moat ("free hosting"
and "trusted/confidential first-class").

## Cross-phase quality gates (every phase)
- `deus-build` + `deus-test` + `deus-lint` green.
- `deus-contracts` (forge build/test) green when contracts change.
- `deus-mcp-selftest` green when the proxy/tool set changes.
- Mirror-rebuild test green (indexer determinism).
- No secret values committed; no UI emojis/gradients/glow; depth via surface
  tone.
