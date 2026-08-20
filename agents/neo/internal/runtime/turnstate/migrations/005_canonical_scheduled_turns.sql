CREATE TABLE turn_messages_canonical (
    message_id TEXT PRIMARY KEY,
    turn_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    content BLOB NOT NULL,
    created_at INTEGER NOT NULL
);

INSERT INTO turn_messages_canonical(message_id, turn_id, session_id, role, content, created_at)
SELECT message_id, turn_id, session_id, role, content, created_at FROM turn_messages;

DROP TABLE turn_messages;
ALTER TABLE turn_messages_canonical RENAME TO turn_messages;

CREATE INDEX turn_messages_by_turn
    ON turn_messages(turn_id, created_at, message_id);

CREATE TABLE scheduled_occurrences_canonical (
    occurrence_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    alarm_id TEXT NOT NULL,
    scheduled_for INTEGER NOT NULL,
    turn_id TEXT NOT NULL UNIQUE
);

INSERT INTO scheduled_occurrences_canonical(occurrence_id, kind, alarm_id, scheduled_for, turn_id)
SELECT occurrence_id, kind, alarm_id, scheduled_for, turn_id FROM scheduled_occurrences;

DROP TABLE scheduled_occurrences;
ALTER TABLE scheduled_occurrences_canonical RENAME TO scheduled_occurrences;
