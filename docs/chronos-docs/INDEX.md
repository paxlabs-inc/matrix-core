# Chronos Developer Documentation

Chronos is Matrix's centralized agent scheduler and wake control plane. One always-on `chronosd` service serves the entire fleet: agents POST alarms (once or cron) with a contextful wake payload, Chronos durably stores them in Postgres, fires due alarms, and asks the router to wake the agent and deliver the resume turn.

This documentation is written for people working on Chronos itself — extending the schedule engine, modifying the dispatch worker, wiring new tools, or understanding how the wake path works.

---

## Contents

| Document | What it covers |
|---|---|
| [Architecture](./architecture.md) | The 5-layer design, topology, network directions, and mission |
| [Data Model](./data-model.md) | The `alarms` table, `Alarm` struct, `View` projection, lifecycle statuses |
| [Schedule Engine](./schedule-engine.md) | `once` and `cron` kinds — `NextOnce`, `NextCron`, timezone handling, `robfig/cron/v3` |
| [Dispatch Worker](./dispatch-worker.md) | Poll-and-claim loop, `FOR UPDATE SKIP LOCKED`, retry ladder, at-least-once delivery |
| [Wake Delivery](./wake-delivery.md) | The 6-step wake path: chronosd → router → Fly → daemon → Neo |
| [Auth System](./auth-system.md) | Two-layer auth: transport bearer + agent-DID ed25519 challenge/verify + HMAC principal tokens |
| [API Reference](./api-reference.md) | HTTP surface — healthz, agent auth, alarm CRUD, request/response shapes |
| [Config System](./config-system.md) | Env-first configuration with optional `chronos.config.kvx` overlay |
| [Tool Surface](./tool-surface.md) | MCP stdio proxy — `alarm_set`, `alarm_list`, `alarm_cancel`, selftest, manifest bijection |

---

## Repository layout

```
chronos/
├── cmd/chronosd/
│   └── main.go              # Service entry: config load, store init, worker spawn, HTTP server
├── internal/
│   ├── auth/
│   │   ├── identity.go      # DID parsing, ed25519 challenge/verify, single-use nonce store
│   │   ├── token.go         # Stateless HMAC principal tokens (mint + verify)
│   │   └── auth_test.go     # Round-trip tests for DID, challenge, signature, token
│   ├── config/
│   │   ├── config.go        # Config struct, Load(), env-over-kvx-over-defaults
│   │   └── kvx.go           # Zero-dep .kvx parser (sectioned key/value, ${ENV} interpolation)
│   ├── dispatch/
│   │   └── dispatch.go      # Poll-and-claim worker: ClaimDue, fire, retry ladder, backoff
│   ├── schedule/
│   │   ├── schedule.go      # NextOnce (delay/absolute), NextCron (5-field/@descriptor/@every)
│   │   └── schedule_test.go # Unit tests for all schedule paths
│   ├── server/
│   │   └── server.go        # HTTP mux: healthz, agent auth, alarm CRUD, transport middleware
│   ├── store/
│   │   ├── store.go         # Postgres pool, migration runner (chronos_schema_migrations)
│   │   └── alarms.go        # Alarm queries: create (idempotent), list, get, cancel, claim-due, reschedule, retry, fail
│   ├── telemetry/
│   │   └── telemetry.go     # Structured JSON logger (slog)
│   └── wake/
│       └── wake.go          # HTTPWaker — POSTs /internal/wake to the router with wake token
├── migrations/
│   └── 001_init.sql         # alarms table + indexes (due, owner, idempotency)
├── pkg/types/
│   └── types.go             # Wire contracts: Envelope, Error, Alarm, View, CreateAlarmRequest, auth types
├── chronos.frozen.kvx       # Frozen architecture spec (design contract)
├── go.mod
└── go.sum
```

---

## The one-sentence contract

Chronos takes an alarm (a future time + a contextful wake message + an opaque payload), durably stores it in Postgres, fires it when due, and delivers the wake to the agent via the router's proven machine-wake path. The agent resumes into its stored conversation with enough context to pick up exactly where it left off.

That is the whole point. It's the boundary between the agent's episodic, scale-to-zero world and the durable, always-on world of time.

---

## Key locked decisions

These decisions are frozen in `chronos.frozen.kvx`. Don't re-litigate them without an explicit spec version bump.

| ID | Decision |
|---|---|
| **i1** | Alarms are durable — a Chronos restart never loses a scheduled wake (state lives in Postgres, not memory) |
| **i2** | DID-scoped — an agent can only create/list/cancel its own alarms; the wake target is derived from the verified DID, never caller-supplied |
| **i3** | At-least-once delivery with idempotency — a fire is recorded only after the router confirms; duplicates are dedup-able |
| **i4** | Context fidelity — `wake_message` + `payload` are delivered verbatim; high-entropy tokens (ids, hashes, cursors) are never paraphrased |
| **i5** | Off-chain only — Chronos holds no signing key and performs no on-chain action; deferred money escalates to MCL at fire time |
| **i6** | Honest failure — an undeliverable wake is retried then surfaced (status + last_error); never fabricated |
| **i7** | Chronos never writes cortex / signs envelopes / touches plan-walk — it delivers a chat turn via the existing `/chat` path; the agent does the work |
| **i8** | Tool registry ↔ manifest stay in strict bijection (`chronos-tools.json` == `agents/*.json`) or daemon boot is fatal |