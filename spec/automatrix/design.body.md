# Design — Automatrix (Neo proactive surprise tasks)

## Overview

**Automatrix** lets Neo work *unprompted*. During normal conversation Neo quietly notices
things the user **mentioned needing/wanting** or that would **clearly benefit** them but never
explicitly asked Neo to do. It captures these as durable **opportunities**. Then, while the user
is **idle** (the per-user daemon has scaled to zero between requests), Neo **randomly wakes**,
picks one eligible opportunity, **does the work autonomously**, and **pings the user** with the
finished result — a pleasant surprise rather than a request fulfilled.

The feature is mostly **additive wiring over substrate that already exists**:

- **Capture** rides the existing background consolidation pass
  (`neo/internal/writeback/consolidator.go`), which already sweeps every turn with a cheap model
  and writes durable learnings to cortex. Automatrix adds one more extraction class:
  `opportunities`.
- **Idle wake** rides **Chronos** (`/root/matrix/chronos`), the centralized, durable agent
  alarm-clock that already wakes a scaled-to-zero daemon by injecting a context-rich `/chat`
  turn via the router's `POST /internal/wake`. Automatrix is a sibling of the existing
  **heartbeat convention** (`chronos/internal/heartbeat` ↔ `neo/internal/agent/heartbeat.go`):
  a self-rescheduling alarm carrying an `AUTOMATRIX` marker instead of `HEARTBEAT`.
- **Execution** rides the existing supervised task engine
  (`neo/internal/server/session.go` `drive`/`superviseTask`), which is already decoupled from the
  user's connection and durable across restart (the Task Durability Rule).
- **Notify** is the one genuinely new outbound seam: a small pluggable `Notifier` (default
  **ntfy**, optional **Apprise** fan-out) plus a durable in-app `automatrix.complete` record.

## Locked decisions

- **Autonomy boundary = full non-financial.** The *only* hard exclusion is **financial / on-chain**
  work. This is enforced structurally, not by a denylist of intents: an Automatrix run executes on a
  **restricted tool surface that omits the money/chain `core_execute` path entirely**, so it
  *physically cannot* spend or sign. This respects frozen invariant **i1** (Neo never holds a
  signing key; all value-moving work crosses into MCL via `core_execute`) and Chronos **i5**
  (off-chain only; deferred money escalates to MCL at fire time).
- **Rollout = opt-in.** Ships behind a Settings toggle, **default OFF**. **Capture still runs
  silently** whenever the feature is built in, so the opportunity queue is warm the moment a user
  opts in — but Neo **never wakes to work unprompted until the user enables it**.
- **Notify = ntfy/Apprise + durable in-app record.** ntfy (`POST ${NTFY_SERVER}/${topic}`,
  per-user topic derived from the agent DID) is the default out-of-app ping; Apprise
  (`APPRISE_URL` or the `apprise` CLI) is an optional fan-out backend (Telegram, Discord, email,
  Pushover, …). Regardless of external delivery, a durable `automatrix.complete` record is always
  written so the result is canonical and replayable in-app.
- **Honesty.** An Automatrix turn is held to the **same Cassandra completion gate** as any other
  state-touching turn. Neo never pings "I did X" unless the work genuinely passed the gate; a
  partial or failed attempt is surfaced honestly (or silently re-queued), never fabricated.

## System map

