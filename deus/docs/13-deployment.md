# 13 — Deployment & Operations

Deus follows the proven Matrix deploy pattern: a shared private control plane on
Fly, data on the Paxeer box, scale-to-zero runners, and a daemon-baked MCP proxy.
Mirror `deploy/browser` and `deploy/tachyon`.

## 13.1 Fly apps

| App | Public IP? | Shape | Notes |
| --- | ---------- | ----- | ----- |
| `deus-control` | Yes (TLS) | N instances, stateless | the `deusd` Go server; public API |
| `deus-runner` | No (6PN) | scale-to-zero shared pool | node20 function tenants |
| `deus-svc-<id>` | No (6PN) | scale-to-zero per service | dedicated/confidential services |

> If the public API should be box-fronted (nginx) like the gateway/router rather
> than a public Fly IP, run `deus-control` private (6PN) and add an nginx
> `/deus/` location on the box (mirror `deploy/box/nginx/...` `/gw/`). Pick one;
> the spec assumes a public `deus-control` for simplicity.

## 13.2 `deploy/deus/` files

```text
deploy/deus/
  Dockerfile          multi-stage: golang:1.22-bookworm build deusd -> debian-slim
  fly.toml            deus-control app (public service :PORT, TLS, N machines)
  deploy.sh           org-capable flyctl deploy (unset FLY_API_TOKEN; flyctl auth login)
  install.sh          box install: binary + systemd unit + env + migrations
  runner/
    Dockerfile        node20 runner base image (harness + runtimes baked)
    fly.toml          deus-runner shared pool (private, scale-to-zero)
  README.md           deploy runbook (mirror deploy/tachyon/README.md)
```

- **Dockerfile (control):** build `cmd/deusd`, copy `migrations/`, `configs/`,
  `pkg/manifest/schema.json`. `CMD ["deusd"]`.
- **Dockerfile (runner):** Node 20, bake `runner/` harness + runtimes; entry is
  the private HTTP server; egress allowlist applied.
- **deploy.sh:** org-level flyctl creds (the box `FLY_API_TOKEN` is app-scoped to
  `matrix-daemon` and cannot create new apps — use ambient `flyctl auth login`,
  like the Tachyon/Browser READMEs). Builds, pushes, deploys.
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
DEUS_FLY_API_TOKEN=...                      # runner orchestration (scoped)
DEUS_PORT=9095
```
Optional: worker counts, settlement window seconds, fleet cap, feature flags
(`DEUS_CONFIDENTIAL_ENABLED`, `DEUS_HOSTED_ENABLED`).

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

1. Deploy contracts (registry + anchor) to chain 125.
2. Stand up Postgres `deus` DB + MinIO bucket; `deusctl migrate`.
3. Deploy `deus-control` (proxy listings + discovery + invoke + net settlement).
4. Deploy `deus-runner` pool; enable hosted listings.
5. Bake + ship `tools/deus` in the daemon image; wire router env.
6. Deploy the console.
7. Enable streaming, then confidential (TEE), then scheduler-recurring (v1.x).

## 13.8 Observability & ops

- `GET /internal/healthz` (DB + chain RPC + objstore reachability),
  `GET /internal/metrics` (Prometheus).
- Dashboards: invoke latency/throughput, charge totals, denial rate, settlement
  lag, indexer lag, runner cold-starts, attestation pass/fail, escrow-vs-ledger
  drift.
- Alerts: settlement lag > window, indexer lag > N blocks, escrow drift > 0,
  denial spike, runner error rate.
- Runbooks in `deploy/deus/README.md`: redeploy control, redeploy runner,
  replay indexer (`deusctl index replay --from`), force settle, rotate signing
  keys, pause a service.

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
