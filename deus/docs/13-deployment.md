# 13 — Deployment & Operations

Deus follows the proven Matrix deploy pattern for the **control plane** (a shared
private Go service + data on the Paxeer box + a daemon-baked MCP proxy), but
**hosted-service execution runs on Paxeer Cloud** (the deployed Appwrite fork),
not on bespoke Fly runners. Mirror `deploy/browser` and `deploy/tachyon` for the
control-plane shape only.

## 13.1 Control-plane placement (`deus-control`)

The `deusd` Go server is stateless and can run wherever is convenient; pick one:

| Option | Public? | Shape | Notes |
| ------ | ------- | ----- | ----- |
| Fly app `deus-control` | Yes (TLS) or 6PN | N instances | mirrors `deploy/tachyon`; public IP or box-fronted |
| Paxeer box (systemd) | via nginx | N instances | `deusd` next to gateway/router; nginx `/deus/` location |
| Paxeer Cloud container | via Appwrite ingress | N instances | co-located with hosted execution |

> If box-fronted (like the gateway/router), run `deus-control` private and add an
> nginx `/deus/` location (mirror `deploy/box/nginx/...` `/gw/`). The hosted
> **execution tier is always Paxeer Cloud** regardless of where the control plane
> runs.

## 13.1b Hosted execution (Paxeer Cloud / Appwrite fork)

- Hosted listings are **Paxeer Cloud Functions** (node20 source) or **container
  Sites** (heavier/confidential), created via the **Appwrite Server API** by
  `internal/hosting`. Appwrite owns build, scale-to-zero, routing, secrets
  (function variables), and logs.
- No `deus-runner` / `deus-svc-<id>` Fly apps. The free-hosting **budget +
  kill-switch** ([`06-execution-hosting.md`](./06-execution-hosting.md) §6.7) is
  enforced by the orchestrator against Paxeer Cloud consumption.
- Confidential services use the Paxeer Cloud TEE runtime where available, else a
  dedicated runner.

## 13.2 `deploy/deus/` files

```text
deploy/deus/
  Dockerfile          multi-stage: golang:1.22-bookworm build deusd -> debian-slim
  fly.toml            deus-control app (service :PORT, TLS, N machines) [if Fly]
  deploy.sh           org-capable flyctl deploy (unset FLY_API_TOKEN; flyctl auth login)
  install.sh          box install: binary + systemd unit + env + migrations
  runner/             Paxeer Cloud function/Site templates (NOT a Fly app)
    node20/           Appwrite node20 function template (harness + handler shim)
    container/        container Site template (Dockerfile + entry shim)
    README.md         how internal/hosting packages + deploys these via Appwrite API
  README.md           deploy runbook (mirror deploy/tachyon/README.md)
```

- **Dockerfile (control):** build `cmd/deusd`, copy `migrations/`, `configs/`,
  `pkg/manifest/schema.json`. `CMD ["deusd"]`.
- **runner/ templates:** the harness + runtime shims that `internal/hosting`
  uploads to **Paxeer Cloud** as Functions/Sites — there is **no runner Fly app
  and no machine image to push**. Egress allowlist + caps are applied on the
  function spec + enforced by the harness.
- **deploy.sh (control only):** org-level flyctl creds (the box `FLY_API_TOKEN`
  is app-scoped to `matrix-daemon` and cannot create new apps — use ambient
  `flyctl auth login`). Builds, pushes, deploys `deus-control` if on Fly.
- **install.sh (box):** install `deusd` binary, write systemd unit, write
  `/etc/matrix/deus.env`, run `deusctl migrate`. Idempotent (mirror
  `gateway/deploy/install.sh`).

## 13.3 Environment (`/etc/matrix/deus.env`)

Required (values live in box env / secret store — never in repo or cortex):
```
DEUS_POSTGRES_URI=postgres://...           # box Postgres, db=deus
PAXEER_RPC_URL=https://...                 # chain 125 RPC
DEUS_SERVICE_REGISTRY_ADDR=0x...           # from Deploy.s.sol
DEUS_SETTLEMENT_ANCHOR_ADDR=0x...
DEUS_OBJSTORE_ENDPOINT=...                 # box MinIO/S3
DEUS_OBJSTORE_KEY=... DEUS_OBJSTORE_SECRET=...
DEUS_GATEWAY_SIGNING_KEY_REF=...           # receipts/quotes signer (secret ref)
DEUS_SETTLER_KEY_REF=...                   # escrow/settlement signer (secret ref)
MATRIX_WALLET_API_URL=https://connect.paxportwallet.com   # embedded wallet
DEUS_EMBED_PROVIDER=fireworks DEUS_EMBED_API_KEY=...
DEUS_APPWRITE_ENDPOINT=...                  # Paxeer Cloud (Appwrite) Server API
DEUS_APPWRITE_PROJECT=...                   # Paxeer Cloud project id
DEUS_APPWRITE_API_KEY=...                   # Appwrite server API key (secret ref)
DEUS_HOSTING_BUDGET_PAX=...                 # aggregate free-hosting ceiling
DEUS_PORT=9095
```
Optional: worker counts, settlement window seconds, feature flags
(`DEUS_CONFIDENTIAL_ENABLED`, `DEUS_HOSTED_ENABLED`, `DEUS_HOSTING_KILLSWITCH`).
`DEUS_FLY_*` only if `deus-control` itself runs on Fly.

