# Design — Cody (the next-generation coding agent)

## Overview

Cody is a specialized coding agent that serves both engineers and vibe coders by
productizing the discipline Matrix was built with: recall-first memory,
spec-driven planning, dependency-waved tasks, no-fakes verification, honest
partials, complete artifacts. The differentiator is that the rigor is
STRUCTURAL — enforced by the engine — not prompted. A mode dial (Prototype /
Engineer / Architect) changes how much ceremony surrounds the work, never
whether the constitution holds.

Two load-bearing ideas:

1. **The environment is the machine.** Matrix already gives every user a
   private, full-access, per-user sandbox (the daemon machine). This feature
   migrates that environment from Fly Machines to Railway (service + volume,
   Ubuntu 24.04 image) and upgrades it from "daemon host" to "daemon host +
   persistent dev environment": real toolchains, `/workspace` on the volume,
   the place where the user's code lives and where Cody codes. No local
   execution, no sandbox hand-off — Cody works inside the user's own machine.

2. **The orchestrator never writes code.** Cody prime Plans, Specs, and
   Delegates: it spawns ONE worker subagent per task (sequential, one alive at
   a time), hands it a self-contained task sheet, independently verifies the
   turn-in, and moves on. Implementation noise never enters the orchestrator's
   context window — that is what sustains long-horizon workloads.

## Locked decisions (user, 2026-07-02)

- **New module, shared internals** — `cody/` is a sibling of `neo/` importing
  the battle-tested libraries (`neo/internal/llm`, cortex continuous-memory,
  delegate failure taxonomy, supervisor durability patterns). NOT a Neo
  persona; NOT built from scratch.
- **One spec** — the Railway migration is the P1 foundation wave of THIS
  feature, not a separate spec.
- **Railway wake-on-request** — keep scale-to-zero semantics; the environment
  sleeps when idle and wakes on inbound traffic.
- **Cody skips MCL** — coding is not value-moving. Direct tool loop; no
  `core_execute`, no wallet/spend seam, no signing key. The MCL walk is
  untouched.
- **Modes = Prototype / Engineer / Architect** — Surgeon cut. A mode is a
  policy tuple; the constitution binds in every mode.
- **One worker at a time** — delegation is per TASK, not per wave; sequential
  by design. Parallel waves are a deliberate non-goal for v1.

## Seam map (grounded in current code)

| Seam | Where | Relevance |
| --- | --- | --- |
| Fly provisioning client | `router/internal/fly/fly.go` (thin Machines REST client: create/start/status/destroy + volumes) | The isolated seam the `Provisioner` interface extracts from |
| Provision + wake consumers | `router/internal/admin/admin.go` (StartProvision), `router/internal/proxy/proxy.go` + `wake.go` (EnsureStarted, waitDaemonReady, `/internal/wake` for chronos) | Re-pointed at the interface, not rewritten |
| Router config | `router/internal/config/config.go` (FLY_* envs) | Gains provider selection + Railway credentials |
| Agent tool surface | `agents/neo.json` — fs (`/workspace`), exec (`tools/exec/exec.mjs`: shell + supervised services), git, browser, fetch, web-search | `agents/cody.json` reuses the same deployed MCP bridges; no new bridges for v1 |
| Language rules | `rules/common/*` (9 docs) + `rules/<language>/*` (13 languages) | Injected into prompts per detected stack — the standards Cody never breaks |
| Skills library | `skills/` (186 skills: frontend, backend, DB, web3, agent engineering) | Cody's deep-domain playbooks, surfaced via the existing skill mechanism |
| LLM client | `neo/internal/llm` (SSE streaming, enable_thinking gating, gateway metering) | Imported by both orchestrator and workers |
| Memory brain | cortex continuous-memory (`AppendMessage` / `Activate` / `RecallDescend`, durable story-so-far) | Cody's per-project long-horizon memory + checkpoint substrate |
| Failure taxonomy | `neo/internal/delegate/classify.go` (Transient / Deterministic / Conflict / Pending) | Worker re-dispatch policy: deterministic failures stop-and-ask, never blind respawn (audit NE-5) |
| Adjudication | Neo's Cassandra completion gate (goal-vs-outcome) | The orchestrator's acceptance gate for worker turn-ins |
| Overflow discipline | Neo's overflow-file pattern (oversized tool output spills to file + read-full gate) | Read-full enforced structurally in both roles |
| Gateway slots | `types.SlotNeo` precedent + `rates.go` whitelist | Cody registers its own `cody` slot; no borrowed slots |
| Client host | `apps/client` (Next.js) + the NeoTask/SSE reducer pattern (`hooks/api/useChat.ts`, buildTaskFromTrace) | The coding workspace surface follows the same durable-trace + live-SSE shape |

## Environment migration (Fly → Railway) — final phase

Built LAST (waves 9-12): the Cody engine is developed and verified first on the
existing Fly environment (codyd deploys wherever the daemon runs), then the
environment moves to Railway once the agent is proven.

### Provisioner abstraction

Extract a provider-neutral interface in the router:

