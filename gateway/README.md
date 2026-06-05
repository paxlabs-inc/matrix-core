# gateway — Matrix LLM Gateway (sess#32)

Top-level Go module that mediates every per-user daemon's LLM call to
Fireworks/Together, debiting a Postgres `credit_ledger` row per call
and enforcing a daily PAX hard-stop. Implements the contract in
[`journal/plan/01-ambient-architect.md`](../journal/plan/01-ambient-architect.md)
§5.15 and §10.

## Why

Ambient agents run 24/7. The off-switch must live in the LLM call
path, not on the honor system. The gateway is that off-switch.

## Wire shape

```
client → https://matrix.paxeer.app/gw/v1/chat/completions
nginx  → http://127.0.0.1:9090/v1/chat/completions   (strips /gw/)
gw     → upstream Fireworks / Together
```

Endpoints (mounted on the loopback listener):

| method | path                      | purpose                          |
|--------|---------------------------|----------------------------------|
| POST   | `/v1/chat/completions`    | OpenAI-compat chat completions   |
| POST   | `/v1/embeddings`          | OpenAI-compat embeddings         |
| GET    | `/healthz`                | unauthenticated liveness         |

## Required headers

| header                  | required | meaning                                       |
|-------------------------|----------|-----------------------------------------------|
| `Authorization`         | always   | `Bearer ${MATRIX_GATEWAY_TOKEN}`              |
| `X-Matrix-Actor-DID`    | always   | wallet/DID; ledger key                        |
| `X-Matrix-Slot`         | always   | `compiler` / `planner` / `executor`           |
| `X-Matrix-Intent-ID`    | optional | for cost-by-intent rollups                    |
| `X-Matrix-Goal-ID`      | optional | for cost-by-goal rollups                      |
| `X-Matrix-Kind-Route`   | optional | executor sub-route (`reason`,`code`,…)        |
| `X-Matrix-BYO-API-Key`  | optional | `true` to bypass metering                     |
| `X-Matrix-User-API-Key` | BYO-only | caller's own provider API key                 |

## Response augmentations

| response header                | meaning                            |
|--------------------------------|------------------------------------|
| `X-Matrix-Cost-Pax`            | this call's cost (PAX, fixed-12)   |
| `X-Matrix-Daily-Spent-Pax`     | actor's running daily spend        |
| `X-Matrix-Daily-Remaining-Pax` | actor's daily-cap headroom         |
| `X-Matrix-Rate-Table-Version`  | `rates.RateTableVersion`           |

## Free-tier whitelist (plan §5.15, v1 launch 2026-06-01)

All model IDs below are prefixed `accounts/fireworks/models/`.

| slot       | allowed models                              | notes                                                                                                   |
|------------|---------------------------------------------|---------------------------------------------------------------------------------------------------------|
| `compiler` | `gpt-oss-120b`, `deepseek-v4-pro`           | `v4-pro` is the low-confidence escalation target: the daemon re-invokes the compiler slot with it when a frame self-reports confidence below threshold (or an invalid verb). |
| `planner`  | `gpt-oss-120b`, `deepseek-v4-flash`, `deepseek-v4-pro` | v1 pins planner = `v4-pro` via `MATRIX_PLANNER_MODEL` (dedicated knob, decoupled from executor); the others stay allowed for smaller-planner deployments. |
| `executor` | `deepseek-v4-flash`, `kimi-k2.6`            | v1 pins executor = `kimi-k2.6` via `MATRIX_EXECUTOR_MODEL`; `v4-flash` stays allowed for summarize / long-context routes. |

Other models on metered traffic → `403 model_not_whitelisted`.
BYO bypasses the whitelist AND skips metering.

## Budget hard-stop

On every metered call the gateway:

1. Reads `actor`'s daily spend from `credit_ledger`.
2. Reads `actor`'s daily cap from `daily_budget_caps` (default 10 PAX).
3. If spend + projected-cost > cap → `429 budget_exhausted`:
   ```json
   {"error":"budget_exhausted","spent_pax":"...","limit_pax":"..."}
   ```
4. Otherwise forwards upstream. On 2xx, prices via
   `internal/rates.Cost`, debits ledger, stamps response headers.
5. On non-2xx upstream → forwards body verbatim, no debit.

## Streaming

`stream=true` requests pipe the SSE response through unmodified. The
trailing `usage` chunk (Fireworks + Together both emit one) is
scanned out of the stream and used to debit the ledger. Cost headers
are NOT added on streaming responses (the 200 status flushed before
upstream emits the usage trailer); daemons read cost via the ledger
snapshot endpoint instead (TODO future).

## Kill switches

* `MATRIX_GATEWAY_DISABLED=true` env → 503 to every request, including
  `/healthz`. Restart to clear.
* `daily_budget_caps.daily_pax_max=0` for an actor → all their calls
  return 429 instantly.
* `-postgres-uri=""` → in-memory ledger (local-dev only; no
  cross-process persistence).

## Layout

```
gateway/
  cmd/matrix-gateway/main.go     CLI; flags wire all the internals
  internal/auth/                 bearer + actor + (future) ed25519
  internal/ledger/               credit_ledger writer; Postgres + Memory
  internal/proxy/                HTTP reverse proxy
  internal/ratelimit/            per-actor token bucket
  internal/rates/                model→PAX/Mtoken table + RateTableVersion
  internal/routing/              free-tier whitelist + provider selection
  internal/types/                shared header names + wire structs
  migrations/001_credit_ledger.sql
  deploy/matrix-gateway.service  systemd unit
  deploy/nginx-snippet.conf      nginx /gw/ block
  deploy/install.sh              idempotent box installer
```

## Schema

See [`migrations/001_credit_ledger.sql`](migrations/001_credit_ledger.sql).

`credit_ledger` is the per-call audit log; `daily_budget_caps` is the
operator's per-actor daily PAX limit. Both ship verbatim to the box
under `deploy/box/postgres/migrations/002_credit_ledger.sql`.

## Build & test

```
go mod tidy
go vet ./...
go test ./... -count=1
```

The default test suite uses the in-memory ledger and stubbed httptest
upstreams; no Postgres or Fireworks/Together reachability is
required.

## Driver

`internal/ledger/postgres.go` uses `database/sql` exclusively. The
`gateway` module's `go.mod` is intentionally driver-free; the binary's
build tags should side-import a driver:

```go
//go:build pq
package main
import _ "github.com/lib/pq"
```

For local-dev / CI without Postgres, leave `-postgres-uri=""` and the
in-memory ledger is used.

## Related

* Daemon-side wiring: `MCL/llm/llm.go` (`Config.GatewayURL`) +
  `executor/cmd/mcl-execute/intent_cost.go`.
* Tauri wizard secret: `MATRIX_GATEWAY_URL` (already wired in
  `desktop/src-tauri/src/wizard.rs`).
* Plan: `journal/plan/01-ambient-architect.md` §5.15, §5.16, §10.
