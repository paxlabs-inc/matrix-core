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
authenticate caller (bearer + X-Caller-DID)             -> 401
price (quote or fresh lxp/1 challenge terms)            -> 402 challenge / 409 quote_expired
  └─ recompute price; verify EIP-712 quote signature when quote_id given
verify X-LayerX-Payment (shape, amount, terms match)    -> fresh 402 challenge
reserve metering row (idempotent, keyed idempotency_key) -> replay returns stored result
settle:
  exact -> (after execute) submit the payer-signed pay intent to layerxd
  hold  -> CreateHold BEFORE execute (captor = gateway DID)  -> 402 insufficient
route:
  proxy   -> egress to manifest.endpoint.proxy_url
  hosted  -> invoke Paxeer Cloud function execution / function domain
  conf.   -> TEE-backed execution; require attestation
apply request policy (timeout, max bytes, retries)      -> 503 service_unavailable
capture result, hash args+result
sign receipt (gateway EIP-712 + layerx_seq + ref; runner co-sign if hosted)
finalize (capture hold / record seq) or void (release)  -> layerxd down = 503, no free call
return result + X-LayerX-Receipt to caller
```

### Metering
- The gateway computes `units` from the operation's pricing model:
  - `per_call` → 1 unit.
  - `per_unit` → units reported by the runner/proxy in a trailer/header
    (`X-Deus-Units`) or derived (e.g. tokens).
- `charge_usdx = unit_price_usdx * units`, floored at `min_charge_usdx`
  (micro-USDX, 6dp). Pure, versioned math (`pkg/pricingmath`).

### Reserve → finalize (no double charge)
- On accept, write a ledger row in `reserved` state keyed by `idempotency_key`.
- On result, transition to `finalized` with the real `units`/`outcome`.
- On failure/timeout, transition to `voided` (no charge — the hold is
  released) and record a failure quality sample.
- `finalized` rows carry the LayerX `layerx_seq` (+ `hold_id` in hold mode),
  cross-binding paid ↔ served.

### Reserve invariant — the LayerX ledger is the concurrency authority
The control plane is stateless and N-instance (§2.8), so deus never arbitrates
concurrent spend itself. Funds-safety is delegated to the LayerX ledger, where
every debit is a single serialized transaction against the payer's spendable
balance (`FOR UPDATE` row lock; insufficient spendable → `402
insufficient_funds`):

- **exact mode** — the pay intent debits payer → payee atomically at settle
  time; two concurrent invokes can never spend the same USDX twice.
- **hold mode** — `CreateHold` debits the payer's spendable balance into an
  open hold row *before* execution; capture consumes the hold through the
  standard transfer path and returns any remainder in the same transaction;
  release/expiry refunds in full ([`08-payments-billing.md`](./08-payments-billing.md) §8.3).

Deus-side, the metering row keyed by `idempotency_key` remains the
replay/no-double-charge spine: reserve → finalize (with `layerx_seq`) or void.
A caller can never be charged past what its own signed intent authorizes —
the signature carries the exact amount (invariant i6).

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
