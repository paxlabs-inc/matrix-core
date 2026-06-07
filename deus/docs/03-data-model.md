# 03 — Data Model

Three stores, each with a clear authority boundary:

- **Chain** (`ServiceRegistry` + precompiles) — **authoritative** for listing
  existence, ownership, pricing commitment hash, settlement, quality.
- **Postgres + pgvector** — **derived mirror** + operational state (ledger,
  embeddings, sessions, analytics). Rebuildable from chain + object store.
- **Object store (S3/MinIO)** — large blobs: artifacts, request/response bodies,
  signed receipts, logs. Referenced by hash from Postgres.

> Authority rule: anything a developer or caller could dispute money over must
> trace to the chain or to a signed, hash-anchored object. Postgres is for speed.

## 3.1 Entity overview

```text
developer 1───* service 1───* endpoint
                 │             *
                 ├───* pricing_plan
                 ├───1 manifest (jsonb) ──1 embedding (vector)
                 ├───* quality_sample ──► quality_score (on-chain PoFQ)
                 └───* invocation (ledger) ──* receipt
caller 1───* invocation
caller 1───* spend_grant (mirror of wallet policy)
service 1───* deployment (hosted only) ──1 fly_machine
```

## 3.2 Postgres schema (DDL direction)

Use `migrations/NNN_name.sql` (sequential, idempotent, forward-only), applied at
control-plane boot like `paxeer-embeded-wallets` does. `solc`/chain types stored
as `text` for big integers (PAX wei) — never float.

### `developers`
| column | type | notes |
| ------ | ---- | ----- |
| `id` | `uuid pk` | |
| `wallet_address` | `text unique not null` | EVM address; identity |
| `payout_address` | `text not null` | where earnings land (defaults to wallet) |
| `supabase_user_id` | `text null` | optional console login link |
| `display_name` | `text` | |
| `created_at` | `timestamptz default now()` | |

### `services` (the one product — registry == marketplace)
| column | type | notes |
| ------ | ---- | ----- |
| `id` | `uuid pk` | internal id |
| `chain_id` | `bigint not null` | on-chain `serviceId` from `ServiceRegistry` |
| `developer_id` | `uuid fk` | owner |
| `slug` | `text unique` | human/agent-friendly handle, e.g. `weather.now` |
| `kind` | `text check (kind in ('data','agent'))` | data API vs agent service |
| `mode` | `text check (mode in ('proxy','hosted'))` | hosting model |
| `display_name` | `text not null` | |
| `summary` | `text not null` | one-line, used in search |
| `manifest` | `jsonb not null` | full machine-readable manifest (see 3.5) |
| `manifest_hash` | `text not null` | keccak256 of canonical manifest; matches chain |
| `status` | `text` | `draft\|active\|paused\|delisted` |
| `confidential` | `bool default false` | TEE-backed |
| `quality_score` | `numeric` | cached PoFQ score (0..1e18), mirror of chain |
| `uptime_bps` | `int` | rolling uptime in basis points |
| `created_at`/`updated_at` | `timestamptz` | |

Indexes: `gin (manifest jsonb_path_ops)`, `btree (kind, mode, status)`,
`btree (quality_score desc)`.

### `endpoints`
| column | type | notes |
| ------ | ---- | ----- |
| `id` | `uuid pk` | |
| `service_id` | `uuid fk` | |
| `operation` | `text` | e.g. `forecast`, `geocode` — a callable op |
| `method` | `text` | `POST` default for invoke |
| `input_schema` | `jsonb` | JSON Schema for args |
| `output_schema` | `jsonb` | JSON Schema for result |
| `proxy_url` | `text null` | proxy mode target |
| `runner_ref` | `text null` | hosted mode internal URL / machine id |

### `pricing_plans`
| column | type | notes |
| ------ | ---- | ----- |
| `id` | `uuid pk` | |
| `service_id` | `uuid fk` | |
| `model` | `text` | `per_call \| per_unit \| per_second(stream)` |
| `unit` | `text` | `call \| token \| request \| second` |
| `price_wei` | `text` | PAX wei per unit (string big-int) |
| `min_charge_wei` | `text` | floor per invocation |
| `currency` | `text default 'PAX'` | only PAX in v1 |
| `version` | `int` | bump on change; quotes pin a version |

### `embeddings` (pgvector)
| column | type | notes |
| ------ | ---- | ----- |
| `service_id` | `uuid pk fk` | one row per service |
| `model` | `text` | embedder id |
| `vec` | `vector(N)` | manifest+summary embedding |
HNSW index: `using hnsw (vec vector_cosine_ops)`.

### `invocations` (the metering ledger — append-only)
| column | type | notes |
| ------ | ---- | ----- |
| `id` | `uuid pk` | |
| `idempotency_key` | `text unique` | client-supplied; dedups retries |
| `service_id` | `uuid fk` | |
| `endpoint_id` | `uuid fk` | |
| `caller_did` | `text not null` | agent/human DID or wallet |
| `caller_wallet` | `text` | EVM address charged |
| `quote_id` | `uuid fk` | the price quote honored |
| `units` | `text` | metered units (string big-int) |
| `price_wei` | `text` | total charge |
| `pricing_version` | `int` | which plan version priced it |
| `args_hash` | `text` | keccak256 of canonical args |
| `result_hash` | `text` | keccak256 of canonical result |
| `outcome` | `text` | `ok \| error \| timeout \| denied` |
| `latency_ms` | `int` | |
| `settlement_id` | `uuid null fk` | set when settled |
| `created_at` | `timestamptz` | |
Indexes: `(service_id, created_at)`, `(settlement_id)`, `(caller_did, created_at)`.

