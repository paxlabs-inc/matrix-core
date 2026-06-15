# Schedule Engine

Package `chronos/internal/schedule` computes alarm fire times for the two kinds: `once` (a relative delay or an absolute instant) and `cron` (a standard 5-field expression, `@descriptor`, or `@every Nm`, evaluated in the alarm's IANA timezone).

Source files: `internal/schedule/schedule.go`, `internal/schedule/schedule_test.go`.

---

## The two kinds

The three user cases collapse onto two kinds:

| User says | Kind | Mechanism |
|---|---|---|
| "in 10 minutes" | `once` | `delay_seconds` relative to `now` |
| "at 3pm tomorrow" | `once` | `fire_at` absolute RFC3339 timestamp |
| "every day at 9am" | `cron` | `cron_expr` parsed by `robfig/cron/v3` |

---

## Once alarms

`NextOnce(delaySeconds int64, fireAt string, now time.Time) (time.Time, error)`

Resolves the single fire instant from exactly one of:

- **`delay_seconds`** — relative to `now`. The resolved time is `now + delay_seconds`, in UTC.
- **`fire_at`** — an absolute RFC3339 timestamp. Must be strictly in the future.

Validation rules:

| Condition | Error |
|---|---|
| Both `delay_seconds` and `fire_at` set | `"once alarm takes either delay_seconds or fire_at, not both"` |
| Neither set | `"once alarm requires delay_seconds or fire_at"` |
| `fire_at` is not valid RFC3339 | `"invalid fire_at … (want RFC3339)"` |
| `fire_at` is in the past | `"fire_at … is not in the future"` |

```go
now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// Relative: 10 minutes from now
t, _ := schedule.NextOnce(600, "", now)
// t = 2026-01-01T12:10:00Z

// Absolute: specific time
t, _ := schedule.NextOnce(0, "2026-01-01T18:30:00Z", now)
// t = 2026-01-01T18:30:00Z
```

---

## Cron alarms

`NextCron(expr, tz string, after time.Time) (time.Time, error)`

Parses the expression with `robfig/cron/v3`, evaluates the next fire time strictly after `after` in the given IANA timezone, and returns it in UTC.

### Supported expression syntax

The parser accepts the standard 5-field syntax plus `@descriptors`:

```
cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor
```

| Expression | Meaning |
|---|---|
| `0 9 * * *` | Every day at 09:00 |
| `*/15 * * * *` | Every 15 minutes |
| `0 9 * * 1-5` | Weekdays at 09:00 |
| `@hourly` | At the start of every hour |
| `@daily` | At midnight every day |
| `@every 10m` | Every 10 minutes |

### Timezone handling

Cron expressions evaluate in the alarm's IANA timezone. Once alarms are absolute instants (timezone-independent).

```go
now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// 09:00 New York on Jan 1 = 14:00 UTC (EST, UTC-5)
t, _ := schedule.NextCron("0 9 * * *", "America/New_York", now)
// t = 2026-01-01T14:00:00Z
```

`LoadLocation(tz string)` resolves the IANA timezone, defaulting to `time.UTC` when empty. Invalid timezone names return an error.

### Edge cases

- **Empty expression** → error: `"empty cron expression"`
- **Invalid expression** → error: `"invalid cron expression …"`
- **No future time** (e.g. a cron that only matches dates in the past) → error: `"cron … yields no future time"`. The dispatch worker treats this as terminal and retires the alarm.

---

## Validation at create time

Both `NextOnce` and `NextCron` are called at alarm creation time in `server.handleCreateAlarm`. The computed `next_fire_at` is stored in the row. The dispatch worker never re-parses expressions — it only compares `next_fire_at <= now()`.

---

## Rescheduling after fire

For cron alarms, after a successful fire the dispatch worker calls `NextCron` again with `now` to compute the next occurrence:

```go
next, err := schedule.NextCron(a.CronExpr, a.Timezone, now)
store.Reschedule(ctx, a.ID, next)
```

This means the cron schedule is re-evaluated fresh after every fire. If the expression becomes invalid (e.g. a timezone is removed from the IANA database), the alarm is retired rather than wedged.

---

## Dependencies

- `github.com/robfig/cron/v3` — the standard Go cron library. Pinned at `v3.0.1` in `go.mod`.
- No other external dependencies. Timezone resolution uses the Go standard library's `time.LoadLocation`.
