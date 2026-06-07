# 05 — API Surface

Deus exposes one HTTP API with three audiences distinguished by auth, not by
host: **developers** (manage listings), **callers/agents** (discover + invoke),
and **internal** (indexer/settler/runners). REST + JSON, versioned under `/v1`.

- Base URL (public): `https://deus.paxeer.app` (gateway-fronted).
- Content type: `application/json` unless noted.
- All money is **PAX wei as a decimal string**, never a number.
- All times are RFC3339 UTC.

## 5.1 Auth model

| Audience | Mechanism |
| -------- | --------- |
| Developer (console) | Supabase JWT **or** wallet signature (EIP-191/712) over a challenge |
| Developer (CI/programmatic) | Wallet-signed request: `Authorization: Wallet <addr>:<sig>` over canonical request hash |
| Caller / agent | `Authorization: Bearer <agent-wallet-token>` (the embedded-wallet agent bearer, ed25519 DID handshake) |
| Internal | mTLS / shared bearer on the private 6PN network only |

The agent bearer is the **same token** the embedded wallet issues
(`/v1/agent/auth/{challenge,verify}` in `paxeer-embeded-wallets`). Deus verifies
the DID and forwards spend authorization to the wallet — it never mints spend
authority itself.

## 5.2 Conventions

- **Errors** (uniform envelope):
  ```json
  { "error": "policy_denied", "message": "spend exceeds per-call cap", "detail": {"cap_wei":"1000","quote_wei":"2000"} }
  ```
  Codes: `invalid_request`, `unauthorized`, `forbidden`, `not_found`,
  `conflict`, `payment_required`, `policy_denied`, `quote_expired`,
  `service_unavailable`, `rate_limited`, `internal_error`.
- **Idempotency**: write/invoke endpoints accept `Idempotency-Key` header;
  replays return the original result.
- **Pagination**: `?limit=&cursor=`; responses include `next_cursor`.
- **Versioning**: breaking changes → `/v2`; additive changes are forward-compatible.

## 5.3 Registry endpoints (developer)

```
POST   /v1/services                 Create a listing (draft). Body: manifest.
GET    /v1/services/{id}            Get a listing (public).
PATCH  /v1/services/{id}            Update manifest/pricing (owner).
POST   /v1/services/{id}/publish    Validate + register on-chain → status=active.
POST   /v1/services/{id}/pause      status=paused.
POST   /v1/services/{id}/delist     status=delisted.
GET    /v1/services/{id}/analytics  Invocations, revenue, quality, latency (owner).
POST   /v1/services/{id}/payout     Set payout address (owner).
```

### `POST /v1/services` request
```json
{ "manifest": { /* see 03-data-model §3.5 */ } }
```
### response
```json
{ "id": "uuid", "slug": "weather.now", "status": "draft", "manifest_hash": "0x..", "validation": {"ok": true, "warnings": []} }
```

### Hosted listing extras
```
POST   /v1/services/{id}/artifact   multipart upload of code/container (hosted mode)
GET    /v1/services/{id}/deployment Deployment status (build/deploy/running)
POST   /v1/services/{id}/redeploy   Rebuild + redeploy
GET    /v1/services/{id}/logs       Tail runner logs (owner)
```

## 5.4 Discovery endpoints (public / agent)

```
GET    /v1/discover                 Structured + semantic search.
POST   /v1/discover                 Same, richer body (plain-language + filters).
GET    /v1/services/{id}/manifest   Machine-readable manifest (for agents).
GET    /v1/catalog                  Browseable paginated catalog.
```

### `POST /v1/discover` — the plain-language path
```json
{
  "query": "a weather API with high uptime under 0.001 PAX per call",
  "filters": { "kind": "data", "max_price_wei": "1000000000000000", "min_quality": "900000000000000000", "min_uptime_bps": 9900, "confidential": false },
  "limit": 10
}
```
### response
```json
{
  "results": [
    {
      "id": "uuid", "slug": "weather.now", "display_name": "Weather Now",
      "summary": "Current conditions and short-range forecast by lat/lng.",
      "kind": "data", "quality_score": "940000000000000000", "uptime_bps": 9970,
      "score": 0.93,                       // blended ranking score
      "operations": [{ "name": "forecast", "price_wei": "200000000000000", "unit": "call" }]
    }
  ],
  "next_cursor": null
}
```
See [`07-discovery.md`](./07-discovery.md) for ranking semantics.

