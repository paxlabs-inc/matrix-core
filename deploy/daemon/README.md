# Matrix daemon — container image + Fly app

This directory holds the **deployment surface** for the per-user Matrix
daemon (`mcl-execute daemon`). It is consumed by:

- `docker build -f deploy/daemon/Dockerfile .` (run from the repo root,
  i.e. `/root/matrix`) for local smoke tests.
- The matrix-router on the storage box, which renders
  `fly.toml.tmpl` per user and provisions Machines via the Fly API.

## Files

| File | Purpose |
|---|---|
| `Dockerfile` | Multi-stage build: `golang:1.22-bookworm` builder → `debian:bookworm-slim` runtime with Node + Python+uv + pre-cached MCP servers + `mc` (MinIO client). |
| `entrypoint.sh` | Sets up `/data` layout, symlinks `/workspace`, optional snapshot pull from MinIO, exec daemon. |
| `.dockerignore` | Tight build context — only the four Go modules, `skills/`, `agents/`, this directory. |
| `fly.toml.tmpl` | Per-user Machine config template (rendered by router). |

## Local smoke test

```bash
# From /root/matrix:
docker build -f deploy/daemon/Dockerfile -t matrix-daemon:dev .

# Run with a host-side cortex-root + Fireworks key:
mkdir -p /tmp/dtest
docker run --rm \
  -p 8080:8080 \
  -v /tmp/dtest:/data \
  -e MATRIX_DEFAULT_SKILL="matrix://skill/writing-plans@1" \
  -e MATRIX_DAEMON_TOKEN="$(openssl rand -hex 16)" \
  -e FIREWORKS_API_KEY="$FIREWORKS_API_KEY" \
  matrix-daemon:dev

# In another shell:
curl -s http://127.0.0.1:8080/healthz | jq .
```

## Fly app bootstrap (one-time per app)

```bash
# 1. Authenticate.
fly auth login

# 2. Create the app (this is the daemon "fleet" — one app, N Machines).
fly apps create matrix-daemon --org personal

# 3. Push the image to Fly's registry.
fly deploy \
  --config deploy/daemon/fly.toml.tmpl \
  --no-deploy \
  --build-only \
  --image-label "v1-$(git rev-parse --short HEAD)"
```

After this point Machines are created **per-user** by the router. No
`fly deploy` is run again until the image SHA changes.

## Per-user Machine creation (sketch)

The router does the equivalent of:

```bash
fly machines run \
  --app matrix-daemon \
  --region iad \
  --image registry.fly.io/matrix-daemon:<sha> \
  --volume mxdata_<uid>:/data:initial_size=10gb \
  --env MATRIX_USER_ID=<uid> \
  --env MATRIX_S3_ENDPOINT=https://box.matrix.wg:9000 \
  --env MATRIX_S3_BUCKET=matrix-state \
  --secret MATRIX_DAEMON_TOKEN=<random> \
  --secret MATRIX_S3_KEY=<scoped> \
  --secret MATRIX_S3_SECRET=<scoped> \
  --secret FIREWORKS_API_KEY=<shared> \
  --secret TOGETHER_API_KEY=<shared> \
  --auto-stop suspend \
  --vm-size shared-cpu-1x \
  --vm-memory 1024
```

The Machine's ID is recorded in Postgres (`users.fly_machine_id`).
