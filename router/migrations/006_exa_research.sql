BEGIN;

CREATE TABLE IF NOT EXISTS exa_research_runs (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    workflow TEXT NOT NULL,
    subject TEXT NOT NULL DEFAULT '',
    cache_key TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued','running','completed','failed','cancelled')),
    cost_dollars DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (cost_dollars >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_exa_research_runs_user_updated
    ON exa_research_runs(user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS exa_research_cache (
    user_id TEXT NOT NULL,
    cache_key TEXT NOT NULL,
    payload JSONB NOT NULL,
    cost_dollars DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (cost_dollars >= 0),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, cache_key)
);

CREATE INDEX IF NOT EXISTS idx_exa_research_cache_expiry
    ON exa_research_cache(expires_at);

CREATE TABLE IF NOT EXISTS exa_daily_spend (
    user_id TEXT NOT NULL,
    spend_day DATE NOT NULL,
    reserved_dollars DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (reserved_dollars >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, spend_day)
);

COMMIT;
