CREATE TABLE turn_messages (
    message_id TEXT PRIMARY KEY,
    turn_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    content BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(turn_id) REFERENCES turn_state(turn_id) ON DELETE CASCADE
);

CREATE INDEX turn_messages_by_turn
    ON turn_messages(turn_id, created_at, message_id);

CREATE TABLE scheduled_occurrences (
    occurrence_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    alarm_id TEXT NOT NULL,
    scheduled_for INTEGER NOT NULL,
    turn_id TEXT NOT NULL UNIQUE,
    FOREIGN KEY(turn_id) REFERENCES turn_state(turn_id) ON DELETE CASCADE
);
