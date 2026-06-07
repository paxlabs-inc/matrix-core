# 06 — Execution & Hosting

This is the "Paxeer runs it for you" layer and the invocation gateway that sits
in front of it. Two listing modes, one gateway, scale-to-zero compute on
**Paxeer Cloud** (Paxeer's deployed Appwrite fork).

## 6.1 Listing modes

| Mode | Who runs the code | When to use |
| ---- | ----------------- | ----------- |
| **Proxy** | The developer (their own HTTPS endpoint). Deus meters + settles + signs receipts. | Existing APIs, services that must run in the dev's infra. |
| **Hosted** | Paxeer Cloud (an Appwrite Function or container Site built from uploaded code). Free hosting. | Devs who don't want to run servers; the "list and earn" path. |
| **Confidential (v1.x)** | Paxeer Cloud TEE-backed execution (or a dedicated runner); attestation verified on-chain. | Regulated/enterprise, verifiable compute. |

Mode is recorded in the manifest and on-chain (`hosted`, `confidential`).

## 6.2 The Invocation Gateway (control plane, Go)

The gateway is the single choke point for every billable call. Pipeline (each
stage fails fast with a structured error):

```text
authenticate caller (agent bearer / wallet)            -> 401
  └─ resolve caller DID + wallet address
validate quote (matches on-chain pricingHash, !expired) -> 409 quote_expired / 400
  └─ recompute price; verify EIP-712 signature
check spend policy (embedded wallet + grants cache)     -> 403 policy_denied / 402
  └─ confirm against wallet on the spend path (not just cache)
reserve = ATOMIC channel-balance decrement + ledger row (idempotent)  -> 409 dup / 402 insufficient
  └─ single transactional UPDATE; never a per-call chain write (see below)
route:
  proxy   -> egress to manifest.endpoint.proxy_url
  hosted  -> invoke Paxeer Cloud function execution / function domain
  conf.   -> TEE-backed execution; require attestation
apply request policy (timeout, max bytes, retries)      -> 503 service_unavailable
capture result, hash args+result
sign receipt (gateway EIP-712, runner co-sign if hosted)
finalize ledger entry + record quality sample
return result to caller
```

### Metering
- The gateway computes `units` from the operation's pricing model:
  - `per_call` → 1 unit.
  - `per_unit` → units reported by the runner/proxy in a trailer/header
    (`X-Deus-Units`) or derived (e.g. tokens).
  - `per_second` (stream) → metered against `0x0906` accrual, not per call.
- `price_wei = unit_price_wei * units`, floored at `min_charge_wei`. Pure,
  versioned math (`pkg/pricing`) shared with settlement.

### Reserve → finalize (no double charge)
- On accept, write a ledger row in `reserved` state keyed by `idempotency_key`.
- On result, transition to `finalized` with the real `units`/`outcome`.
- On failure/timeout, transition to `voided` (no charge) and record a failure
  quality sample.
- Settlement only ever reads `finalized` rows.

### Reserve invariant — atomic channel-balance decrement (load-bearing)
The control plane is stateless and N-instance (§2.8) and the `spend_grants`
cache is a *fast pre-check only* (§9.3). Two concurrent invokes with different
idempotency keys can both clear the cache and **oversell** the caller's payment
channel before any authoritative check fires. Therefore the reserve **must** be
a single serialized, transactional decrement of the caller's channel/escrow
balance — not a cache read:

```sql
UPDATE channels
   SET reserved = reserved + :max_total_wei
 WHERE channel_id = :id
   AND (balance - reserved) >= :max_total_wei;   -- 0 rows affected => 402 insufficient
```

- The decrement is a **Postgres row lock** (the per-caller channel row is the
  serialization point), bounded by the on-chain escrow cap. It is **never** an
  on-chain write per reserve (that would kill lazy-net, [`08-payments-billing.md`](./08-payments-billing.md) §8.3).
- The off-chain ledger is thus the concurrency authority *within* a window; the
  escrow contract is the authority *across* windows.
- `finalize` releases the unused portion of the reservation back to
  `balance - reserved`; `void` releases the whole reservation.
- This is an implementation-critical invariant: violating it lets a caller (or
  a buggy parallel agent) spend past its funded channel.

## 6.3 Hosted runner (Paxeer Cloud / Appwrite fork)

Hosted execution runs on **Paxeer Cloud**, Paxeer's deployed Appwrite fork,
which already provides multi-runtime serverless **Functions**, **container
Sites**, **databases**, **edge/web-workers**, and **proxies** with native
scale-to-zero. Deus does not hand-roll machine orchestration; it drives the
Appwrite **Server API** and lets the platform handle build, routing, scaling,
secrets, and logs.

### Build / deploy pipeline (Hosting orchestrator → Appwrite Server API)
1. Developer uploads an artifact: either (a) a source bundle with a declared
   runtime (`node20`, `python311`, `static`, …) or (b) a container image ref.
   v1 supports **node20 function** + **container Site** first.
2. The orchestrator creates/updates a **Paxeer Cloud Function** (source bundle)
   or **Site** (container) via the Appwrite Server API; Appwrite builds and
   deploys it. No Fly registry/Machine management.
3. Per-service secrets are set as **Appwrite function variables**; resource caps
   (timeout, memory) are set on the function spec.
4. Record `deployments` row + `runner_ref` = the function's **execution endpoint
   / function domain** (used by the gateway to invoke; via the Appwrite
   `executions` API or the function's HTTP domain).

### Runner contract (the harness the uploaded code runs inside)
Hosted code implements a tiny handler interface; the harness wraps it:
```ts
// developer implements:
export async function handle(op: string, args: unknown, ctx: DeusCtx): Promise<unknown>
// ctx gives: { callerDid, invocationId, deadlineMs, logger, secrets }
```
The harness runs *inside* the Paxeer Cloud function/Site and:
- Receives the gateway's invoke (function execution payload or HTTP request).
- Enforces `timeout_ms`, `max_response_bytes` (within Appwrite's own function
  timeout/memory caps).
- Reports `units` and `outcome`, **co-signs** the receipt with the runner key.
- Emits logs (captured by Paxeer Cloud; mirrored to the object store).
- Is **network-egress-restricted** by default (allowlist) — see [`09-security.md`](./09-security.md).

### Scale-to-zero
- Paxeer Cloud functions are **natively scale-to-zero** and cold-start on
  invocation, so Deus does not run a bespoke `EnsureStarted`/suspend loop — the
  platform owns lifecycle. The gateway treats a cold invocation's latency as
  first-call latency and surfaces a `cold_start_ms` hint in quotes.
- Hot services can request **always-warm** execution (a developer setting);
  always-warm capacity counts against the free-hosting budget (§6.7) and may
  require the developer to stake/pay or earn it via volume.

## 6.4 Proxy egress

- The gateway (or a dedicated egress worker) calls `manifest.endpoint.proxy_url`
  with the args, applying the operation's `timeout_ms`, `max_response_bytes`, and
  a bounded retry (idempotent ops only).
- Captures status/latency/result; non-2xx or schema-invalid (for `data` kind)
  → `outcome=error`, no charge (or partial per policy), failure quality sample.
- The developer's endpoint may require a shared secret Deus injects
  (`X-Deus-Service-Secret`) so only Deus can call the paid endpoint.

## 6.5 Where it runs (concrete)

| Tier | Purpose | Platform |
| ---- | ------- | -------- |
| `deus-control` (`deusd` Go) | control plane / public API | Fly app **or** the Paxeer box **or** a Paxeer Cloud container; N instances, stateless |
| Hosted service functions | developers' hosted node20 functions | **Paxeer Cloud** Functions (scale-to-zero, multi-tenant) |
| Hosted service Sites | heavier / containerized services | **Paxeer Cloud** container Sites |
| Confidential services (v1.x) | TEE-backed execution | Paxeer Cloud TEE runtime, else a dedicated runner |
| Postgres + pgvector + object store | data | the Paxeer box (Postgres + MinIO/S3) |

The control plane (Fly/box) and Paxeer Cloud should sit in the same region to
keep gateway↔function latency low.

## 6.6 Cold-start budget

- A cold Paxeer Cloud function adds its cold-start to first-call latency.
  Mitigations: prefer the shared node20 function runtime (warm pool semantics
  where Paxeer Cloud supports it), allow opt-in always-warm for hot services
  (§6.7 budget), and surface a `cold_start_ms` hint in quotes so agents can
  prefer warm services.

## 6.7 Limits, quotas & the free-hosting budget (v1)

- Per-service caps — max artifact size, max response bytes, max `timeout_ms`
  (e.g. 30s), max memory — live in `configs/limits.<env>.yaml` and are applied
  to the Paxeer Cloud function/Site spec + enforced by the harness.
- **Free hosting is a subsidy and gets an explicit budget, not an emergent one.**
  Because the platform fee is 0%, hosted execution is a deliberate cost center.
  v1 commits to an **aggregate free-hosting budget** (a number in
  `configs/limits.<env>.yaml`) with an allocation policy and a **kill-switch**,
  rather than letting the ceiling be whatever the Paxeer Cloud bill happens to
  be:
  - **Free tier = scale-to-zero functions only.** Idle hosted services cost
    ~nothing; cold unused services are evicted after an inactivity window.
  - **Always-warm / dedicated capacity is not free** — the developer stakes/pays
    PAX or earns warmth via invoke volume.
  - The orchestrator tracks aggregate hosted consumption against the budget and
    **refuses new always-warm/dedicated allocations** past the ceiling (new
    scale-to-zero functions still allowed), alerting ops.
- This makes "free hosting for N listings" a chosen, bounded subsidy that the
  business bets recovers via PAX/network activity — with a tripwire, not a
  surprise invoice.