## 5.5 Invocation endpoints (caller / agent) — the hero path

```
POST   /v1/quote/{service_id}       Get a signed price quote for an operation.
POST   /v1/invoke/{service_id}      Invoke an operation (meter + route + receipt).
GET    /v1/invocations/{id}         Invocation status + receipt.
GET    /v1/receipts/{invocation_id} Signed EIP-712 receipt (+ attestation).
```

### `POST /v1/quote/{service_id}`
```json
{ "operation": "forecast", "estimated_units": "1" }
```
→
```json
{
  "quote_id": "uuid", "service_id": "uuid", "operation": "forecast",
  "unit_price_wei": "200000000000000", "max_units": "1", "max_total_wei": "200000000000000",
  "pricing_version": 3, "expires_at": "2026-06-08T00:10:00Z",
  "eip712": { "domain": "DeusQuote", "digest": "0x..", "signature": "0x.." }
}
```

### `POST /v1/invoke/{service_id}`
```json
{
  "operation": "forecast",
  "args": { "lat": 37.77, "lng": -122.41 },
  "quote_id": "uuid",
  "payment": { "rail": "net" },          // net | stream | direct ; stream needs stream_id
  "idempotency_key": "client-uuid"
}
```
Headers: `Authorization: Bearer <agent-wallet-token>`.

**Gateway sequence** (see [`02-architecture.md`](./02-architecture.md) §2.5C):
authenticate → validate quote (hash matches on-chain pricing commitment, not
expired) → confirm wallet policy permits → reserve charge in ledger → route →
sign receipt → finalize.

→ success
```json
{
  "invocation_id": "uuid",
  "outcome": "ok",
  "result": { "tempC": 14.2, "summary": "Partly cloudy" },
  "charged_wei": "200000000000000",
  "latency_ms": 412,
  "receipt": { "digest": "0x..", "gateway_sig": "0x..", "runner_sig": "0x..", "attestation": null }
}
```
→ policy failure (no charge, no call)
```json
{ "error": "policy_denied", "message": "spend exceeds per-call cap", "detail": {"cap_wei":"100000000000000","quote_wei":"200000000000000"} }
```

### Streaming
```
POST   /v1/streams                  Open a stream for a service (proxies 0x0906 open()).
POST   /v1/streams/{id}/settle      Settle accrued.
POST   /v1/streams/{id}/close       Close + refund unspent cap.
GET    /v1/streams/{id}             Stream state (accrued, cap, settled).
```
Then `invoke` with `"payment": {"rail":"stream","stream_id":"..."}`.

## 5.6 Caller spend / account endpoints

```
GET    /v1/me                       Caller identity (DID, wallet), policy summary.
GET    /v1/me/spend                 Spend history, per-service totals, grants.
GET    /v1/me/grants                Active spend grants (mirror of wallet policy).
```
Spend grants are **read-only here**; they are set/changed in the wallet's owner
control plane ([`10-integration.md`](./10-integration.md)).

## 5.7 Internal endpoints (private network only)

```
POST   /internal/index/replay       Re-tail chain from a block (ops).
POST   /internal/settle/run         Trigger a settlement window (settler).
POST   /internal/runner/callback    Runner posts result + co-signature.
GET    /internal/healthz            Liveness/readiness (DB + chain RPC).
GET    /internal/metrics            Prometheus metrics.
```

## 5.8 Rate limits

- Per caller DID and per IP, token-bucket. Discovery is generous; invoke is
  bounded by wallet policy first, then a safety ceiling.
- `429 rate_limited` with `Retry-After`.

## 5.9 OpenAPI

The canonical machine spec lives at `deus/api/openapi.yaml` (generated/maintained
alongside this doc). The agent-facing subset (discover/quote/invoke) is also
published as MCP tool schemas for `deus.mjs` ([`10-integration.md`](./10-integration.md)).
