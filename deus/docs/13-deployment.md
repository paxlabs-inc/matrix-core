Deus is ready to deploy, but the checked-in runbook has several mismatches that would block or weaken a production launch. I did not perform any chain or production writes.

Preflight is green:

- `go build ./...`
- MCP self-test: 6 tools, manifest matches
- Foundry: 6/6 contract tests pass

The repo-supported deployment target is the Paxeer/Supabase box using Docker Compose—not this dev box. This machine currently has no `/opt/deus/deus.env`, `supabase-db`, or `supabase_default` network.

### Fix or work around these blockers first

1. The Compose build context is wrong. It uses `../../deus`, while the Dockerfile requires the repository root to copy both `deus/` and `layerx/`: [docker-compose.yml](/root/matrix/deploy/deus/docker-compose.yml:35), [Dockerfile](/root/matrix/deploy/deus/Dockerfile:3).

   Until patched, build manually from `/root/matrix`:

   ```bash
   docker build -f deploy/deus/Dockerfile -t deus-control:latest .
   docker compose -f deploy/deus/docker-compose.yml up -d --no-build
   ```

2. Verify the MinIO health check on the target before starting `deus-control`:

   ```bash
   docker compose -f deploy/deus/docker-compose.yml up -d deus-minio
   docker inspect deus-minio --format '{{json .State.Health}}'
   ```

   Compose currently expects `mc ready local` inside the MinIO image: [docker-compose.yml](/root/matrix/deploy/deus/docker-compose.yml:28). If it goes unhealthy, that check needs correcting before `deus-control` can start.

3. Use `DEUS_GATEWAY_SIGNING_KEY`, not only `DEUS_GATEWAY_SIGNING_KEY_REF`. Configuration accepts either, but the running server currently constructs the gateway only from the actual key: [main.go](/root/matrix/deus/cmd/deusd/main.go:150).

4. The current deployment script deploys only `ServiceRegistry`, not `SettlementAnchor`: [Deploy.s.sol](/root/matrix/deus/contracts/script/Deploy.s.sol:8). The README’s instruction to record both is stale.

### Deployment order

On the actual Supabase/Paxeer box:

1. Confirm prerequisites:

   ```bash
   docker inspect supabase-db
   docker network inspect supabase_default
   ```

2. Create the `deus` Postgres role/database and enable `vector` and `pgcrypto`, following [the box runbook](/root/matrix/deploy/deus/README.md:17).

3. Create `/opt/deus/deus.env` with mode `0600`, using [deus.env.example](/root/matrix/deploy/deus/deus.env.example:1). Production needs real values for:

   - `DEUS_POSTGRES_URI`
   - `PAXEER_RPC_URL`
   - `DEUS_SERVICE_REGISTRY_ADDR`
   - `DEUS_LAYERX_URL`
   - `DEUS_LXP_KEY`
   - all four `DEUS_OBJSTORE_*` variables
   - `DEUS_GATEWAY_SIGNING_KEY`
   - `DEUS_DEVELOPER_AUTH_SECRET`
   - `DEUS_SIWE_DOMAIN`

   Leave `DEUS_APPWRITE_*` unset for the initial LXP MVP; hosted Paxeer Cloud listings can follow later.

4. Deploy `ServiceRegistry`. This is an on-chain write and requires your explicit approval before running the broadcast:

   ```bash
   cd /root/matrix/deus/contracts
   forge script script/Deploy.s.sol:Deploy \
     --rpc-url "$PAXEER_RPC_URL" \
     --broadcast
   ```

   Use your normal secure Foundry signer and set `DEUS_REGISTRY_GOVERNOR` to the real governor address. Put the resulting registry address in `/opt/deus/deus.env`.

5. Build and start:

   ```bash
   cd /root/matrix
   docker build -f deploy/deus/Dockerfile -t deus-control:latest .
   docker compose -f deploy/deus/docker-compose.yml up -d --no-build
   docker logs -f deus-control
   ```

6. Verify locally:

   ```bash
   docker exec deus-control curl -fsS \
     http://localhost:9095/internal/healthz
   ```

7. Add the external Caddy route:

   ```text
   deus.paxeer.app -> deus-control:9095
   ```

   That Caddy configuration is not present in this repository, despite the runbook claiming it exists.

8. Set this on the Matrix router service and redeploy it:

   ```text
   MATRIX_DEUS_URL=https://deus.paxeer.app
   ```

   Do not use `deus-control.internal` for Railway user machines; the Docker container hostname is only visible inside `supabase_default`. The router already forwards `MATRIX_DEUS_URL`: [main.go](/root/matrix/router/cmd/matrix-router/main.go:247).

### Payment rollout

Leave automatic payment disabled initially. Without `LAYERX_MAX_SPEND_USDX`, Deus can discover and quote, but the bridge refuses to sign charges by design: [deus.mjs](/root/matrix/tools/deus/deus.mjs:218).

For a real paid canary, set these on one per-user daemon service:

```text
LAYERX_MAX_SPEND_USDX=<owner-approved per-call amount>
LAYERX_MAX_DAILY_USDX=<owner-approved rolling-day amount>
```

The router does not currently propagate those variables, so fleet-wide automatic payments need either a small router change or direct per-user service configuration.

Then verify through a provisioned daemon:

1. `/diag/mcp` reports `deus` with six tools.
2. `deus_discover` reaches production.
3. `deus_quote` returns signed USDX terms.
4. A real priced `deus_invoke` settles once on LayerX and returns `layerx_receipt.seq`.
5. Replaying the same idempotency key returns the stored result without another charge.

The canonical architecture and rollout sequence are in [13-deployment.md](/root/matrix/deus/docs/13-deployment.md:95), but the corrections above reflect the actual current code.