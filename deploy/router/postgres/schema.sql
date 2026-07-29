-- schema.sql — matrix-router persistent state.
--
-- Idempotent: every CREATE uses IF NOT EXISTS. Re-run safely from
-- bootstrap.sh.
--
-- Rows authored by:
--   - matrix-router      provisioning + active session bookkeeping
--   - admin (manually)   feature flags, hard quotas

BEGIN;

-- 1. Users — mirror of Supabase auth.users (subset we need).
CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY,                    -- supabase user id
    email           TEXT,
    handle          TEXT,                                -- optional display name
    state           TEXT NOT NULL DEFAULT 'provisioning',
        -- one of: 'provisioning' | 'active' | 'suspended' | 'deleted' | 'failed'
    fly_machine_id  TEXT,                                -- e.g. 32874a1b... (legacy; kept in sync for fly rows)
    fly_volume_id   TEXT,
    fly_region      TEXT,
    provider        TEXT NOT NULL DEFAULT 'fly',         -- 'fly' | 'railway'
    env_id          TEXT,                                -- provider env id (fly machine / railway service)
    env_volume_id   TEXT,
    daemon_token_hash BYTEA,                             -- sha256(MATRIX_DAEMON_TOKEN)
    s3_access_key   TEXT,                                -- MinIO scoped key
    daily_token_budget BIGINT NOT NULL DEFAULT 1000000,  -- LLM tokens / 24h
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_users_state ON users(state);
CREATE INDEX IF NOT EXISTS idx_users_machine ON users(fly_machine_id);
CREATE INDEX IF NOT EXISTS idx_users_env ON users(env_id);

-- 2. Provision jobs — async lifecycle for Fly Machines API calls.
CREATE TABLE IF NOT EXISTS provision_jobs (
    id           BIGSERIAL PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    op           TEXT NOT NULL,                          -- 'create' | 'destroy' | 'restart'
    state        TEXT NOT NULL DEFAULT 'queued',         -- 'queued' | 'running' | 'done' | 'failed'
    error        TEXT,
    fly_response JSONB,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_provision_jobs_user ON provision_jobs(user_id);
CREATE INDEX IF NOT EXISTS idx_provision_jobs_state ON provision_jobs(state) WHERE state IN ('queued','running');

-- 3. Intent index — lightweight materialised view of envelope chains so
--    the web client can list intents per user without touching the
--    daemon's journal directly. The journal on the user's Volume is
--    still the source of truth.
CREATE TABLE IF NOT EXISTS intents (
    intent_id    TEXT PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    skill_uri    TEXT NOT NULL,
    verb         TEXT NOT NULL,
    prose        TEXT,
    intent_hash  TEXT,
    plan_hash    TEXT,
    overall_root TEXT,
    state        TEXT NOT NULL,                          -- lifecycle state at last update
    compile_ms   INTEGER,
    walk_ms      INTEGER,
    tool_calls   INTEGER,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_intents_user_created ON intents(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_intents_overall ON intents(overall_root);

-- 4. Rate-limit ledger — token-bucket persistence (so restarts don't
--    reset budgets).
CREATE TABLE IF NOT EXISTS rate_buckets (
    user_id     UUID NOT NULL,
    bucket      TEXT NOT NULL,                           -- 'messages' | 'tokens' | ...
    tokens      DOUBLE PRECISION NOT NULL,
    refilled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, bucket)
);

-- 5. Beta-launch controls — invitation codes, training-consent registry,
--    beta disclosure acks, and bug/feedback reports. Kept in sync with
--    router/migrations/003_beta_launch.sql (this file is the apply path
--    run by bootstrap.sh; the router has no separate migration runner).
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

-- 6. Schema version (so future migrations can detect drift).
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT INTO schema_meta(key, value) VALUES ('version', '2')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
INSERT INTO schema_meta(key, value) VALUES ('applied_at', now()::TEXT)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;

COMMIT;

\ir ../../../router/migrations/005_railway_sharding.sql
