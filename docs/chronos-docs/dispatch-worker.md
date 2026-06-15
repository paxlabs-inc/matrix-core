# Dispatch Worker

Package `chronos/internal/dispatch` is the durable poll-and-claim worker that fires due alarms. Single instance now, HA-ready by construction — the DB claim uses `FOR UPDATE SKIP LOCKED` plus a lease, so adding workers never double-fires.

Source file: `internal/dispatch/dispatch.go`.

---

## Design decisions

**The DB is the source of truth; the ticker is just a heartbeat.** The worker polls on a short interval (default 1s) but the claim query is what actually determines which alarms are due. There is no in-memory timer heap.

**At-least-once delivery.** A fire is recorded only AFTER the router confirms the wake. A crash mid-fire leaves the lease, which expires and is reclaimed — the alarm will fire again.

**HA by construction.** `FOR UPDATE SKIP LOCKED` means concurrent workers (now or future) never claim the same row. The lease (`claimed_at`) prevents a crashed worker from permanently locking rows.

---

## The poll loop

```
┌──────────────────┐
│  ticker fires    │  Every Tick (default 1s)
└────────┬─────────┘
         ▼
┌──────────────────┐
│  ClaimDue        │  SELECT … FOR UPDATE SKIP LOCKED
│  (batch, lease)  │  Claims up to `batch` due alarms
└────────┬─────────┘
         ▼
┌──────────────────┐
│  for each alarm  │
│  fire(alarm)     │
└────────┬─────────┘
         ▼
┌──────────────────┐
│  waker.Wake()    │  POST /internal/wake to router
└────────┬─────────┘
    ┌────┴────┐
    ▼         ▼
 success    failure
    │         │
    ▼         ▼
onSuccess  onFailure
```

---

## Claim query

```sql
UPDATE alarms SET claimed_at = now(), updated_at = now()
WHERE id IN (
    SELECT id FROM alarms
    WHERE status = 'active'
      AND next_fire_at <= now()
      AND (claimed_at IS NULL OR claimed_at < now() - ($1 * interval '1 second'))
    ORDER BY next_fire_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
RETURNING …
```

Key properties:
- Only `active` alarms with `next_fire_at` in the past are claimed
- The lease check (`claimed_at < now() - lease`) prevents re-claiming an alarm that another worker is currently delivering
- `FOR UPDATE SKIP LOCKED` is the HA primitive — no two workers can claim the same row
- `ORDER BY next_fire_at` ensures oldest-due fires first

---

## Fire outcomes

### Success path

```
fire success
    │
    ├── kind=once ──► MarkFired (status='fired', retained for audit)
    │
    └── kind=cron ──► NextCron(expr, tz, now)
                      │
                      ├── valid next ──► Reschedule (advance next_fire_at)
                      │
                      └── no future ──► MarkFired (retire the series)
```

### Failure path — the retry ladder

```
fire failure
    │
    ├── failure_count < max_failures
    │       │
    │       └── RecordRetry: backoff(next_fire_at = now + backoff(failures))
    │           The alarm stays active; the worker will claim it again after the backoff.
    │
    └── failure_count >= max_failures
            │
            ├── kind=once ──► MarkFailed (status='failed', last_error recorded)
            │                 Terminal. Never retried.
            │
            └── kind=cron ──► RescheduleAfterFailure (skip-and-advance)
                              Advances next_fire_at past the bad fire so one
                              failure does not wedge the whole series.
```

---

## Backoff

Exponential backoff starting at 30s, doubling each failure, capped at 15 minutes:

```go
func backoff(failureCount int) time.Duration {
    d := 30 * time.Second
    for i := 0; i < failureCount; i++ {
        d *= 2
        if d >= 15 * time.Minute {
            return 15 * time.Minute
        }
    }
    return d
}
```

| Failure count | Backoff |
|---|---|
| 0 (first retry) | 30s |
| 1 | 1m |
| 2 | 2m |
| 3 | 4m |
| 4 | 8m |
| 5+ | 15m (capped) |

---

## Config tunables

| Config key | Default | Meaning |
|---|---|---|
| `Tick` | 1s | Poll interval |
| `Lease` | 2m | How long a claimed alarm stays leased before another worker may reclaim it |
| `Batch` | 100 | Max alarms claimed per tick |
| `MaxFailures` | 5 | Default wake-delivery retry ceiling per alarm |

These are set via `dispatch.Config` and resolved from environment variables or the kvx overlay (see [Config System](./config-system.md)).

---

## Worker lifecycle

```go
func (w *Worker) Run(ctx context.Context)
```

Blocks until `ctx` is cancelled. The ticker is the only goroutine — all work is synchronous within each tick. A `ctx.Err() != nil` check between alarms in a batch allows clean shutdown mid-batch.

The worker is spawned as a goroutine in `main()`:

```go
worker := dispatch.New(st, waker, log, dispatch.Config{…})
go worker.Run(ctx)
```

---

## Honest failure (invariant i6)

Every failure path records `last_error` on the alarm row. A permanently failed once-alarm has `status='failed'` with the error text. A cron alarm that skips a fire records the error before advancing. Nothing is silently dropped.
