CREATE TABLE canonical_records (
    logical_turn_id TEXT NOT NULL,
    record_type TEXT NOT NULL,
    record_key TEXT NOT NULL,
    state BLOB NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(logical_turn_id, record_type, record_key)
);

CREATE INDEX canonical_records_by_turn
    ON canonical_records(logical_turn_id, record_type, record_key);
