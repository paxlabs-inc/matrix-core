# Cody Client — deploy & live-test runbook

Everything for spec/cody-client is code-complete and green (cody + router build/vet/test;
client tsc clean). Deploy is git-push-driven via `.github/workflows/docker.yml`. This is the
copy-paste path to get it live and smoke-tested.

## 0. What ships on push
Pushing to `main` rebuilds + pushes to GHCR (only changed images) and redeploys the control
plane:
- `matrix-daemon-railway` (deploy/railway/Dockerfile) — the per-user VM image incl. **codyd**
  with the preview lifecycle + sandbox client. New user provisions pick this up via
  `ROUTER_DAEMON_IMAGE`; existing user envs are rolled by the operator (staged), not CI.
- `matrix-router` (deploy/router/Dockerfile) — the front door incl. `/preview/{user}` proxy,
  CORS, and the JWT header/query/cookie acceptance. Auto-redeployed by the `redeploy` job.

## 1. Router prod env (set BEFORE/with the push)
On the **matrix-router** Railway service (`/etc/centra/router.env` or Railway vars). These flow
to every per-user codyd automatically via the router's MachineEnv injection.

    # Preview: Railway sandbox provisioning (same project/env as user VMs)
    RAILWAY_API_TOKEN=<railway project API token>
    RAILWAY_PROJECT_ID=<the one project id>
    RAILWAY_ENVIRONMENT_ID=<the one environment id>
    # Preview: router internal door codyd registers its sandbox target at
    ROUTER_PREVIEW_TOKEN=<shared secret; enables /internal/preview/*>
    ROUTER_INTERNAL_URL=http://matrix-router.railway.internal:8088   # default; override if addr differs
    # Cross-origin: lock CORS to the frontend origin (empty/unset = allow any; Bearer-auth so safe)
    ROUTER_CORS_ORIGINS=https://<your-frontend-origin>
    # Optional: preview sandbox base image (default node:22-bookworm)
    # CODY_PREVIEW_IMAGE=node:22-bookworm

Also confirm the router already verifies the SAME Supabase issuer the frontend uses:
`SUPABASE_URL=https://supabase.paxeer.app` (JWKS) and/or the legacy `SUPABASE_JWT_SECRET`.

## 2. Frontend prod env
Wherever apps/client is hosted (Vercel/etc.) — `.env` is only the local default:

    NEXT_PUBLIC_MATRIX_ROUTER_URL=https://api.paxlabs.app
    NEXT_PUBLIC_SUPABASE_URL=https://supabase.paxeer.app
    NEXT_PUBLIC_SUPABASE_ANON_KEY=<anon/publishable key>

If either Supabase var is missing the client sends NO Bearer token and every api.paxlabs.app
call 401s (authEnabled gate in lib/env.ts).

## 3. Deploy
    git add -A && git commit -m "cody-client: preview lifecycle + client wiring + cross-origin"
    git push origin main
Watch the `docker` workflow: `build (matrix-daemon-railway)` + `build (matrix-router)` must be
green, then the `railway redeploy` job triggers the router.

## 4. Live smoke test (after deploy)
    # 4a. Router reachable + CORS preflight answered (no token, must be 204 with ACAO)
    curl -i -X OPTIONS https://api.paxlabs.app/chat \
      -H "Origin: https://<your-frontend-origin>" \
      -H "Access-Control-Request-Method: POST" \
      -H "Access-Control-Request-Headers: authorization,content-type"
    #   expect: 204, Access-Control-Allow-Origin: <origin>, Allow-Headers incl authorization

    # 4b. In the app: open /cody → create a Prototype project → "build a simple vite react page"
    #     - watch the waved plan + turn-in cards stream (SSE cross-origin over Bearer)
    #     - on plan.completed, the Preview tab (hero in Prototype) shows "Building preview…"
    #       then the iframe renders the running app (api.paxlabs.app/preview/{you}/?access_token=…)
    # 4c. Engineer project greenfield → SDR decision card must BLOCK wave 1 until you approve.
    # 4d. Undo/checkpoint round-trips; restore refused (409) while the run is live.

## 5. If preview doesn't render
- 401 on the iframe → Supabase token missing/expired, or router `SUPABASE_URL` issuer mismatch.
- iframe blocked by CSP → confirm the served CSP has `frame-src` incl. api.paxlabs.app
  (lib/security/csp.ts buildFrameSrc; CSP is applied per-request in proxy.ts).
- "no preview yet" forever → codyd couldn't provision: check RAILWAY_* creds + ROUTER_PREVIEW_TOKEN
  in the user VM env, and that DetectStart found a dev/start script (a Node app needs package.json).
- cross-user 403 on /preview → working as intended (path user must equal JWT subject).

## Deferred (per user, after the presentation)
- 7.3 preview+screenshot property test, 7.4 client vitest (reducer equivalence + tier rendering),
  task 8 final checkpoint. spec.kvx statuses for these remain `pending`.
