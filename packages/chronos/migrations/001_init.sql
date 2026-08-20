-- 001_init.sql — Chronos alarms schema (chronos.frozen.kvx [data_model]).
-- Forward-only, idempotent (CREATE IF NOT EXISTS). Safe to re-run. Chronos
-- shares the matrix DB with router + gateway; every object is chronos-scoped.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- One row per scheduled wake; the row IS the durable timer (invariant i1).
CREATE TABLE IF NOT EXISTS alarms (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_did        TEXT NOT NULL,                       -- did:matrix:<user_id>:<keyfp>
    user_id          TEXT NOT NULL,                       -- Supabase user UUID = router wake target
    label            TEXT NOT NULL DEFAULT '',
    kind             TEXT NOT NULL CHECK (kind IN ('once', 'cron')),
    fire_at          TIMESTAMPTZ,                          -- once: absolute moment to fire
    cron_expr        TEXT NOT NULL DEFAULT '',             -- cron: 5-field / @descriptor / @every
    timezone         TEXT NOT NULL DEFAULT 'UTC',          -- IANA tz for cron evaluation
    next_fire_at     TIMESTAMPTZ NOT NULL,                 -- dispatch claim key
    conversation_id  TEXT NOT NULL DEFAULT '',             -- conversation to resume into ('' = fresh)
    wake_message     TEXT NOT NULL DEFAULT '',             -- agent-authored resume turn (verbatim)
    payload          JSONB NOT NULL DEFAULT '{}'::jsonb,   -- opaque state echoed back on wake
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active', 'fired', 'cancelled', 'failed')),
    idempotency_key  TEXT NOT NULL DEFAULT '',             -- per-owner dedup key ('' = none)
    max_failures     INT  NOT NULL DEFAULT 5,              -- wake-delivery retry ceiling
    failure_count    INT  NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    claimed_at       TIMESTAMPTZ,                          -- dispatch lease (NULL = unclaimed)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_fired_at    TIMESTAMPTZ
);

-- The dispatch worker's hot claim path: due, active alarms ordered by next_fire_at.
CREATE INDEX IF NOT EXISTS alarms_due_idx
    ON alarms (next_fire_at)
    WHERE status = 'active';

-- Owner-scoped listing.
CREATE INDEX IF NOT EXISTS alarms_owner_idx
    ON alarms (owner_did, created_at DESC);

-- Per-owner idempotency: re-posting the same key is a no-op, not a duplicate.
CREATE UNIQUE INDEX IF NOT EXISTS alarms_idempotency_idx
    ON alarms (owner_did, idempotency_key)
    WHERE idempotency_key <> '';

COMMIT;
