-- 004_public_launch.sql — public first-run approvals and launch entitlement

BEGIN;

CREATE TABLE IF NOT EXISTS launch_entitlements (
    user_id       TEXT PRIMARY KEY,
    plan          TEXT NOT NULL,
    source        TEXT NOT NULL,
    starts_at     TIMESTAMPTZ NOT NULL,
    ends_at       TIMESTAMPTZ NOT NULL,
    claimed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

CREATE INDEX IF NOT EXISTS idx_launch_entitlements_active
    ON launch_entitlements (ends_at)
    WHERE plan = 'unlimited';

COMMIT;
