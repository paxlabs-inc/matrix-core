# Data Model

Package `chronos/pkg/types` defines the wire contracts and `chronos/internal/store` implements the Postgres persistence. The `alarms` table IS the durable timer — state lives in the DB, never in memory, so a restart never loses a scheduled wake (invariant i1).

Source files: `pkg/types/types.go`, `internal/store/alarms.go`, `migrations/001_init.sql`.

---

## The alarms table

One row per scheduled wake. The row itself is the timer — the dispatch worker polls `next_fire_at`, not an in-memory heap.

```sql
CREATE TABLE alarms (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_did        TEXT NOT NULL,
    user_id          TEXT NOT NULL,
    label            TEXT NOT NULL DEFAULT '',
    kind             TEXT NOT NULL CHECK (kind IN ('once', 'cron')),
    fire_at          TIMESTAMPTZ,
    cron_expr        TEXT NOT NULL DEFAULT '',
    timezone         TEXT NOT NULL DEFAULT 'UTC',
    next_fire_at     TIMESTAMPTZ NOT NULL,
    conversation_id  TEXT NOT NULL DEFAULT '',
    wake_message     TEXT NOT NULL DEFAULT '',
    payload          JSONB NOT NULL DEFAULT '{}'::jsonb,
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active', 'fired', 'cancelled', 'failed')),
    idempotency_key  TEXT NOT NULL DEFAULT '',
    max_failures     INT  NOT NULL DEFAULT 5,
    failure_count    INT  NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    claimed_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_fired_at    TIMESTAMPTZ
);
```

### Indexes

| Index | Definition | Purpose |
|---|---|---|
| `alarms_due_idx` | `next_fire_at` WHERE `status = 'active'` | The dispatch worker's hot claim path |
| `alarms_owner_idx` | `owner_did, created_at DESC` | Owner-scoped listing |
| `alarms_idempotency_idx` | `owner_did, idempotency_key` WHERE `idempotency_key <> ''` | Per-owner dedup — re-posting the same key is a no-op |

---

## Column reference

| Column | Type | Meaning |
|---|---|---|
| `id` | UUID | Primary key, auto-generated |
| `owner_did` | TEXT | Full agent DID: `did:matrix:<user_id>:<keyfp>` |
| `user_id` | TEXT | Supabase user UUID extracted from the DID label — the router wake target |
| `label` | TEXT | Short human label, e.g. "daily portfolio summary" |
| `kind` | TEXT | `once` or `cron` |
| `fire_at` | TIMESTAMPTZ | For `once`: the absolute moment to fire (computed from delay or explicit time at post) |
| `cron_expr` | TEXT | For `cron`: standard 5-field expression, `@descriptor`, or `@every Nm` |
| `timezone` | TEXT | IANA timezone for cron evaluation, default `"UTC"` |
| `next_fire_at` | TIMESTAMPTZ | The next scheduled fire; the dispatch worker's claim key |
| `conversation_id` | TEXT | Conversation to resume into; empty → agent opens a fresh conversation on wake |
| `wake_message` | TEXT | Agent-authored resume text delivered as the chat turn |
| `payload` | JSONB | Opaque state the agent stashed (task state, ids, cursors) — echoed back verbatim on wake |
| `status` | TEXT | Lifecycle: `active`, `fired`, `cancelled`, or `failed` |
| `idempotency_key` | TEXT | Optional per-owner dedup key; re-posting the same key returns the existing alarm |
| `max_failures` | INT | Wake-delivery retry ceiling before `status=failed` (default from config) |
| `failure_count` | INT | Running count of failed wake deliveries |
| `last_error` | TEXT | Last wake-delivery error text (honest failure surfacing) |
| `claimed_at` | TIMESTAMPTZ | Dispatch lease timestamp; NULL = unclaimed |
| `created_at` | TIMESTAMPTZ | Row creation time |
| `updated_at` | TIMESTAMPTZ | Last mutation time |
| `last_fired_at` | TIMESTAMPTZ | Last successful fire (observability / time-tracking) |

---

## Go types

### Alarm

`Alarm` is the internal representation, mapping 1:1 onto a row:

