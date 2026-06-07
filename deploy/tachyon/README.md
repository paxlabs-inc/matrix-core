# matrix-tachyon — shared Solidity/EVM engine for the agent fleet

A single private Fly app running **tachyond** (the agent-native Solidity/EVM
toolbox, `github.com/paxlabs-inc/tachyon-tools`) over its JSON-RPC `/rpc`
transport. Every per-user daemon reaches it over Fly's private 6PN network via
the daemon-side stdio proxy `tools/tachyon/tachyon.mjs`, which answers
`initialize`/`tools/list` locally and forwards `tools/call` to the engine.

```
daemon (per user)                          matrix-tachyon (shared)
┌──────────────────────────────┐            ┌──────────────────────────┐
│ executor ──stdio──> tachyon  │            │ tachyond                 │
│            tools/tachyon/     │  6PN/HTTP  │  forge + solc 0.8.27     │
│            tachyon.mjs ───────┼──────────> │  :8645/rpc  (JSON-RPC)   │
└──────────────────────────────┘  POST /rpc └──────────────────────────┘
        │ mints + forwards                      embedded wallet = MULTI-TENANT
        │ this agent's wallet_token             (keyfile-empty, holds NO seed)
        ▼
  did:matrix bearer (executor.key)
```

## Why this shape
- **One shared engine, not Foundry-per-daemon** — keeps the per-user image
  light; `forge` + `solc` + the OpenZeppelin corpus live in one place.
- **Seedless / multi-tenant custody** — the box runs the embedded signer in
  keyfile-empty mode. WRITE tools (`tachyon_deploy`, and `tachyon_call` unless
  `simulate_only`) require a forwarded `wallet_token` that the daemon proxy
  mints from *its own* `executor.key`. The engine signs + broadcasts as the
  **calling agent**, via `connect.paxportwallet.com`; no signing key ever lands
  on this box.
- **Upload-source model** — callers pass a `sources` map; the engine compiles
  in an ephemeral workdir that links this image's baked `contracts/` (OZ) +
  `lib/` (forge-std) so `@openzeppelin/contracts/...` and `forge-std/...`
  imports resolve. Contracts under `src/`, tests under `test/`.
- **Private only** — no public Fly IP. Reachable solely at
  `matrix-tachyon.internal:8645` from the org.

## Deploy

Org is `personal` ("Andrew") — same org as `matrix-daemon`, so
`matrix-tachyon.internal` 6PN DNS resolves from the daemons.

**Token scope matters.** The `FLY_API_TOKEN` in `/etc/matrix/router.env` is an
app-scoped deploy token for `matrix-daemon` — it CANNOT create new apps. App
creation + the first deploy need an org-capable credential (`flyctl auth
login`). `flyctl` prefers `$FLY_API_TOKEN`, so `unset` it first.

The build context is the **repo root** (the Dockerfile COPYs `tachyon/...`),
so run `flyctl deploy` from `/root/matrix`:

```bash
unset FLY_API_TOKEN                       # stop the app-scoped token shadowing login
flyctl auth login                         # auth as Andrew
flyctl apps create matrix-tachyon --org personal   # private; do NOT allocate a public IP

# from the repo root (context = .) so tachyon/ is in the build context:
flyctl deploy --ha=false --now -c deploy/tachyon/fly.toml .

# Confirm it's private (NO public v4/v6) + healthy:
flyctl ips list  -a matrix-tachyon
flyctl status    -a matrix-tachyon
flyctl ssh console -a matrix-tachyon -C "curl -fsS http://127.0.0.1:8645/healthz"
```

For repeatable redeploys, mint a dedicated app-scoped token once:

```bash
flyctl tokens create deploy -a matrix-tachyon   # then: FLY_API_TOKEN=<tok> flyctl deploy ...
```

> The `lib/` Foundry submodules (forge-std, etc.) must be checked out in the
> working tree before deploy — they're copied into the image. If `lib/` is empty,
> run `git -C tachyon submodule update --init` first.

### Optional bearer auth
Set `TACHYON_AUTH_TOKEN` as a Fly secret to require a bearer on every request
except `GET /healthz` and `GET /`, and set the matching `MATRIX_TACHYON_TOKEN`
in the router env so daemons send it:

```bash
flyctl secrets set TACHYON_AUTH_TOKEN=<tok> -a matrix-tachyon
# then in /etc/matrix/router.env: MATRIX_TACHYON_TOKEN=<tok>  (restart matrix-router)
```

## Wiring the fleet
The daemon proxy reads `MATRIX_TACHYON_URL` (default
`http://matrix-tachyon.internal:8645/rpc`, injected by the router's
`MachineEnv`). Override fleet-wide via `/etc/matrix/router.env` + restart
`matrix-router`. The proxy answers `initialize`/`tools/list` locally, so if this
app is down the daemons still boot — `tachyon_*` calls just return a structured
"not configured / unreachable" error until it's back.

## Version pinning (bump together)
1. `deploy/tachyon/Dockerfile` — `ARG FOUNDRY_VERSION` (and the warmed
   `solc_version`, which tracks `tachyon/foundry.toml`).
2. `tools/tachyon/tachyon-tools.json` — the advertised tool set.
3. `agents/default.json` — the enumerated `tachyon` tool set + `package_digest`.

After any tool change: `node tools/tachyon/tachyon.mjs --selftest` (offline
drift guard — must print `tachyon OK`) and `node tools/tachyon/proxy.selftest.mjs`
(hermetic forwarding + wallet-token test).

## State + scaling caveat
The deployment registry, cached artifacts, and active-chain selection are
in-process (`registry.json` on the container fs). That's why this is a single
always-on instance: a `compile` and its follow-up `deploy` must land on the same
machine. To scale >1, move the registry to a shared volume/store and add sticky
routing first.

## Hardening backlog
- Per-request resource caps on `forge` (CPU/mem/time) — uploaded sources run
  arbitrary solc; today only the source-size cap + workdir isolation apply.
- `--no-ffi` is the forge default (we never pass `--ffi`), but consider a
  seccomp/namespace sandbox for the compile subprocess.
- Move registry/artifacts to a Fly Volume for restart durability.
