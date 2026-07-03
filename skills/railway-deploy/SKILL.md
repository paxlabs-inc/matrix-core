---
name: railway-deploy
description: Deploy an app to Railway — build/start command detection, service config, environment variables, private networking, databases, and health checks. Use when deploying any web app or service to Railway.
origin: Matrix
---

# Railway Deploy Playbook

Deploy a web app or service to Railway. Railway builds from a repo (Nixpacks or a
Dockerfile), runs a detected or declared start command, and wires services together over a
private network. This playbook covers the config that matters and the mistakes that cost a
redeploy.

Use when: shipping any of the stack-selection frameworks (React Router, Astro SSR,
SvelteKit, Angular static, a Go/Python service) to Railway.

## Concepts

- **Project** contains **services**; a service is one deployable (your app, a database, a
  worker). Each has its own env vars, domain, and networking.
- **Build**: Nixpacks auto-detects the stack, or Railway uses your `Dockerfile` if present.
- **Start command**: detected from the framework, or set explicitly (do this — see below).

## Start command detection and override

Railway/Nixpacks infers the start from the ecosystem, but inference is where deploys go
wrong. Be explicit per stack:

| Stack | Build | Start |
|---|---|---|
| React Router (framework/SSR) | `npm run build` | `npm start` (`react-router-serve ./build/server/index.js`) |
| Astro (static) | `npm run build` | serve `dist/` statically (or add `@astrojs/node` for SSR) |
| Astro (SSR, node adapter) | `npm run build` | `node ./dist/server/entry.mjs` |
| SvelteKit (adapter-node) | `npm run build` | `node build` |
| Angular (static) | `ng build` | serve `dist/<app>/browser` statically |
| Go service | `go build -o app` | `./app` |
| Python (uv) | `uv sync` | `uv run <entrypoint>` (e.g. `uv run uvicorn app:app --host 0.0.0.0 --port $PORT`) |

Bind to `0.0.0.0` and read `$PORT` from the environment — Railway injects `PORT`. Hardcoding
a port or binding to `127.0.0.1` makes the service unreachable and the deploy "healthy but
dead."

## Declaring config in-repo

Prefer committed config over dashboard clicks. `railway.json` (or `railway.toml`):

```json
{
  "$schema": "https://railway.app/railway.schema.json",
  "build": { "builder": "NIXPACKS" },
  "deploy": {
    "startCommand": "npm start",
    "healthcheckPath": "/healthz",
    "healthcheckTimeout": 100,
    "restartPolicyType": "ON_FAILURE"
  }
}
```

For full control of the build, commit a `Dockerfile` and Railway uses it instead of
Nixpacks.

## Environment variables and references

- Set vars per service in the dashboard or with `railway variables`. Never commit secrets.
- **Reference variables** wire services without copy-paste:
  `DATABASE_URL=${{ Postgres.DATABASE_URL }}` pulls the value from the Postgres service.
- Railway provides `PORT`, `RAILWAY_*`, and service domains at runtime.

## Private networking

- Services in a project reach each other over an internal IPv6 network at
  `<service>.railway.internal` — no public exposure, no egress cost.
- App → database traffic must use the **private** host (the `*.railway.internal` /
  reference-variable URL), not the public proxy URL. Using the public URL leaks over the
  internet and is slower. Bind listeners to `::` / `0.0.0.0` so they accept the internal
  interface.
- Expose only what needs a public domain (the web frontend). Databases and internal workers
  stay private.

## Databases

- Add Postgres/MySQL/Redis as services from the dashboard; consume via reference variables.
- Run migrations on deploy: a release/pre-deploy step, or a start command that runs
  migrations then boots (`migrate && start`). Do not migrate lazily on first request.

## Health checks

Expose a cheap `/healthz` that returns 200 only when the app can serve (DB reachable, etc.),
and set `healthcheckPath`. Railway holds traffic until it passes and uses it to gate
restarts.

## CLI workflow

```bash
npm i -g @railway/cli
railway login
railway link                 # attach repo to a project/service
railway up                   # build + deploy current dir
railway logs                 # tail deploy/runtime logs
railway variables            # inspect env
```

## Common pitfalls

- Binding to `127.0.0.1` or a hardcoded port instead of `0.0.0.0:$PORT` — service unreachable.
- App talking to the DB over the **public** URL instead of `*.railway.internal` — slow,
  exposed, and sometimes billed egress.
- Relying on start-command auto-detection for a static SPA — set it explicitly.
- No health check — Railway routes traffic to a not-yet-ready process.
- Committing secrets or a `.env` instead of using service variables/references.
- Skipping a migration step and hitting a schema mismatch at first request.
