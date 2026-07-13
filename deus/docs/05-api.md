# 05 — API Surface

Deus exposes one HTTP API with three audiences distinguished by auth, not by
host: **developers** (manage listings), **callers/agents** (discover + invoke),
and **internal** (indexer/runners). REST + JSON, versioned under `/v1`.

- Base URL (public): `https://deus.paxeer.app` (gateway-fronted).
- Content type: `application/json` unless noted.
- All money is a **decimal string**, never a number. Pay-path amounts are
  **USDX at 6dp** (`*_usdx`); legacy `*_wei` fields linger on some analytics
  responses and are `0`/omitted on USDX-priced plans.
- All times are RFC3339 UTC.

## 5.1 Auth model

| Audience | Mechanism |
| -------- | --------- |
| Developer (console) | Supabase JWT **or** wallet signature (EIP-191/712) over a challenge |
| Developer (CI/programmatic) | Wallet-signed request: `Authorization: Wallet <addr>:<sig>` over canonical request hash |
| Caller / agent | `Authorization: Bearer <token>` + `X-Caller-DID` (identifies the caller for challenge prefetch) |
| Internal | mTLS / shared bearer on the private 6PN network only |

The caller bearer only **identifies**; it never authorizes spend. Payment
authority is the payer's own ed25519 signature over the canonical LayerX
intent preimage, carried in-band via `X-LayerX-Payment` (LXP — see
[`08-payments-billing.md`](./08-payments-billing.md)). Deus never mints spend
authority.

## 5.2 Conventions

- **Errors** (uniform envelope):
  ```json
  { "error": "quote_expired", "message": "quote expired", "detail": {"quote_id":"uuid"} }
  ```
  Codes: `invalid_request`, `unauthorized`, `forbidden`, `not_found`,
  `conflict`, `payment_required`, `payment_unavailable`, `quote_expired`,
  `service_unavailable`, `rate_limited`, `internal_error`. Payment failures
  answer with a **fresh 402 lxp/1 challenge** (new nonce, machine-readable
  reason); a payment-rail outage is `503 payment_unavailable` — never a free
  call.
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
```

Priced services **must** register a `payee_did` (validated `did:matrix` shape)
in the manifest — every LXP settlement pays it directly on LayerX.

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
POST   /v1/services/{id}/artifacts    multipart upload of code/container (hosted mode)
POST   /v1/services/{id}/deploy       Build + deploy
POST   /v1/services/{id}/redeploy     Rebuild + redeploy
GET    /v1/services/{id}/deployments  Deployment status (build/deploy/running)
GET    /v1/services/{id}/logs         Tail runner logs (owner)
```

## 5.4 Discovery endpoints (public / agent)

```
GET    /v1/discover                 Structured + semantic search.
POST   /v1/discover                 Same, richer body (plain-language + filters).
GET    /v1/services/{id}            Public listing (manifest included).
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
  "unit_price_usdx": "0.031500", "max_units": "1", "max_total_usdx": "0.031500",
  "pricing_version": 3, "expires_at": "2026-06-08T00:10:00Z",
  "eip712": { "domain": "DeusQuote", "digest": "0x..", "signature": "0x.." }
}
```
The quote round-trip is **optional**: the 402 challenge carries equivalent
terms, so an agent can go straight to invoke.

### `POST /v1/invoke/{service_id}`
```json
{
  "operation": "forecast",
  "args": { "lat": 37.77, "lng": -122.41 },
  "quote_id": "uuid",
  "idempotency_key": "client-uuid"
}
```
Headers: `Authorization: Bearer <token>`, `X-Caller-DID: did:matrix:...`, and
on the paid retry `X-LayerX-Payment: <base64url payment>`.

**Gateway sequence** (see [`02-architecture.md`](./02-architecture.md) §2.5C):
authenticate → price (quote or challenge terms) → verify the payer-signed
payment → reserve the metering row → settle on LayerX (exact) or hold →
execute → capture/void → sign receipt → respond with `X-LayerX-Receipt`.

→ unpaid request: `402 Payment Required` with the lxp/1 terms body
([`08-payments-billing.md`](./08-payments-billing.md) §8.2).

→ success
```json
{
  "invocation_id": "uuid",
  "outcome": "ok",
  "result": { "tempC": 14.2, "summary": "Partly cloudy" },
  "charged_usdx": "0.031500",
  "latency_ms": 412,
  "layerx_seq": 4182,
  "ref": "0x...",
  "receipt": { "digest": "0x..", "gateway_sig": "0x..", "runner_sig": "0x.." }
}
```
plus header `X-LayerX-Receipt: base64url({seq, leaf_hash, sequencer_sig,
amount_usdx, ref})` — the payment receipt, cross-bound to the execution
receipt via `layerx_seq` + `ref`.

## 5.6 Caller / developer account endpoints

```
GET    /v1/me                       Caller identity (DID) summary.
GET    /v1/me/spend                 Spend history, per-service totals.
GET    /v1/me/services              Developer's listings (owner).
GET    /v1/me/earnings              Invocation earnings joined with LayerX reads
                                    (balance, escrow, transfers) + the LayerX
                                    /v1/withdraw link-out.
```
Spend limits live **client-side** (the daemon leash: `LAYERX_MAX_SPEND_USDX`,
`LAYERX_MAX_DAILY_USDX`) — deus never mints or caps spend authority; the
payer's signature is the authorization.

## 5.7 Internal endpoints (private network only)

```
GET    /internal/healthz            Liveness/readiness (DB + chain RPC).
GET    /internal/metrics            Prometheus metrics.
```

## 5.8 Rate limits

- Per caller DID and per IP, token-bucket. Discovery is generous; invoke is
  bounded by payment (no valid payment, no execution) plus a safety ceiling.
- `429 rate_limited` with `Retry-After`.

## 5.9 OpenAPI

The canonical machine spec lives at `deus/api/openapi.yaml` (generated/maintained
alongside this doc). The agent-facing subset (discover/quote/invoke) is also
published as MCP tool schemas for `deus.mjs` ([`10-integration.md`](./10-integration.md)).