```
                       ┌─────────────────────────── normal conversation ───────────────────────────┐
   user turn ──▶ Neo agent loop ──▶ (answer)                                                          │
                       │                                                                              │
                       └──▶ writeback.Consolidator (cheap model, every turn)                          │
                              ├─ facts / user_facts / preferences / corrections / patterns / outcome  │
                              └─ NEW: opportunities ──▶ cortex Opportunity records (deduped, ranked)  │
                                                          status: pending | scheduled | in_progress    │
                                                                  | done | dismissed                   │
                                                          eligible_autonomous: bool (non-financial)    │
                       ┌──────────────────────────────────────────────────────────────────────────────┘
   opt-in ON  ──▶ create recurring AUTOMATRIX alarm (Chronos)         opt-in OFF ──▶ cancel alarm
                       │
   (daemon idle / scaled to zero)                                            ┌── ntfy  ─▶ phone/desktop
                       │                                                     │
   Chronos fires ─▶ router POST /internal/wake ─▶ daemon /chat ─▶ Neo engine │── Apprise fan-out
       (jittered)        (EnsureStarted + 6PN)      (origin=chronos,         │
                                                     marker=AUTOMATRIX)      └── durable automatrix.complete
                       │                                                          (in-app inbox + unread badge)
                       ▼
        engine: opt-in still on?  busy?  pick one eligible opportunity
                       │  (re-schedule with jitter if busy / opt-out / queue empty)
                       ▼
        session.drive ─▶ superviseTask ─▶ agent.Chat on RESTRICTED tool surface
                                              (no money/chain core_execute)
                       │
                       ▼ (Cassandra-gated genuine completion)
        notify(user) + write automatrix.complete + mark opportunity done
```

## Capture — the `opportunities` extraction class

`writeback.Consolidator.process` already decodes a strict-JSON `extract` struct and fans each field
into a `pager.Remember*` call. Automatrix adds an `opportunities` array to `consolidatePrompt` and
the `extract` struct:

```jsonc
"opportunities": [
  {
    "summary": "short imperative — what would help the user",      // "Draft the quarterly update doc you mentioned"
    "rationale": "why this benefits the user / where they said it", // grounding, anti-hallucination
    "financial": false,                                            // model's first-pass class; re-checked deterministically
    "confidence": 0.0                                              // 0..1; low-confidence is dropped
  }
]
```

Prompt rules (mirroring the existing selective discipline — *"usually `[]`"*):

- An opportunity is something the user **implied wanting or would benefit from** but **did NOT ask
  Neo to do this turn**. A direct request is *not* an opportunity (it is already being handled).
- It must be **specific and actionable**, grounded in something the user actually said/did
  (`rationale` must cite it). Vague "be helpful" items are dropped.
- `financial: true` for anything involving spending, sending value, trading, or on-chain writes.
- Usually `[]`. Most turns yield no opportunity, and that is the correct, common answer.

Each accepted opportunity (above a confidence floor, capped per turn like the other classes) is
written to cortex via a new `pager.RememberOpportunity`, **deduped** against existing pending
opportunities (same normalize-and-cosine discipline as `RememberFact`) so a recurring mention does
not pile up duplicates.

## Opportunity record + queue (cortex)

A new cortex memory shape (reusing the cortex `Goal`/typed-record machinery; see
`neo/internal/memory`) with:

| field | meaning |
|---|---|
| `summary` | the actionable task (imperative, self-sufficient) |
| `rationale` | grounding — where/why it arose |
| `status` | `pending` → `scheduled` → `in_progress` → `done` \| `dismissed` |
| `eligible_autonomous` | `true` only if non-financial (the wake-eligibility flag) |
| `confidence` | extraction confidence (ranking input) |
| `origin_conversation_id` | the thread it arose in (the wake resumes here for context) |
| `created_at` / `updated_at` / `attempts` | lifecycle + retry bookkeeping |

Pager surface (new, in `neo/internal/memory/writeback.go` + a small reader):
`RememberOpportunity(ctx, spec)` (dedup-or-write), `PendingOpportunities(ctx, limit)`
(ranked: `eligible_autonomous && status=pending`, by confidence × salience × recency),
`SetOpportunityStatus(ctx, uri, status)`.

## Non-financial gating (defense in depth)

Three independent layers, any one of which is sufficient to keep money out:

1. **Classification at capture.** `financial=true` ⇒ `eligible_autonomous=false`. A re-check runs
   deterministically on the summary (keyword + the model's flag) so a missed flag is caught.
   Financial opportunities are still captured and **surfaced for explicit user approval**, never
   auto-run.
2. **Selection.** `PendingOpportunities` only ever returns `eligible_autonomous` items, so the
   autonomous picker can never select a financial opportunity.
3. **Restricted tool surface (the structural guarantee).** The woken run executes with a tool
   surface built by the existing sub-agent `RestrictTools` mechanism that **excludes the
   money/chain `core_execute` delegate**. Even a mis-classified item or a model that tries to spend
   **cannot reach the signing path** — the tool simply isn't there. This is the load-bearing
   guarantee; layers 1–2 are about *quality of selection*, layer 3 is about *safety*.

## Idle wake — the AUTOMATRIX Chronos alarm

A sibling of `neo/internal/agent/heartbeat.go`, in a new `automatrix.go` next to it:

```go
const AutomatrixWakeMarker  = "AUTOMATRIX"
const AutomatrixWakeMessage = "AUTOMATRIX: you have idle time. Review your pending non-financial " +
    "opportunities and, if a worthwhile one exists, pick ONE and complete it end-to-end, then " +
    "notify the user with the result. If nothing is worth doing right now, reply with exactly AUTOMATRIX_IDLE."
const AutomatrixIdle = "AUTOMATRIX_IDLE"   // sentinel → suppress the turn (like HEARTBEAT_OK)
```

- **Scheduling.** When a user opts in, the daemon creates **one recurring** Automatrix alarm via
  the Chronos MCP tool (`alarm_set`). To make wakes feel **random** rather than clockwork, the alarm
  fires on a base cadence and the engine, on each fire, **reschedules the next one with randomized
  jitter** within a configured window (e.g. base `@every 45m` ± jitter), and may **skip** a fire
  probabilistically. (Chronos cron is the durable backbone; the jitter/skip lives in Neo so the DB
  stays the source of truth.) A mirror of `chronos/internal/heartbeat` — `chronos/internal/automatrix`
  — provides the canonical marker + a `BuildAlarm` helper.
- **Busy-check.** On wake the engine checks whether a run is already in flight for the user
  (`session.active`). If busy, it **does nothing and reschedules** — Automatrix never competes with
  or interrupts real user work (it folds in like F5 would, but the cleaner behavior is to defer).
- **Opt-in re-check.** The engine re-reads the opt-in setting on every wake (defense in depth); if
  the user opted out since the alarm was created, it cancels the alarm and returns.
- **Empty queue.** No eligible opportunity ⇒ reply `AUTOMATRIX_IDLE` ⇒ suppressed turn, reschedule.

Detection mirrors `isHeartbeatWake`: `isAutomatrixWake(input)` checks for the `AUTOMATRIX` marker
on the injected wake turn (Chronos tags the delivered turn `origin=chronos, alarm_id`).

## Execution

The wake handler (engine, on recognizing an Automatrix wake) does **not** just feed the marker to a
normal turn. It:

1. Re-checks opt-in + busy + picks the single highest-ranked eligible opportunity
   (`SetOpportunityStatus(scheduled→in_progress)`).
2. Builds the working prompt from the opportunity's `summary` + `rationale` and resumes into its
   `origin_conversation_id` so Neo regains the full thread context + cortex memory (Chronos context
   fidelity, i4).
3. Dispatches a supervised `session.drive` run **on the restricted (no-money) tool surface**,
   bounded by the normal task wall-clock + step budget.
4. The run is subject to the **same Cassandra completion gate** as any state-touching turn — Neo
   must genuinely finish (or honestly report a partial) before it may notify.

On genuine completion: notify + write the durable record + `SetOpportunityStatus(done)`. On
partial/failure: leave the opportunity `pending` (bounded `attempts`), surface nothing to the user
(no failed-surprise spam), and let a later wake retry or eventually dismiss it.

## Notify — pluggable `Notifier`

A small interface in a new `neo/internal/notify` package:

```go
type Notification struct { Title, Body, URL string }
type Notifier interface { Notify(ctx context.Context, n Notification) error }
```

Backends:

- **ntfy** (default): `POST ${NTFY_SERVER}/${topic}` with `Title`/`Click` headers. Topic is derived
  deterministically from the agent DID (so it is per-user and unguessable) or set explicitly via
  config. No key management, works out-of-app, has first-party mobile/desktop apps.
- **Apprise** (optional fan-out): either an Apprise API server (`APPRISE_URL`) or the local
  `apprise` CLI, letting the user route the ping to Telegram/Discord/email/Pushover/etc. with one
  config string.
- **noop** when nothing is configured (the durable in-app record still happens).

Delivery is **best-effort and never blocks** the agent; a failed external send is logged and the
in-app record remains the canonical surface (honest-failure: we never claim a ping we could not
send, but the result itself is never lost).

### Durable in-app record

A new `automatrix.complete` broker event + a small durable sidecar (mirroring the trace store at
`neo/internal/trace`) records `{opportunity_summary, result_summary, conversation_id, created_at,
read}`. The client renders an **unread Automatrix inbox / badge** and, on open, the completed work
appears as an assistant turn in the relevant conversation. This obeys the consumer rule: **show the
result, not the protocol** — no mention of Chronos/alarms/markers in the UI.

## Control surface (opt-in + management)

- **Setting.** `automatrix_enabled` (default `false`) stored in the per-user daemon settings
  (alongside the existing `/settings` + onboarding profile). Toggling ON creates the Chronos alarm;
  toggling OFF cancels it. The engine treats the setting as authoritative on every wake.
- **Client.** A **Settings → Automatrix** section: the master toggle, a plain-language explanation
  ("Neo will occasionally work on helpful things you mentioned and let you know when it's done —
  never anything that spends money"), and a **queue view** of pending opportunities where the user
  can **dismiss** an item or **approve a financial one** for a normal (gated) run. Plus the
  completion **inbox/badge**. UI follows house rules (separation by background contrast, no borders
  for depth, no emojis/gradients/glow).

## Configuration (new Neo knobs)

| key (kvx `[automatrix]` / env) | default | meaning |
|---|---|---|
| `enabled` / `NEO_AUTOMATRIX_ENABLED` | `false` | build-level master (per-user opt-in still required) |
| `base_interval_minutes` / `NEO_AUTOMATRIX_INTERVAL` | `45` | base wake cadence (0 = disabled) |
| `jitter_minutes` / `NEO_AUTOMATRIX_JITTER` | `30` | ± randomization window for the "random" feel |
| `max_tasks_per_day` / `NEO_AUTOMATRIX_MAX_PER_DAY` | `3` | cap on proactive tasks/day (anti-spam) |
| `min_confidence` / `NEO_AUTOMATRIX_MIN_CONFIDENCE` | `0.6` | capture floor for an opportunity |
| `NTFY_SERVER` / `NTFY_TOPIC` | `https://ntfy.sh` / DID-derived | ntfy delivery |
| `APPRISE_URL` | unset | optional Apprise fan-out |

## What this feature does NOT do (non-goals)

- **No money. Ever.** No financial/on-chain action runs autonomously; the restricted surface omits
  the `core_execute` money path. Financial opportunities are only ever *surfaced for explicit
  approval*.
- **No change to the signed MCL walk** (compile/plan/walk/synthesize/critic/emitFinalTurn, D11
  byte-identity) and **no new Chronos signing/cortex/plan-walk capability** (Chronos i7 holds —
  it only delivers a `/chat` turn; Neo does the work).
- **No interrupting real work.** Automatrix defers (reschedules) whenever the user is active.
- **No silent default-on.** Opt-in, default OFF.

## Verification strategy (no fakes)

Every property is proven against **real code paths**:

- Capture: feed a real transcript through the real consolidator extraction and assert a real
  `Opportunity` cortex record is written (and a non-opportunity transcript writes none).
- Gating: a `financial=true` opportunity is never returned by `PendingOpportunities`; the restricted
  tool surface really lacks the money `core_execute` tool (assert the schema set).
- Wake: `isAutomatrixWake` detects the real marker; `AUTOMATRIX_IDLE` suppresses the turn; a busy
  session defers.
- Notify: a real ntfy backend round-trips against an `httptest` server; the durable record is
  written and marked unread.
- Opt-in: alarm is created on enable and cancelled on disable; the engine refuses to run when the
  setting is off.
- No test substitutes a stub/mock/fake for a real code path to manufacture a pass.