```go
type Provisioner interface {
    Ensure(ctx, userID) (Endpoint, error)   // create-or-get env + volume
    Status(ctx, userID) (EnvStatus, error)
    Wake(ctx, userID) error                 // provider-specific wake path
    Destroy(ctx, userID) error
}
```

`router/internal/fly` becomes one implementation (unchanged behavior);
`router/internal/railway` is the new one (Railway public GraphQL API:
project/environment/service/volume/deployment). Config selects the provider
per deployment (`ROUTER_PROVIDER=fly|railway`); Fly stays intact as the
fallback until cutover is proven.

### Wake semantics (wake-on-request)

Fly has an explicit start API; Railway wakes a slept service on inbound
traffic. The router's flow inverts: instead of "start, poll, then forward",
the proxy forwards (the request itself is the wake) and the existing
`waitDaemonReady` probe absorbs cold-start latency. Chronos `/internal/wake`
becomes an HTTP poke at the service. The provision-status flow
(`provision_jobs`, `GET /provision/status`) plumbs into Railway deployment
status so the onboarding readiness gate keeps working.

### The environment image

Ubuntu 24.04 base carrying the full daemon stack (MCL daemon, Neo, codyd, MCP
tool bridges) PLUS a baked dev toolchain: git, build-essential, Node + pnpm,
Go, Python + uv, Rust, Foundry. Volume mounts `/data` (daemon state) and
`/workspace` (user code). `/workspace` is already the root the fs/git/exec MCP
servers point at, so the whole tool surface transfers unchanged. Dev servers
started via exec `service_start` are reachable through the environment's
networking for preview.

## The Cody engine (`cody/`) — built first

### Roles

**Orchestrator (Cody prime).** Holds: repo map, the plan, task sheets,
turn-in reports, mode policy. Never edits files, never runs implementation
commands (its only executions are verification re-runs). Its cycle:

1. **Understand** — workspace model (language/framework/build detection),
   read before write, cortex recall of project memory.
2. **Plan** — waved task list; depth per mode (Prototype: terse inline plan;
   Engineer: waved list; Architect: durable spec files — EARS requirements,
   design doc, waved tasks — persisted in the workspace, resumable across
   sessions: the Matrix /spec method productized).
3. **Spec** — author the task sheet for the next eligible task.
4. **Delegate** — spawn one worker with a fresh context; wait.
5. **Verify** — independently re-run the verification commands and adjudicate
   the turn-in goal-vs-outcome (never trusts the worker's word).
6. **Accept or re-dispatch** — accept: checkpoint to cortex, discard the
   worker transcript, keep the turn-in report. Reject: bounded re-dispatch
   with concrete feedback; deterministic failures stop-and-ask.
7. Loop to 3 until the plan is done; deliver the completion report.

**Worker.** A fresh-context instance of the same engine with the full tool
surface (fs/exec/git/browser), the edit engine, and the verification runner.
Receives ONLY the task sheet + a minimal grounding bundle. Must return a
structured turn-in report. Dies after turn-in; a crashed/failed worker is
replaced by a fresh one from the same sheet (Task Durability at task level).

### The task sheet (orchestrator → worker contract)

Self-contained — the worker needs zero conversation history:

```
sheet {
  task_id, title, goal                      // what done means
  acceptance[]                              // the req clauses, testable
  grounding { files[], line_refs[], notes } // exact seams, read-before-write set
  constraints { constitution, mode_policy, rules_refs[] }  // incl rules/<lang>
  verify { commands[], must_be_green }      // detected project commands
  deliverable { shape, do_not_touch[] }
}
```

### The turn-in report (worker → orchestrator contract)

```
report {
  task_id, status: done|partial|blocked
  changes[] { path, kind, why }
  verification { command, exit, output_excerpt }[]   // real evidence
  gaps[], assumptions[]                              // honest partials
}
```

### Coding-native primitives

- **Workspace model** — persistent repo index over `/workspace` (tree,
  languages, frameworks, build targets, entry points), maintained on the
  volume across sessions.
- **Edit engine** — anchored find/replace edits with staleness detection
  (edit fails if the file drifted since read), never blind overwrites.
- **Verification runner** — detects the project's own commands
  (package.json scripts, Makefile, go.mod, cargo, pytest, …) and runs
  build/test/lint/typecheck. Task-done is GATED on green — structurally: the
  accept path requires a passing verification record, and tests may never be
  weakened or deleted to pass.
- **Checkpoint/rollback** — workspace snapshot before risky multi-file
  changes; restore on request. Progress checkpoints per accepted task go to
  cortex (survive restarts and session refreshes).

## The constitution (engine-enforced, every mode)

- **No fakes** — never stub/mock/fake to make a test pass; verification runs
  real code paths.
- **Verify before done** — the acceptance gate requires green verification +
  goal-vs-outcome adjudication.
- **Read full** — truncated tool output must be retrieved in full before
  reasoning (overflow-file discipline).
- **No false success** — partials are surfaced as partials, in the turn-in
  report and in the user-facing completion report.
- **Complete artifacts** — runnable deliverables with dependencies and run
  instructions, never fragments.
- **User drives git** — Cody stages, diffs, proposes; commits/pushes only on
  explicit instruction.
- **Respect the project** — follow existing repo style; no drive-by
  comments/docs/refactors outside the task sheet.

## Modes (one engine, a policy dial)

A mode is a policy tuple: planning depth, verification cadence, creativity
temperature, autonomy level, explanation register.

| Mode | For | Planning | Verify cadence | Character |
| --- | --- | --- | --- | --- |
| Prototype | vibe coders, spikes | terse inline plan | at milestones + at done | lean, creative, opinionated defaults, ships fast |
| Engineer (default) | day-to-day dev | waved task list | after every task | balanced rigor |
| Architect | experienced users, long horizon | durable spec files (EARS reqs, design, waved tasks) persisted in the workspace | per task + property tests | multi-session, resumable |

The explanation register adapts with the mode: vibe coders get outcome
language ("your site now has login"), engineers get technical detail — the
result-not-protocol rule applied to code.

Per-role model policy: the orchestrator (planning/adjudication) can pin a
stronger model; workers can run faster models in Prototype. Both route
through the `cody` gateway slot.

## System map

```
user ── apps/client coding workspace (chat + mode dial + files + diff + terminal + preview)
             │ SSE / HTTP (router, JWT)
             ▼
  per-user Railway service (Ubuntu 24.04, volume: /data + /workspace)
   ├── codyd  ── orchestrator loop ── worker subagents (one at a time)
   │              │        │              └── MCP tools: fs / exec / git / browser …
   │              │        └── acceptance gate (verify re-run + adjudication)
   │              └── cortex (checkpoints, story-so-far, project memory)
   ├── neod   (untouched)
   └── mcl daemon (untouched — Cody never routes through it)
             ▲
   matrix-router ── Provisioner{fly | railway} ── Railway GraphQL API
```

## Server shape

`codyd` runs beside Neo in the environment, following the proven pattern:
engine + session supervisor + SSE broker + durable trace, mounted behind the
router under the user's JWT. Own gateway LLM slot `cody` (types.Slot + routing
validSlot + rates whitelist — mirroring the Neo slot precedent; the rate card
itself stays Andrew-owned). `agents/cody.json` defines the tool manifest.

