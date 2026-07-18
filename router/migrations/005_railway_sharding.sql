BEGIN;

CREATE TABLE IF NOT EXISTS railway_shards (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    router_url TEXT NOT NULL,
    capacity INTEGER NOT NULL CHECK (capacity > 0),
    state TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active','draining','disabled','unhealthy')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, environment_id)
);

ALTER TABLE users ADD COLUMN IF NOT EXISTS railway_shard_id TEXT
    REFERENCES railway_shards(id);

CREATE TABLE IF NOT EXISTS railway_allocations (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    shard_id TEXT NOT NULL REFERENCES railway_shards(id),
    state TEXT NOT NULL
        CHECK (state IN ('reserved','provisioning','active','cleanup_pending','failed','released')),
    operation_key TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    CHECK ((state = 'released') = (released_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_railway_allocations_capacity
    ON railway_allocations(shard_id, state)
    WHERE state <> 'released';

CREATE TABLE IF NOT EXISTS railway_operations (
    id BIGSERIAL PRIMARY KEY,
    operation_key TEXT NOT NULL UNIQUE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    shard_id TEXT NOT NULL REFERENCES railway_shards(id),
    project_id TEXT NOT NULL,
    environment_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('ensure','status','wake','destroy','attach_volume','reconcile')),
    state TEXT NOT NULL CHECK (state IN ('intent','running','unknown','cleanup_pending','succeeded','failed')),
    service_id TEXT,
    volume_id TEXT,
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reconciled_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_railway_operations_reconcile
    ON railway_operations(state, updated_at)
    WHERE state IN ('intent','running','unknown','cleanup_pending');

CREATE TABLE IF NOT EXISTS railway_ingress_replays (
    shard_id TEXT NOT NULL REFERENCES railway_shards(id) ON DELETE CASCADE,
    replay_id TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (shard_id, replay_id)
);

INSERT INTO railway_shards(id, project_id, environment_id, router_url, capacity, state)
SELECT 'shard-0',
       current_setting('matrix.railway_project_id', true),
       current_setting('matrix.railway_environment_id', true),
       current_setting('matrix.railway_router_url', true),
       COALESCE(NULLIF(current_setting('matrix.railway_shard_capacity', true), '')::integer, 20),
       'active'
WHERE current_setting('matrix.railway_project_id', true) <> ''
  AND current_setting('matrix.railway_environment_id', true) <> ''
  AND current_setting('matrix.railway_router_url', true) <> ''
ON CONFLICT (id) DO NOTHING;

UPDATE users
SET railway_shard_id = 'shard-0', updated_at = now()
WHERE provider = 'railway'
  AND railway_shard_id IS NULL
  AND EXISTS (SELECT 1 FROM railway_shards WHERE id = 'shard-0');

INSERT INTO railway_allocations(user_id, shard_id, state, operation_key)
SELECT id, railway_shard_id,
       CASE WHEN state = 'active' THEN 'active' ELSE 'provisioning' END,
       'backfill:' || id::text
FROM users
WHERE provider = 'railway' AND railway_shard_id IS NOT NULL
ON CONFLICT (user_id) DO NOTHING;

COMMIT;
