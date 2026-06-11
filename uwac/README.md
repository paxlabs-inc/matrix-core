# UWAC — Universal Web App Connectors

UWAC is the Matrix control plane that lets an agent act inside a user's external
apps (Gmail, Calendar, …) **without the agent ever holding the OAuth token**.
It is the credential analogue of the embedded wallet: just as the daemon signs
transactions it never holds the key for, here it invokes app actions it never
holds the token for. The token is vaulted server-side; only scoped **results**
return to the daemon.

> Design of record: `../uwac/connections.frozen.kvx` (frozen spec). Read it
> first — it explains the philosophy, the consequence model, and the credential
> flow this code implements.

## Architecture

```
agent (per-user Fly daemon)
  │  MCP stdio
  ▼
tools/uwac/uwac.mjs ──HTTP──▶ uwacd (this module, shared Fly app)
                               ├─ agent-DID auth  (challenge/verify, ed25519)
                               ├─ OAuth connect   (GoTrue scope-elevation PKCE)
                               ├─ token vault     (AES-256-GCM at rest)
                               └─ tool invoke     (scope+consequence gate → provider API)
                                     │
                                     ▼ injects token server-side
                               Google / GitHub / Slack / …
```

### Two-layer auth
- **Transport** — shared `MATRIX_UWAC_TOKEN` bearer proves the caller is a Matrix
  daemon (router-injected, like `MATRIX_TACHYON_TOKEN`).
- **Principal** — the daemon's ed25519 executor key signs a challenge; uwacd
  resolves the owner Supabase `user_id` from the **DID label**
  (`did:matrix:<user_id>:<keyfp>`), which is the *same* id that consented the app
  via GoTrue. The binding closes itself.

The OAuth token never crosses the wire to the daemon.

## Layout

| Path | Role |
| --- | --- |
| `cmd/uwacd` | server entrypoint (`uwacd`, `uwacd -dump-tools`) |
| `pkg/types` | wire contracts (envelope, connector spec, requests) |
| `pkg/api` | HTTP server (auth, connect/callback, invoke) |
| `pkg/mcp` | MCP tool advertisement (source of truth for `uwac-tools.json`) |
| `internal/engine` | orchestration: auth, connect, gate + refresh + dispatch |
| `internal/identity` | DID parse/verify, challenge store, principal tokens |
| `internal/vault` | per-user token store (in-memory; Postgres TODO) |
| `internal/cryptox` | AES-256-GCM seal/open for tokens at rest |
| `internal/oauth` | GoTrue PKCE connect + provider-token refresh |
| `internal/connectors` | connector registry + the `google-workspace` connector |
| `internal/httpx` | small JSON/form provider HTTP client |
| `internal/catalog` | first-party connector assembly |

The stdio MCP proxy + generated tool registry live at `../tools/uwac/`.

## Build / test

```bash
go build ./... && go vet ./... && go test ./...

# regenerate the proxy's tool registry from the Go source of truth
go run ./cmd/uwacd -dump-tools > ../tools/uwac/uwac-tools.json

# guard against manifest drift (would crash the daemon at boot)
node ../tools/uwac/uwac.mjs --selftest
```

## Run (dev)

```bash
cp .env.example .env   # fill GoTrue + Google client creds; UWAC_ENV=development
set -a && . ./.env && set +a
go run ./cmd/uwacd
curl localhost:8646/healthz
```

## Status

First slice implemented end-to-end: agent-DID auth, GoTrue scope-elevation
connect, in-memory encrypted vault, and the `google-workspace` connector
(Gmail + Calendar). **Not yet wired:** Postgres-backed vault + background
refresher, marketplace UI, and prod deploy (gated — do not deploy without
sign-off).
