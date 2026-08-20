CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE turn_state (
    turn_id TEXT PRIMARY KEY,
    actor_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    status TEXT NOT NULL,
    state BLOB NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX turn_state_recovery
    ON turn_state(status, updated_at, turn_id);

CREATE INDEX turn_state_session
    ON turn_state(session_id, updated_at DESC, turn_id DESC);

CREATE TABLE cognition_state (
    session_id TEXT PRIMARY KEY,
    state BLOB NOT NULL,
    updated_at INTEGER NOT NULL
);
