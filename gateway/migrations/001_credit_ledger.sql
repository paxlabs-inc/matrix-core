-- 001_credit_ledger.sql — sess#32 ambient-architect plan §5.15
--
-- Idempotent (CREATE ... IF NOT EXISTS); safe to re-run from
-- bootstrap.sh + the box-level migration runner.
--
-- Tables:
--   credit_ledger        one row per LLM call routed via gateway
--   daily_budget_caps    one row per actor; default cap = 10 PAX/day
--
-- Cost columns use NUMERIC(20,12) so the canonical PAX-string
-- representation produced by gateway/internal/rates round-trips
-- byte-identical between Go big.Float math and Postgres.

BEGIN;

CREATE TABLE IF NOT EXISTS credit_ledger (
    id            BIGSERIAL PRIMARY KEY,
    actor_did     TEXT NOT NULL,
    intent_id     TEXT,
    goal_id       TEXT,
    model         TEXT NOT NULL,
    slot          TEXT,
    kind_route    TEXT,
    tokens_input  INTEGER NOT NULL,
    tokens_output INTEGER NOT NULL,
    cost_pax      NUMERIC(20,12) NOT NULL,
    rate_table_v  INTEGER NOT NULL,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-actor time-range index for the DailySpend rollup. The gateway
-- queries WHERE actor_did = $1 AND occurred_at >= $2 AND occurred_at
-- < $3 (half-open UTC day, see ledger/postgres.go), so a plain
-- (actor_did, occurred_at) b-tree is fully sargable. NOTE: indexing
-- date_trunc('day', occurred_at) is rejected here — date_trunc on a
-- TIMESTAMPTZ is STABLE (timezone-dependent), not IMMUTABLE, so it
-- cannot appear in an index expression.
CREATE INDEX IF NOT EXISTS idx_credit_ledger_actor_day
    ON credit_ledger (actor_did, occurred_at);

-- Per-goal index for plan §5.11 cost telemetry views (daemon-side
-- /goals/{id}/usage endpoint reads this). Partial so empty/null goal
-- rows don't bloat the index.
CREATE INDEX IF NOT EXISTS idx_credit_ledger_goal
    ON credit_ledger (goal_id) WHERE goal_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS daily_budget_caps (
    actor_did     TEXT PRIMARY KEY,
    daily_pax_max NUMERIC(20,12) NOT NULL DEFAULT 10,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