### `quotes`
Short-lived signed price promises. `id`, `service_id`, `endpoint_id`,
`pricing_version`, `unit_price_wei`, `max_units`, `expires_at`, `signature`
(gateway EIP-712), `caller_did`.

### `receipts`
| column | type | notes |
| ------ | ---- | ----- |
| `invocation_id` | `uuid pk fk` | |
| `eip712_digest` | `text` | the signed digest |
| `gateway_sig` | `text` | gateway signature |
| `runner_sig` | `text null` | runner co-sign (hosted/confidential) |
| `attestation` | `jsonb null` | TEE attestation for confidential calls |
| `blob_ref` | `text` | object-store key of full receipt JSON |
| `anchored_tx` | `text null` | tx hash when the batch root was anchored |

### `settlements`
| column | type | notes |
| ------ | ---- | ----- |
| `id` | `uuid pk` | |
| `developer_id` | `uuid fk` | payee |
| `rail` | `text` | `net \| stream \| direct` |
| `total_wei` | `text` | sum paid |
| `invocation_count` | `int` | entries covered |
| `merkle_root` | `text` | root of covered receipts |
| `tx_hash` | `text` | on-chain settlement tx |
| `window_start`/`window_end` | `timestamptz` | |
| `status` | `text` | `pending \| confirmed \| failed` |

### `spend_grants` (mirror of wallet policy — read-only cache)
`caller_did`, `service_id null` (null = any), `max_per_call_wei`,
`max_total_wei`, `spent_wei`, `expires_at`, `source` (`argus|wallet_policy`).
Authoritative copy lives in the embedded wallet; this is a cache the gateway
checks fast, then confirms against the wallet on the spend path.

### `deployments` (hosted only)
`id`, `service_id`, `fly_app`, `fly_machine_id`, `image_ref`, `status`,
`region`, `last_invoked_at`, `min_machines`, `created_at`.

### `index_cursor`
Single-row indexer bookmark: `last_block`, `last_log_index`, `updated_at`.

## 3.3 On-chain state (authoritative — see [`04-onchain.md`](./04-onchain.md))

`ServiceRegistry` stores, per service: `owner`, `payoutAddress`, `manifestHash`,
`pricingCommitmentHash`, `status`, `hosted`, `confidential`, and emits
`ServiceRegistered/Updated/Delisted`. Quality lives via PoFQ rolling score keyed
by service. Settlement amounts and receipt-batch roots are recorded by the
settlement path.

## 3.4 Object store layout

```text
deus/
  artifacts/<service_id>/<version>/...        uploaded code/containers (hosted)
  receipts/<yyyy>/<mm>/<dd>/<invocation_id>.json
  bodies/<invocation_id>/{request,response}.bin    (only when > inline threshold)
  logs/<service_id>/<deployment_id>/...
```
Everything keyed/looked-up by hash; Postgres holds the references.

## 3.5 Service manifest schema (the heart of the model)

A single JSON document, JSON-Schema-validated, embedded for search, hashed for
the chain. Direction for `pkg/manifest`:

```jsonc
{
  "schema_version": "1",
  "slug": "weather.now",
  "kind": "data",                         // data | agent
  "display_name": "Weather Now",
  "summary": "Current conditions and short-range forecast by lat/lng.",
  "description": "Longer markdown description used in console + search.",
  "tags": ["weather", "geo", "forecast"],
  "owner": "0xDeveloperWallet",
  "payout_address": "0xPayout",
  "mode": "proxy",                        // proxy | hosted
  "confidential": false,
  "operations": [
    {
      "name": "forecast",
      "method": "POST",
      "input_schema": { "type": "object", "properties": { "lat": {"type":"number"}, "lng": {"type":"number"} }, "required": ["lat","lng"] },
      "output_schema": { "type": "object" },
      "timeout_ms": 5000,
      "max_response_bytes": 262144
    }
  ],
  "pricing": [
    { "operation": "forecast", "model": "per_call", "unit": "call", "price_wei": "200000000000000", "min_charge_wei": "200000000000000" }
  ],
  "endpoint": { "proxy_url": "https://api.dev.example/forecast" },   // proxy mode
  "sla": { "target_uptime_bps": 9900, "p99_latency_ms": 800 },
  "attestation": null                     // or { "family": "intel_tdx", "expected_report_data": "0x..." }
}
```

Rules:
- `manifest_hash = keccak256(canonical_json(manifest))`; canonicalization is a
  pure, versioned function shared by Go (`pkg/manifest`) and the validator.
- Pricing in the manifest is a **commitment**; `pricing_commitment_hash` is
  registered on-chain so a developer cannot silently overcharge vs the listing.
- `attestation != null` ⇒ `confidential = true` and routes only to TEE runners.

## 3.6 Rebuild / replay invariant

Postgres mirror must be **fully rebuildable** by: (1) replaying `ServiceRegistry`
events from genesis into `services/endpoints/pricing_plans`, (2) recomputing
embeddings from `manifest`, (3) re-deriving settlements from the metering ledger
+ on-chain settlement txns. The metering ledger itself is the only
Postgres-origin durable truth (each row is signed into a receipt and anchored),
so it is backed up + anchored, not derivable. CI should include a
"mirror-rebuild" check on a fixture chain.