## Config knobs (proposed)

- `ROUTER_PROVIDER` = `fly` | `railway`; `RAILWAY_API_TOKEN`,
  `RAILWAY_PROJECT_ID`, `RAILWAY_ENVIRONMENT_ID`
- `[cody]` in the daemon kvx: `default_mode`, `worker_model`,
  `orchestrator_model`, `max_redispatch` (default 3), `verify_timeout`,
  `checkpoint_every` (default: every accepted task)
- `CODY_ENABLED` env gate for staged rollout

## Migration & retro-compatibility

- Fly implementation stays intact and selectable; existing users keep working
  during the Railway rollout. Cutover is per-deployment config, user-driven.
- Neo is not modified. The MCL walk, Liaison boundary, and wallet seam are
  untouched.
- `agents/neo.json` is untouched; `agents/cody.json` is additive.

## Non-goals (v1)

- No parallel workers / concurrent waves (sequential by design).
- No Neo modifications; no Neo→Cody handoff yet.
- No local-machine execution; the environment is the machine.
- No new MCP bridges; the deployed surface suffices.
- No CLI front (the engine is remote; a thin CLI attaches later for free).
- No spend/wallet capability of any kind in Cody.

## Verification strategy (no fakes)

- **Provisioner** — real interface conformance tests against recorded Railway
  GraphQL fixtures via httptest (same pattern as `fly_test.go`); the Fly impl
  keeps its existing tests green.
- **Wake-on-request** — proxy integration test: a slept-service simulation
  where the first forwarded request triggers readiness probing and succeeds
  within the wake budget.
- **Task sheet / turn-in round-trip** — real orchestrator authors a sheet for
  a seeded repo task; a real worker executes it against a real workspace
  (tempdir git repo); the report carries real verification output.
- **Acceptance gate** — a worker turn-in claiming success with red
  verification is REJECTED (proves the gate is structural, not trusting);
  a green turn-in is accepted and checkpointed to real cortex.
- **Re-dispatch policy** — a deterministic failure stops-and-asks after
  classification; a transient failure re-dispatches bounded by
  `max_redispatch`.
- **Constitution** — property tests: task-done unreachable with failing
  verify; tests deleted/weakened by a worker cause rejection; oversized tool
  output forces the read-full path.
- **Context economy** — after N accepted tasks, the orchestrator window
  contains reports + plan only (no worker transcripts) — asserted on the real
  message window.
- **Durability** — kill codyd mid-plan; a fresh process resumes from cortex
  checkpoints at the correct next task.
- **Modes** — the same seeded task under Prototype vs Architect yields the
  policy differences (plan depth, verify cadence) while both hold the
  constitution.
- **Client surface** — vitest reducer tests for the coding-workspace event
  families, mirroring the NeoTask pattern; UI house rules (no border-stroke
  depth, no emojis/gradients/glow).
