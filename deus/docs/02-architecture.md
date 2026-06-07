# 02 — System Architecture

## 2.1 Design principles

1. **On-chain truth, off-chain speed.** The `ServiceRegistry` contract is the
   source of truth for listings, ownership, pricing commitments, and quality.
   Postgres is a rebuildable mirror for fast search and analytics.
2. **The chain is the rail, not a dependency to route around.** Payments,
   receipts, and reputation use Paxeer precompiles natively. No external payment
   network.
3. **Delegate custody, never re-implement it.** Spend limits and rules are the
   embedded wallet's / Argus's job. Deus *presents* policy outcomes; it never
   becomes a second custody authority.
4. **Stateless control plane.** Every Deus control-plane process is horizontally
   scalable and crash-safe; all durable state lives in Postgres + chain +
   object storage. Restarting a node loses nothing.
5. **Determinism at the settlement boundary.** Metering, pricing math, and
   receipt hashing are pure and reproducible so settlement and disputes are
   verifiable.
6. **One product.** A single `services` entity backs both "API marketplace" and
   "agent registry" views.
7. **Fail safe for spend, fail open for discovery.** A metering/settlement
   failure blocks the call; a search/index hiccup degrades to structured filters.

## 2.2 Component map

```text
                        Humans (browser)            AI Agents
                              |                          |
                              v                          v
                   +---------------------+    +-----------------------+
                   |  Console (Next.js)  |    |  deus.mjs MCP proxy    |
                   |  developer + spend  |    |  (in Matrix daemon)    |
                   +----------+----------+    +-----------+-----------+
                              |  HTTPS                    |  HTTP (agent API)
                              v                           v
        ============================ DEUS CONTROL PLANE (Go) ============================
        |                                                                              |
        |   +-------------+   +-------------+   +-------------+   +-----------------+   |
        |   |  Registry   |   |  Discovery  |   |  Gateway    |   |  Settlement     |   |
        |   |  API        |   |  (search)   |   |  (invoke)   |   |  (billing)      |   |
        |   +------+------+   +------+------+   +------+------+   +--------+--------+   |
        |          |                 |                 |                  |            |
        |          +--------+--------+--------+--------+---------+--------+            |
        |                   |                          |                              |
        |             +-----v-----+              +-----v------+                       |
        |             | Indexer   |              |  Metering  |                       |
        |             | (chain->db)|             |  ledger    |                       |
        |             +-----+-----+              +-----+------+                       |
        ===================|==============================|============================
                           |                              |
              +------------v-----------+        +----------v-----------+
              |  Postgres + pgvector   |        |  Object store (S3/    |
              |  (registry mirror,     |        |  MinIO: artifacts,    |
              |   ledger, embeddings)  |        |  receipts, logs)      |
              +------------------------+        +----------------------+
                           |                              |
        ===================|=========== ON-CHAIN (Paxeer 125) ==========================
        |   ServiceRegistry.sol   PaymentStreams 0x0906   PoFQ 0x0904                  |
        |   Settlement anchor     EIP712 0x0908           TEEAttestor 0x0907           |
        |   Embedded wallet / Argus policy plane          Scheduler 0x0905            |
        ================================================================================
                           |
              +------------v------------------------------+
              |  EXECUTION LAYER (Fly Machines, Node)      |
              |  - Hosted-service runners (scale-to-zero)  |
              |  - Proxy egress to developer endpoints     |
              |  - Confidential (TEE) runners (v1.x)       |
              +-------------------------------------------+
```

## 2.3 Components (responsibilities)

### Control plane (Go — one binary, sub-commands or one server with route groups)

| Component | Responsibility | Spec |
| --------- | -------------- | ---- |
| **Registry API** | CRUD on listings; manifest validation; ownership; submits registry txns | [`05-api.md`](./05-api.md) |
| **Discovery** | Structured filter queries + plain-language semantic search over pgvector | [`07-discovery.md`](./07-discovery.md) |
| **Gateway** | Authenticate caller → check policy/quote → meter → route → sign receipt | [`06-execution-hosting.md`](./06-execution-hosting.md) |
| **Metering ledger** | Append-only record of every invocation (price, tokens/units, outcome) | [`03-data-model.md`](./03-data-model.md) |
| **Settlement** | Batches ledger entries → net settlement / stream settle / direct transfer | [`08-payments-billing.md`](./08-payments-billing.md) |
| **Indexer** | Tails `ServiceRegistry` + settlement events → updates Postgres mirror | [`03-data-model.md`](./03-data-model.md) |
| **Quality** | Computes per-service score via PoFQ from delivery outcomes; writes on-chain | [`04-onchain.md`](./04-onchain.md) |
| **Hosting orchestrator** | Builds + deploys hosted listings to Fly; lifecycle, scale-to-zero | [`06-execution-hosting.md`](./06-execution-hosting.md) |

### Execution layer (Node on Fly)
- **Hosted runner** — runs a developer's uploaded container/function, invoked
  per call, scaled to zero when idle.
- **Proxy egress** — forwards to a developer's external HTTPS endpoint, applying
  timeout/size/retry policy and capturing the result for metering + receipts.
- **Confidential runner (v1.x)** — runs inside a TEE; emits an attestation quote
  verified by `0x0907`.

### Web (Next.js)
- Developer console (list/manage/analytics), discovery UI, spend dashboard,
  try-it console. Mirrors `client/` stack and house style.

