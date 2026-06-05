# matrix-browser — shared headless browser for the agent fleet

A single private Fly app running [`@playwright/mcp`](https://github.com/microsoft/playwright-mcp)
(pinned `0.0.75`) over its Streamable-HTTP transport. Every per-user daemon
reaches it over Fly's private 6PN network via the daemon-side stdio proxy
`tools/browser/browser.mjs`, which bridges the daemon's simple MCP HTTP
transport to Playwright-MCP's SSE + `Mcp-Session-Id` protocol.

```
daemon (per user)                         matrix-browser (shared)
┌─────────────────────────────┐           ┌──────────────────────────┐
│ executor ──stdio──> browser │           │ @playwright/mcp          │
│            tools/browser/   │  6PN/HTTP  │  --headless --isolated   │
│            browser.mjs ─────┼──────────> │  :8931/mcp  (Chromium)   │
└─────────────────────────────┘  Streamable└──────────────────────────┘
                                  HTTP (SSE)
```

## Why this shape
- **One shared browser, not Chromium-per-daemon** — keeps the per-user image
  light; the browser runtime lives in one place.
- **Single always-on instance** — MCP Streamable-HTTP sessions are
  instance-affine (`Mcp-Session-Id` lives on one machine). `min=max=1` avoids
  scattering sessions. Scaling >1 needs sticky routing first.
- **Private only** — no public Fly IP. Reachable solely at
  `matrix-browser.internal:8931` from the org. This matters because the exposed
  tool set includes `browser_evaluate` / `browser_run_code_unsafe`
  (RCE-equivalent in the server process).
- **`--isolated`** — each daemon session gets a throwaway browser context, so
  cookies/auth don't leak across users.

## Deploy

Org is `personal` (display name "Andrew") — the same org as `matrix-daemon`, so
`matrix-browser.internal` 6PN DNS resolves from the daemons.

**Token scope matters.** The `FLY_API_TOKEN` in `/etc/matrix/router.env` is an
**app-scoped deploy token for `matrix-daemon`** — it can push/deploy that app
but CANNOT create new apps or deploy `matrix-browser`. App creation + the first
deploy need an **org-capable** credential (`flyctl auth login`). And `flyctl`
prefers `$FLY_API_TOKEN` over your login, so `unset` it first or it shadows you.

```bash
# Create + first deploy: use your own org-level login, NOT the daemon token.
unset FLY_API_TOKEN                       # stop the app-scoped token shadowing login
flyctl auth login                         # prints a URL; auth as Andrew
flyctl apps create matrix-browser --org personal   # private; do NOT allocate a public IP

flyctl deploy --ha=false --now -c deploy/browser/fly.toml

# Confirm it's private (should show NO public v4/v6) + running:
flyctl ips list  -a matrix-browser
flyctl status    -a matrix-browser
```

For repeatable/automated redeploys, mint a dedicated app-scoped token once and
store it (e.g. as `MATRIX_BROWSER_DEPLOY_TOKEN`):

```bash
flyctl tokens create deploy -a matrix-browser   # then: FLY_API_TOKEN=<tok> flyctl deploy ...
```

### Optional bearer auth
Set `MATRIX_BROWSER_TOKEN` in the router env to have every daemon send
`Authorization: Bearer <token>`. (Playwright-MCP itself does not validate it;
this is for a future token-checking reverse proxy in front of the app.)

## Wiring the fleet
The daemon proxy reads `MATRIX_BROWSER_URL` (default
`http://matrix-browser.internal:8931/mcp`, injected by the router's
`MachineEnv`). To override fleet-wide, set it in `/etc/matrix/router.env` and
restart `matrix-router`. The proxy answers `initialize`/`tools/list` locally, so
if this app is down the daemons still boot — `browser_*` calls just return a
structured "not configured / unreachable" error until it's back.

## Version pinning (3 places, bump together)
1. `deploy/browser/Dockerfile` — `ARG PWMCP_VERSION`
2. `tools/browser/playwright-tools.json` — the verbatim `tools/list`
3. `agents/default.json` — the enumerated `browser` tool set + `package_digest`

After any change: `node tools/browser/browser.mjs --selftest` (offline drift
guard — must print `browser OK`).

## Hardening backlog
- Drop `browser_run_code_unsafe` / `browser_evaluate` from the surface (needs a
  Playwright-MCP cap filter, or curate via the proxy).
- Per-user browser pods instead of one shared trust boundary.
- Token-checking reverse proxy (Caddy/nginx) in front of `:8931`.
- Liaison human-approval gate before side-effecting actions (form submit, login,
  purchase) on a user's behalf.