## 13.4 Data tier (Paxeer box)

- **Postgres**: a `deus` database on the existing box Postgres (separate from
  the matrix gateway/router DB; or a separate schema). `create extension vector`.
- **MinIO/S3**: a `deus-*` bucket for artifacts/receipts/bodies/logs. Apply a
  lifecycle/retention policy (avoid the unbounded-versioning issue seen with
  matrix-state).

## 13.5 On-chain deploy

1. `forge build && forge test` in `deus/contracts`.
2. `forge script script/Deploy.s.sol --rpc-url $PAXEER_RPC_URL --broadcast`
   (or via Tachyon's deploy path using the agent wallet).
3. Record addresses in `configs/chain.<env>.json` + `/etc/matrix/deus.env`.
4. Register the Deus relayer/settler address in the `x/feemarket` agent fee lane
   (gov/op step) so registrations + settlements get the lane gas price.
5. Seed any initial gov params (none required for v1 registry).

## 13.6 MCP proxy + daemon

1. Bake `tools/deus` into the daemon image (`deploy/daemon/Dockerfile COPY
   tools/deus`).
2. Add `deus` to `agents/default.json` (bijection with `deus-tools.json`).
3. `router` `MachineEnv` injects `MATRIX_DEUS_URL` (+ token).
4. Rebuild + redeploy the daemon image; new per-user provisions pick it up.
5. Verify: `node tools/deus/deus.mjs --selftest`; on a provisioned Machine
   `GET /diag/mcp` shows `deus` running with the right tool count.

## 13.7 Rollout order

1. Deploy `ServiceRegistry` to chain 125 (Phase 1). (`SettlementAnchor` +
   channel/escrow contracts come with Phase 2.5.)
2. Stand up Postgres `deus` DB + MinIO bucket; `deusctl migrate`.
3. Deploy `deus-control` for the **direct-rail MVP** (proxy listings + discovery
   + invoke + direct-transfer pay + PoFQ). No escrow/net settlement yet.
4. Bake + ship `tools/deus` in the daemon image; wire router env.
5. Deploy the console.
6. **Fast-follow (Phase 2.5):** deploy `SettlementAnchor` + channel/escrow
   contracts; enable the payment channel + net settlement.
7. Enable hosted listings on **Paxeer Cloud** (Phase 3); wire the hosting budget.
8. Enable streaming, then confidential (TEE), then scheduler-recurring (v1.x).

## 13.8 Observability & ops

- `GET /internal/healthz` (DB + chain RPC + objstore reachability),
  `GET /internal/metrics` (Prometheus).
- Dashboards: invoke latency/throughput, charge totals, denial rate, settlement
  lag, indexer lag, runner cold-starts, attestation pass/fail, escrow-vs-ledger
  drift.
- Alerts: settlement lag > window, indexer lag > N blocks, escrow drift > 0,
  denial spike, runner error rate.
- Runbooks in `deploy/deus/README.md`: redeploy control, redeploy a hosted
  service on Paxeer Cloud, replay indexer (`deusctl index replay --from`), force
  settle, rotate signing keys, pause a service, trip/reset the hosting budget
  kill-switch.

## 13.9 Backups & DR

- Postgres: the metering ledger is the only Postgres-origin truth → continuous
  backup + the receipts are anchored on-chain (recoverable evidence).
- The registry mirror + embeddings are **rebuildable** from chain + manifests
  (`deusctl index replay` + re-embed) — DR for those is "rebuild," not "restore."
- Object store: receipts retained ≥ dispute window; bodies short-TTL.

## 13.10 Invariants for ops (do not violate)

- No git commit/push on the dev box (user drives commits).
- Destructive chain/data ops require explicit approval.
- Never store secret values in repo/cortex/logs — only references.
- Fly app creation needs org-level creds (ambient `flyctl auth login`), not the
  app-scoped box token.