### On-chain
- `ServiceRegistry.sol` + the agent precompiles. See [`04-onchain.md`](./04-onchain.md).

### Off-chain stores
- **Postgres + pgvector** — registry mirror, metering ledger, embeddings, caller
  spend snapshots, search analytics.
- **Object store (S3/MinIO)** — uploaded artifacts/containers, large request/
  response bodies (hashed), signed receipts, execution logs.

## 2.4 Runtime topology (where each piece lives)

| Tier | What runs there | Hosting |
| ---- | --------------- | ------- |
| Control plane | `deusd` Go server (registry/discovery/gateway/settlement/indexer/quality) | Fly app `deus-control` (public via gateway), or the shared Paxeer box; multiple instances behind Fly proxy |
| Execution | hosted runners + proxy egress | Fly app(s) `deus-runner-*`, scale-to-zero Machines per service or shared pool |
| Data | Postgres + pgvector, object store | The Paxeer box (Postgres, like matrix) + MinIO/S3 |
| Chain | `ServiceRegistry` + precompiles | Paxeer mainnet 125 |
| Web | Next.js console | Static/SSR deploy (Netlify/Fly), like `client/` |
| Agent bridge | `deus.mjs` MCP proxy | Inside each Matrix per-user daemon image |

See [`13-deployment.md`](./13-deployment.md) for the concrete Fly apps + env.

## 2.5 Key request flows

### A. Developer lists a service (proxy listing)
1. `POST /v1/services` with manifest (name, description, schema, pricing,
   endpoint URL, payout address). Auth: developer wallet signature or Supabase
   JWT.
2. Registry API validates the manifest (JSON Schema + pricing sanity + URL
   reachability probe).
3. Registry API submits `ServiceRegistry.register(...)` on-chain (developer pays
   gas, or Deus relays via agent fee lane). Returns `service_id`.
4. Indexer observes `ServiceRegistered` → upserts the Postgres row.
5. Discovery computes the embedding for the manifest and inserts into pgvector.
6. Service is live and discoverable. Time target: < 10 min, mostly the dev typing.

### B. Developer lists a service (hosted listing)
Same as A, plus: artifact upload → object store → Hosting orchestrator builds a
container → deploys a scale-to-zero Fly Machine → endpoint URL is the internal
runner URL. The on-chain listing records `hosted=true`.

### C. Agent discovers + calls a service (the hero path)
1. Agent (via `deus.mjs` or agent API) sends a **plain-language need**:
   `"weather API, >99% uptime, under 0.001 PAX/call"`.
2. Discovery returns ranked candidates (semantic + filters + quality score).
3. Agent (or its planner) picks one; calls `POST /v1/invoke/{service_id}` with
   args + an **invocation authorization** from its agent wallet (a bounded
   spend grant).
4. Gateway: authenticates the caller DID → fetches a **price quote** → checks the
   wallet policy permits this spend → **debits the metering ledger** (reserve).
5. Gateway routes to the runner (hosted) or proxies to the developer endpoint.
6. On success: result returned to agent; an **EIP-712 call receipt** is signed;
   the ledger entry is finalized; the service's quality sample is recorded.
7. Settlement later **net-settles** the developer's payout on-chain.
8. PoFQ score updated from the delivery outcome.

### D. Continuous use (streaming)
For long/continuous services, the caller opens a PaymentStreams (`0x0906`)
stream to the developer; the gateway meters against accrual and `settle()`s
periodically; `close()` refunds the unspent cap.

### E. Confidential call (v1.x)
Same as C, but the runner executes in a TEE and returns an attestation quote;
the gateway verifies it via `0x0907` (`verifyAndExpect` against the expected
report data) before finalizing the receipt and releasing payment.

## 2.6 Trust & determinism boundaries

- **Pricing math** (units → PAX) is a pure function, versioned, identical in Go
  (settlement) and the published quote. A quote is a signed promise of price.
- **Receipt hash** = canonical hash of `{caller, service_id, args_hash,
  result_hash, price, units, outcome, ts}`. EIP-712-signed by the gateway and
  (optionally) the runner. Anchored to the chain in batches.
- **Metering ledger** is append-only; settlement reads it, never mutates history.
- **Indexer is idempotent** and replay-safe: re-tailing from any block yields the
  same Postgres mirror (chain is truth).

## 2.7 Failure modes & degradation

| Failure | Behavior |
| ------- | -------- |
| Search/embedding down | Fall back to structured filters; log; never block listing or invoke |
| Indexer lag | Reads may be slightly stale; invoke still works (gateway reads chain for price commitments on cache miss) |
| Runner cold/unreachable | Gateway returns structured `service_unavailable`; no charge; quality sample = failure |
| Settlement batch fails | Ledger retains unsettled entries; retried with backoff; never double-charges (idempotency keys) |
| Wallet policy denies | `402 payment_required` / `403 policy_denied`; no call made |
| Chain RPC down | Invoke blocked for new spend (fail-safe); reads serve from mirror |

## 2.8 Scaling posture

- Control plane: stateless, scale by instance count behind Fly proxy.
- Gateway throughput dominated by metering writes → batch + use idempotency
  keys; net settlement amortizes chain writes (one settlement per
  developer/window, not per call).
- Runners: per-service scale-to-zero; hot services pinned warm.
- Postgres: read replicas for discovery; pgvector index (HNSW) for search.
- Hard ceiling for v1 mirrors the launch cap (Fly machine budget); raise later.
