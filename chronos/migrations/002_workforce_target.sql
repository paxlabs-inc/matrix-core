BEGIN;

ALTER TABLE alarms
    ADD COLUMN IF NOT EXISTS target TEXT NOT NULL DEFAULT 'neo_chat',
    ADD COLUMN IF NOT EXISTS occurrence_at TIMESTAMPTZ;

ALTER TABLE alarms
    DROP CONSTRAINT IF EXISTS alarms_target_check;

ALTER TABLE alarms
    ADD CONSTRAINT alarms_target_check
    CHECK (target IN ('neo_chat', 'workforced'));

COMMIT;
