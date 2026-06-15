# Architecture

Chronos is a centralized control plane: one `chronosd` process serves every per-user agent in the Matrix fleet. It is NOT a per-daemon timer — it is the single source of truth for scheduled time across the entire system.

Source files: `chronos.frozen.kvx` (design contract), `cmd/chronosd/main.go` (service entry).

---

## The 5 layers

Chronos is built as five stacked layers, each with a single responsibility:

```
┌──────────────────────────────┐
│  Layer 5: API + Auth         │  HTTP surface for the MCP proxy
│  server/server.go            │  Two-layer auth (transport + principal)
└──────────────────────────────┘
              │
┌──────────────────────────────┐
│  Layer 4: Wake               │  Deliver the contextful turn to the agent
│  wake/wake.go                │  via the router's proven wake path
└──────────────────────────────┘
              │
┌──────────────────────────────┐
│  Layer 3: Dispatch           │  Poll-and-claim worker that fires due alarms
│  dispatch/dispatch.go        │  Retry ladder, at-least-once delivery
└──────────────────────────────┘
              │
┌──────────────────────────────┐
│  Layer 2: Schedule           │  Parse + next-fire computation
│  schedule/schedule.go        │  once (delay/absolute) + cron (5-field/@descriptor/@every)
└──────────────────────────────┘
              │
┌──────────────────────────────┐
│  Layer 1: Store              │  Postgres alarms table — the durable timer
│  store/store.go + alarms.go  │  State lives in the DB, never in memory
└──────────────────────────────┘
```

The core insight: the **hard half** (waking a suspended agent) already exists in the router. Chronos owns only the **easy half** (durable timing + context storage) and hands the fire off to the router's battle-tested `EnsureStarted` → `waitDaemonReady` → 6PN reverse-proxy path.

---

## Topology

Chronos runs on the core-infra box alongside the router and gateway, as a systemd service. It shares the same Postgres instance the router and gateway use.

```
Chronos topology (3 network directions)
========================================

INBOUND (agent → chronosd)
  daemon's chronos.mjs → public nginx /chronos/ → 127.0.0.1:9096
  Purpose: post, list, cancel alarms

WAKE (chronosd → router)
  chronosd → POST 127.0.0.1:8088/internal/wake (wake-token auth)
  Purpose: request the router to wake a machine

DELIVER (router → daemon)
  router → fly.EnsureStarted → waitDaemonReady → POST :8080/chat over Fly 6PN
  Purpose: actually wake the machine + inject the context turn
```

Chronos **never** talks to Fly or the daemon directly. It reuses the router's existing wake-then-proxy path via one new router surface: `POST /internal/wake`.

---

## Mission

> Give every Matrix agent a reliable sense of time: schedule future work, defer mid-task, recur daily, and get woken with enough context to resume seamlessly.

Agents are episodic — a per-user daemon scales to zero between requests and has no way to act later or wait without holding an expensive live process. A centralized durable scheduler lets an agent set work down, release the machine, and be woken precisely when due.

**Chronos is:** the agent alarm clock + reminder service — post an alarm, get woken with context.

**Chronos is NOT:** a workflow engine, a job queue for heavy compute, or an on-chain trustless scheduler. It is off-chain timing + contextful wake delivery only.

---

## Design principles

| Principle | Meaning |
|---|---|
| **Centralized** | One `chronosd` serves the whole fleet; agents are clients, not schedulers |
| **Durable by default** | Every alarm is a Postgres row; a restart never loses a scheduled wake |
| **Context is the point** | The value is the wake payload — the agent stores enough context at post time to resume with zero re-derivation |
| **At-least-once** | Delivery is at-least-once with idempotency; better to wake twice (dedup-able) than silently miss |
| **DID-scoped** | An agent can only create/list/cancel its own alarms; the wake target is derived from the verified DID |
| **Off-chain only** | Pure off-chain infra; the on-chain Scheduler precompile `0x0905` stays reserved for Deus trustless/economic triggers |
| **Structural safety** | No throttles/allowlists on what an agent may schedule; safety is structural — deferred money escalates to MCL at fire time |
| **Honest failure** | A wake that cannot be delivered is retried then surfaced; Chronos never fabricates that an agent was woken |

---

## Relationship to other subsystems

| Subsystem | How Chronos relates |
|---|---|
| **Neo** | Concrete backend for Neo's `scheduled_tasks` — Neo sets an alarm via the MCP tool, gets woken into the same conversation |
| **UWAC** | Supersedes the UWAC spec note that time-triggers fire via on-chain Scheduler `0x0905` — a UWAC poll/cron trigger schedules a Chronos alarm |
| **MCL** | Ordinary MCP server, so the rigorous compile→plan→execute pipeline can schedule too; Chronos is agent-agnostic |
| **Router** | Chronos depends on ONE new router surface (`POST /internal/wake`); everything else is reused |
| **Chain** | Off-chain only; the on-chain Scheduler precompile `0x0905` stays for Deus trustless/economic triggers + the MCL monetary/irreversible on-chain rail |
