# 06 — Execution & Hosting

This is the "Paxeer runs it for you" layer and the invocation gateway that sits
in front of it. Two listing modes, one gateway, scale-to-zero compute on Fly.

## 6.1 Listing modes

| Mode | Who runs the code | When to use |
| ---- | ----------------- | ----------- |
| **Proxy** | The developer (their own HTTPS endpoint). Deus meters + settles + signs receipts. | Existing APIs, services that must run in the dev's infra. |
| **Hosted** | Paxeer (Fly Machine built from uploaded code/container). Free hosting. | Devs who don't want to run servers; the "list and earn" path. |
| **Confidential (v1.x)** | Paxeer, inside a TEE; attestation verified on-chain. | Regulated/enterprise, verifiable compute. |

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
reserve charge in metering ledger (idempotent)          -> 409 on dup key (returns prior)
route:
  proxy   -> egress to manifest.endpoint.proxy_url
  hosted  -> EnsureStarted(machine) then call runner_ref
  conf.   -> TEE runner; require attestation
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

## 6.3 Hosted runner (Node on Fly)

### Build pipeline (Hosting orchestrator)
1. Developer uploads an artifact: either (a) a container image ref, or (b) a
   source bundle with a declared runtime (`node20`, `python311`, `static`,
   `wasm`). v1 supports **container** + **node20 function** first.
2. For source bundles, build with a baked builder image (Nixpacks/Buildpacks or a
   thin Dockerfile per runtime) → push to `registry.fly.io/deus-runner-<id>`.
3. Create/Update a Fly app `deus-svc-<id>` (or a shared pool slot) with
   `auto_stop_machines=suspend`, `min_machines_running=0`. Private (6PN only).
4. Record `deployments` row + `runner_ref` = `http://deus-svc-<id>.internal:PORT`.

### Runner contract (the harness the uploaded code runs inside)
Hosted code implements a tiny handler interface; the harness wraps it:
```ts
// developer implements:
export async function handle(op: string, args: unknown, ctx: DeusCtx): Promise<unknown>
// ctx gives: { callerDid, invocationId, deadlineMs, logger, secrets }
```
The harness:
- Receives the gateway's invoke over the private network.
- Enforces `timeout_ms`, `max_response_bytes`, memory/CPU caps.
- Reports `units` and `outcome`, **co-signs** the receipt with the runner key.
- Streams logs to the object store.
- Is **network-egress-restricted** by default (allowlist) — see [`09-security.md`](./09-security.md).

### Scale-to-zero
- Idle machines suspend (Fly `suspend`); the gateway calls `EnsureStarted` before
  routing and waits for `/healthz` (reuse the matrix-router `waitDaemonReady`
  pattern: any HTTP response = ready, transport errors retry, timeout → 503 +
  `Retry-After`).
- Hot services can pin `min_machines_running >= 1` (developer setting; may carry
  a PAX cost to the dev later, but free in v1 within the fleet cap).

## 6.4 Proxy egress

- The gateway (or a dedicated egress worker) calls `manifest.endpoint.proxy_url`
  with the args, applying the operation's `timeout_ms`, `max_response_bytes`, and
  a bounded retry (idempotent ops only).
- Captures status/latency/result; non-2xx or schema-invalid (for `data` kind)
  → `outcome=error`, no charge (or partial per policy), failure quality sample.
- The developer's endpoint may require a shared secret Deus injects
  (`X-Deus-Service-Secret`) so only Deus can call the paid endpoint.

## 6.5 Where it runs (concrete)

Mirrors the proven `deploy/browser` + `deploy/tachyon` shared-private-app pattern.

| App | Purpose | Public? | Shape |
| --- | ------- | ------- | ----- |
| `deus-control` | Go control plane (`deusd`) | Yes (behind gateway TLS) | N instances, stateless |
| `deus-runner` (shared pool) | hosted node20 functions, multi-tenant | No (6PN) | scale-to-zero pool |
| `deus-svc-<id>` | dedicated container per heavy/confidential service | No (6PN) | scale-to-zero per service |
| Postgres + MinIO | data | No | on the Paxeer box |

Region: primary near the Paxeer box / matrix fleet (e.g. `fra`/`iad`); pin
runners to the same region to keep gateway↔runner latency low.

## 6.6 Cold-start budget

- Gateway adds `EnsureStarted + waitReady` to first-call latency for a cold
  hosted service. Mitigations: keep a warm shared `deus-runner` pool for node20
  functions (no per-service cold start), suspend (not stop) so resume is fast,
  and surface a `cold_start_ms` hint in quotes so agents can prefer warm
  services.

## 6.7 Limits & quotas (v1)

- Max artifact size, max response bytes, max `timeout_ms` (e.g. 30s), max memory
  per runner — all in `configs/limits.<env>.yaml`.
- Fleet machine cap mirrors the launch ceiling; the orchestrator refuses new
  dedicated machines past the cap and falls back to the shared pool.
