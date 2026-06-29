-- 003_beta_launch.sql — Neo Onboarding & Beta Launch
--
-- Closed-beta controls: invitation codes, training-consent registry,
-- beta disclosure acknowledgements, and bug/feedback reports.
-- All live in the matrix-router central Postgres so they are queryable
-- across the whole user base independent of any single per-user daemon.
--
-- Idempotent (CREATE ... IF NOT EXISTS); safe to re-run.

BEGIN;

CREATE TABLE IF NOT EXISTS invite_codes (
    code             TEXT PRIMARY KEY,
    max_redemptions  INTEGER NOT NULL DEFAULT 1,
    redemptions_used INTEGER NOT NULL DEFAULT 0,
    expires_at       TIMESTAMPTZ,
    created_by       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS invite_redemptions (
    code        TEXT NOT NULL REFERENCES invite_codes(code),
    user_id     TEXT NOT NULL,
    redeemed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (code, user_id)
);

CREATE TABLE IF NOT EXISTS beta_consent (
    user_id        TEXT PRIMARY KEY,
    training_opt_in BOOLEAN NOT NULL DEFAULT false,
    policy_version TEXT NOT NULL,
    decided_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS beta_disclosure_ack (
    user_id            TEXT NOT NULL,
    disclosure_version TEXT NOT NULL,
    ack_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, disclosure_version)
);

CREATE TABLE IF NOT EXISTS bug_reports (
    id             BIGSERIAL PRIMARY KEY,
    user_id        TEXT NOT NULL,
    message        TEXT NOT NULL,
    context        JSONB NOT NULL DEFAULT '{}'::jsonb,
    attachment_ref TEXT,
    status         TEXT NOT NULL DEFAULT 'new',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_bug_reports_user ON bug_reports(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_bug_reports_status ON bug_reports(status) WHERE status = 'new';

COMMIT;