```go
type Alarm struct {
    ID             string
    OwnerDID       string
    UserID         string
    Label          string
    Kind           string         // "once" | "cron"
    FireAt         *time.Time
    CronExpr       string
    Timezone       string
    NextFireAt     time.Time
    ConversationID string
    WakeMessage    string
    Payload        json.RawMessage
    Status         string
    IdempotencyKey string
    MaxFailures    int
    FailureCount   int
    LastError      string
    ClaimedAt      *time.Time
    CreatedAt      time.Time
    UpdatedAt      time.Time
    LastFiredAt    *time.Time
}
```

### View

`View` is the JSON projection returned to the agent. High-entropy fields (`payload`, `wake_message`, ids) are passed through verbatim (invariant i4):

```go
type View struct {
    ID             string          `json:"id"`
    Label          string          `json:"label"`
    Kind           string          `json:"kind"`
    CronExpr       string          `json:"cron_expr,omitempty"`
    Timezone       string          `json:"timezone,omitempty"`
    NextFireAt     *time.Time      `json:"next_fire_at,omitempty"`
    ConversationID string          `json:"conversation_id,omitempty"`
    WakeMessage    string          `json:"wake_message"`
    Payload        json.RawMessage `json:"payload,omitempty"`
    Status         string          `json:"status"`
    IdempotencyKey string          `json:"idempotency_key,omitempty"`
    MaxFailures    int             `json:"max_failures"`
    FailureCount   int             `json:"failure_count"`
    LastError      string          `json:"last_error,omitempty"`
    CreatedAt      time.Time       `json:"created_at"`
    LastFiredAt    *time.Time      `json:"last_fired_at,omitempty"`
}
```

`ViewOf(a Alarm)` projects an `Alarm` onto its wire shape. `NextFireAt` is only included when `status == "active"`.

---

## Lifecycle statuses

```
                  ┌─────────┐
                  │  active  │
                  └────┬─────┘
         ┌─────────────┼─────────────┐
         ▼             ▼             ▼
    ┌────────┐   ┌──────────┐   ┌────────┐
    │ fired  │   │ cancelled│   │ failed │
    └────────┘   └──────────┘   └────────┘
    (once only)  (explicit)    (once only,
                                retries
                                exhausted)
```

| Status | Meaning | Transition |
|---|---|---|
| `active` | Scheduled and waiting to fire | Initial state on create |
| `fired` | Once alarm that has fired successfully | Retained for audit, not deleted |
| `cancelled` | Explicitly cancelled by the owner | Via `DELETE /v1/alarms/{id}` |
| `failed` | Once alarm whose wake delivery exhausted `max_failures` | Terminal; never retried |

Cron alarms stay `active` indefinitely — they never self-delete. Each fire advances `next_fire_at` and the alarm remains active. Cancellation is explicit.

---

## Idempotency

When `idempotency_key` is non-empty, `CreateAlarm` uses a partial unique index:

```sql
CREATE UNIQUE INDEX alarms_idempotency_idx
    ON alarms (owner_did, idempotency_key)
    WHERE idempotency_key <> '';
```

The insert uses `ON CONFLICT (owner_did, idempotency_key) WHERE idempotency_key <> '' DO NOTHING`. If the row already exists, the existing alarm is returned with `deduped=true`. No duplicate is created.

---

## Store operations

All alarm mutations go through `store.Store` methods:

| Method | SQL | Notes |
|---|---|---|
| `CreateAlarm` | INSERT … ON CONFLICT DO NOTHING RETURNING | Idempotent; returns `(Alarm, deduped, error)` |
| `ListAlarms` | SELECT … WHERE owner_did ORDER BY created_at DESC LIMIT | Owner-scoped, capped at 500 |
| `GetAlarm` | SELECT … WHERE id AND owner_did | Returns `ErrNotFound` for unknown/unowned |
| `CancelAlarm` | UPDATE status='cancelled' WHERE id AND owner_did AND status='active' | Idempotent; already-terminal alarms return success |
| `ClaimDue` | UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED) | Atomic lease; HA-safe |
| `MarkFired` | UPDATE status='fired', last_fired_at=now() | Once alarms only |
| `Reschedule` | UPDATE next_fire_at, last_fired_at=now() | Cron alarms only |
| `RecordRetry` | UPDATE failure_count++, next_fire_at=retry_time | Bounded backoff |
| `MarkFailed` | UPDATE status='failed', last_error | Once alarms, retries exhausted |
| `RescheduleAfterFailure` | UPDATE next_fire_at, last_error | Cron skip-and-advance |
