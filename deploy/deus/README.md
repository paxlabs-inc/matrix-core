# deploy/deus — Deus control plane (box deploy)

Runs `deusd` as a Docker service co-located with the Supabase stack on the
Paxeer box, joined to the `supabase_default` network. Postgres is the box's
`supabase-db`; object storage is a dedicated `deus-minio` sidecar (Supabase
Storage's S3 API is path-prefixed and incompatible with the minio-go client);
the chain is Paxeer mainnet (125). The hosted-execution tier (Phase 3) targets
Paxeer Cloud and is disabled until `DEUS_APPWRITE_*` is set.

Files:

- `Dockerfile` — multi-stage build of `deusd` + `deusctl` (context = `deus/`).
- `docker-compose.yml` — `deus-control` + `deus-minio` services on the external `supabase_default` net.
- `deus.env.example` — env template. Real values: `/opt/deus/deus.env` (`chmod 600`).
- `runner/node20/` — Paxeer Cloud function template (uploaded by `internal/hosting`, not a Fly app).

## First deploy

```bash
# 1. Database (as postgres superuser inside supabase-db)
docker exec -i supabase-db psql -U postgres <<'SQL'
CREATE ROLE deus LOGIN PASSWORD '<pw>';
CREATE DATABASE deus OWNER deus;
\c deus
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
SQL

# 2. Env
install -m 600 /dev/null /opt/deus/deus.env   # then fill from deus.env.example

# 3. Contracts (chain 125) — see deus/docs/13-deployment.md §13.5
cd deus/contracts && forge build && forge test
forge script script/Deploy.s.sol --rpc-url "$PAXEER_RPC_URL" --broadcast
# record ServiceRegistry / SettlementAnchor addresses into /opt/deus/deus.env

# 4. Build + run (deusd auto-migrates the deus DB at boot)
docker compose -f deploy/deus/docker-compose.yml up -d --build

# 5. Verify
docker exec deus-control curl -fsS http://localhost:9095/internal/healthz
```

## Ops

- Logs: `docker logs -f deus-control`
- Restart: `docker compose -f deploy/deus/docker-compose.yml restart`
- Rebuild after code change: `... up -d --build`
- Migrations are forward-only and run automatically at boot from `/app/migrations`.
- Rotate keys: edit `/opt/deus/deus.env`, then restart.
- Caddy fronts it at `deus.paxeer.app -> deus-control:9095` (supabase Caddyfile).

## Invariants

- No secret values in the repo, cortex, or logs — only in `/opt/deus/deus.env`.
- On-chain / destructive ops require explicit operator approval.
- No git commit/push on this box (user drives commits).
