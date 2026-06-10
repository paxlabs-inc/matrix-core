# Marketplace Ops Runbook

Operator-side provisioning for `marketplace/` (Cloudflare Workers) and the
deusd developer-auth change. Everything here is the part the code cannot do
for itself. Code-side behavior is env-gated throughout: each feature no-ops
until its resource/secret exists, so these steps can land incrementally.

## 0. Deploy ordering (IMPORTANT)

The deusd trust-boundary fix and the marketplace SIWE flow ship together:

- New deusd **requires `X-Developer-Token`** for owner-scoped routes unless
  `DEUS_DEV=1`. The bare `X-Developer-Wallet` header no longer authenticates
  in production — that header was the earnings-theft hole.
- Old marketplace builds only send the bare header.

Order: deploy **deusd first**, then the marketplace immediately after.
In between, dashboard owner actions return 401 (public pages unaffected).
Already-linked wallets must re-link (one signature) because old sessions hold
no developer token.

## 1. deusd (Supabase box, deploy/deus)

Add to `/opt/deus/deus.env`:

```bash
# HMAC secret for SIWE nonces + developer tokens. Falls back to
# DEUS_GATEWAY_SIGNING_KEY when unset; set a dedicated value:
DEUS_DEVELOPER_AUTH_SECRET=$(openssl rand -hex 32)
# Pin EIP-4361 messages to the marketplace host (reject foreign-domain sign-ins):
DEUS_SIWE_DOMAIN=<marketplace host, e.g. market.paxeer.app>
```

Then rebuild/redeploy `deus-control` the usual way (compose build + up from
`deploy/deus/docker-compose.yml`; tag a rollback image first).

Smoke (node, no curl on the box):

```bash
node -e 'fetch("https://deus.paxeer.app/v1/developers/nonce",{method:"POST"}).then(r=>r.json()).then(console.log)'
# => { nonce: "...", expires_at: "..." }
node -e 'fetch("https://deus.paxeer.app/v1/me/services",{headers:{"X-Developer-Wallet":"0x1111111111111111111111111111111111111111"}}).then(r=>console.log(r.status))'
# => 401  (the old hole is closed)
```

## 2. Cloudflare Worker secrets

From `marketplace/` (per env: add `--env staging` / `--env production`):

```bash
wrangler secret put SESSION_SECRET          # openssl rand -hex 32
wrangler secret put SUPABASE_URL            # https://supabase.paxeer.app
wrangler secret put SUPABASE_ANON_KEY
wrangler secret put TURNSTILE_SECRET_KEY    # after §4
wrangler secret put SENTRY_DSN              # optional; enables error reporting
```

Notes:

- Production **throws on boot-path requests** if `SESSION_SECRET` is missing
  or shorter than 16 chars (fail-closed; no more forgeable-cookie fallback).
- Dev email login is dead in production regardless of flags
  (`ENVIRONMENT=production` is set in wrangler.jsonc vars).

## 3. KV namespaces (sessions + data cache)

```bash
wrangler kv namespace create marketplace-sessions
wrangler kv namespace create marketplace-cache
# repeat with --env production / --env staging for separate namespaces
```

Paste the returned IDs into the commented `kv_namespaces` blocks in
`wrangler.jsonc` (top level + per env) and redeploy. Until then:

- Sessions fall back to signed cookie storage (works, but no server-side
  revocation).
- The data cache falls back to per-isolate memory; the one-shot form tokens
  (payout/create-listing) fall back to button-disable only.

## 4. Turnstile (bot protection)

1. Cloudflare dashboard → Turnstile → Add site (managed mode) for the
   marketplace hostname.
