CREATE TABLE effect_state (
    idempotency_key TEXT PRIMARY KEY,
    tool_name TEXT NOT NULL,
    status TEXT NOT NULL,
    state BLOB NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX effect_state_recovery
    ON effect_state(status, updated_at, idempotency_key);

CREATE TABLE supervisor_runs (
    run_id TEXT PRIMARY KEY,
    actor_id TEXT NOT NULL,
    status TEXT NOT NULL,
    state BLOB NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX supervisor_runs_actor
    ON supervisor_runs(actor_id, updated_at DESC, run_id DESC);

CREATE INDEX supervisor_runs_recovery
    ON supervisor_runs(status, updated_at, run_id);