2. Put the **site key** in `wrangler.jsonc` `vars.TURNSTILE_SITE_KEY`
   (it's public) for each env.
3. `wrangler secret put TURNSTILE_SECRET_KEY`.

Widgets appear automatically on the dev login form and the try-it run panel;
actions verify server-side via `siteverify`. Unset = disabled (local dev).

## 5. Rate limiting

Workers rate-limit bindings are already declared in `wrangler.jsonc`
(`RL_LOGIN` 10/min, `RL_WALLET` 20/min, `RL_INVOKE` 60/min, keyed by IP or
wallet) and deploy with the worker — no dashboard work. Tune limits there.

Layer the edge on top (dashboard):

- WAF → managed rulesets ON for the zone.
- Bot Fight Mode (or Bot Management if licensed).
- Optional zone Rate Limiting rules in front of `/login` and `/services/*`
  POSTs as a coarse outer ring.

## 6. Secrets hygiene (one-time)

`.dev.vars` is now gitignored with a committed `.dev.vars.example`, but the
old file is still tracked. From the repo root, when you next commit:

```bash
git rm --cached marketplace/.dev.vars
```

(Values in it were dev-only; nothing to rotate.)

## 7. Supabase OAuth (PKCE)

The login flow now sends `code_challenge` (S256) and the callback exchanges
`?code=` server-side via `/auth/v1/token?grant_type=pkce`. GoTrue supports
this out of the box on current self-hosted images. Verify on the box that the
redirect URL allow-list includes `https://<marketplace-host>/auth/callback`
(GOTRUE_URI_ALLOW_LIST in /opt/supabase/.env). Implicit-flow fragments
(`#access_token=`) are no longer accepted by the callback.

## 8. Caching / CDN (dashboard)

- **Tiered Cache**: Caching → Tiered Cache → Smart Tiered Caching ON.
- Edge HTML caching is in-worker (Cache API, 60s fresh / 10min SWR /
  24h stale-if-error, anonymous GETs only). Responses carry `X-Edge-Cache:
  hit|miss|stale-while-revalidate|stale-if-error` for monitoring — chart the
  hit ratio in Workers Logs and alert if it collapses.
- Purge-on-publish: not wired (cache tags are Enterprise). Worst case a
  published/paused listing is stale on public pages for ~10 minutes. If that
  bites, purge by URL via the CF API from the deusd publish path.

## 9. CI deploy token

Create a scoped API token (template "Edit Cloudflare Workers", scoped to the
account + zone) and store as the `CLOUDFLARE_API_TOKEN` repo secret. The
`marketplace` CI job currently builds/tests only; add a deploy step with
`wrangler deploy --env production` gated on main when ready. `workers_dev`
is disabled for production in wrangler.jsonc.

## 10. Monitoring

- Uptime monitor → `https://<host>/healthz` (liveness) and
  `https://<host>/healthz?deep=1` (also probes deusd, 3s timeout).
- Workers Logs: structured JSON lines `{msg:"request", request_id, path,
  status, duration_ms, edge_cache}`; the same `request_id` reaches deusd as
  `X-Request-ID`.
- Sentry: set `SENTRY_DSN` and unhandled SSR/stream errors are reported
  (dependency-free store-API client; swap to @sentry/cloudflare if/when more
  is needed).

## 11. Legal artifacts

Drop final HTML files over the placeholders in `marketplace/public/legal/`:
`terms.html`, `privacy.html`, `acceptable-use.html`,
`developer-agreement.html`, `dmca.html`, `refunds.html`,
`risk-disclosure.html`, `security-policy.html`. Footer + security.txt already
link to these URLs; replacing the files requires a redeploy (they ship as
Worker assets). Remove the `noindex` meta from the final versions.
`abuse@paxeer.app` (service-report mailto) and `security@paxeer.app`
(security.txt) need real mailboxes.

## 12. Staging

`wrangler deploy --env staging` deploys `deus-marketplace-staging` with
production-like behavior (CSP on, dev auth off). Provision its own KV
namespaces and secrets (same commands with `--env staging`).

## 13. Optional: private origin hop

The Worker reaches deusd over the public edge (`https://deus.paxeer.app`).
A Service Binding is impossible (deusd is not a Worker). If the public hop
ever becomes a concern, run `cloudflared` on the Supabase box and point
`DEUS_API_URL` at the tunnel hostname — no code change needed.
