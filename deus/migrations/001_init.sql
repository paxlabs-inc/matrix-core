-- 001_init.sql — Deus control-plane schema (docs/03-data-model.md §3.2)
-- Forward-only, idempotent (CREATE IF NOT EXISTS). Safe to re-run.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS developers (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_address   TEXT UNIQUE NOT NULL,
    payout_address   TEXT NOT NULL,
    supabase_user_id TEXT,
    display_name     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS services (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain_id       BIGINT NOT NULL,
    developer_id   UUID NOT NULL REFERENCES developers(id),
    slug           TEXT UNIQUE NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('data', 'agent')),
    mode           TEXT NOT NULL CHECK (mode IN ('proxy', 'hosted')),
    display_name   TEXT NOT NULL,
    summary        TEXT NOT NULL,
    manifest       JSONB NOT NULL,
    manifest_hash  TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'draft',
    confidential   BOOLEAN NOT NULL DEFAULT false,
    quality_score  NUMERIC,
    uptime_bps     INT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_services_manifest ON services USING gin (manifest jsonb_path_ops);
CREATE INDEX IF NOT EXISTS idx_services_kind_mode_status ON services (kind, mode, status);
CREATE INDEX IF NOT EXISTS idx_services_quality ON services (quality_score DESC);

CREATE TABLE IF NOT EXISTS endpoints (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id    UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    operation     TEXT NOT NULL,
    method        TEXT NOT NULL DEFAULT 'POST',
    input_schema  JSONB,
    output_schema JSONB,
    proxy_url     TEXT,
    runner_ref    TEXT,
    UNIQUE (service_id, operation)
);

CREATE TABLE IF NOT EXISTS pricing_plans (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id     UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    model          TEXT NOT NULL,
    unit           TEXT NOT NULL,
    price_wei      TEXT NOT NULL,
    min_charge_wei TEXT NOT NULL,
    currency       TEXT NOT NULL DEFAULT 'PAX',
    version        INT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS embeddings (
    service_id UUID PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
    model      TEXT NOT NULL,
    vec        vector(768)
);

CREATE TABLE IF NOT EXISTS quotes (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id       UUID NOT NULL REFERENCES services(id),
    endpoint_id      UUID NOT NULL REFERENCES endpoints(id),
    pricing_version  INT NOT NULL,
    unit_price_wei   TEXT NOT NULL,
    max_units        TEXT NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    signature        TEXT NOT NULL,
    caller_did       TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS settlements (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    developer_id     UUID NOT NULL REFERENCES developers(id),
    rail             TEXT NOT NULL,
    total_wei        TEXT NOT NULL,
    invocation_count INT NOT NULL,
    merkle_root      TEXT NOT NULL,
    tx_hash          TEXT,
    window_start     TIMESTAMPTZ NOT NULL,
    window_end       TIMESTAMPTZ NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending'
);

CREATE TABLE IF NOT EXISTS invocations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key  TEXT UNIQUE NOT NULL,
    service_id       UUID NOT NULL REFERENCES services(id),
    endpoint_id      UUID NOT NULL REFERENCES endpoints(id),
    caller_did       TEXT NOT NULL,
    caller_wallet    TEXT,
    quote_id         UUID REFERENCES quotes(id),
    units            TEXT NOT NULL,
    price_wei        TEXT NOT NULL,
    pricing_version  INT NOT NULL,
    args_hash        TEXT,
    result_hash      TEXT,
    outcome          TEXT NOT NULL,
    latency_ms       INT,
    settlement_id    UUID REFERENCES settlements(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_invocations_service_created ON invocations (service_id, created_at);
CREATE INDEX IF NOT EXISTS idx_invocations_settlement ON invocations (settlement_id);
CREATE INDEX IF NOT EXISTS idx_invocations_caller_created ON invocations (caller_did, created_at);

CREATE TABLE IF NOT EXISTS receipts (
    invocation_id UUID PRIMARY KEY REFERENCES invocations(id) ON DELETE CASCADE,
    eip712_digest TEXT NOT NULL,
    gateway_sig   TEXT NOT NULL,
    runner_sig    TEXT,
    caller_sig    TEXT,
    voucher_id    UUID,
    attestation   JSONB,
    blob_ref      TEXT,
    anchored_tx   TEXT
);

CREATE TABLE IF NOT EXISTS channels (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    caller_did       TEXT NOT NULL,
    caller_wallet    TEXT NOT NULL,
    escrow_addr      TEXT NOT NULL,
    balance_wei      TEXT NOT NULL,
    reserved_wei     TEXT NOT NULL DEFAULT '0',
    cumulative_wei   TEXT NOT NULL DEFAULT '0',
    nonce            BIGINT NOT NULL DEFAULT 0,
    last_voucher_sig TEXT,
    window_start     TIMESTAMPTZ NOT NULL,
    window_end       TIMESTAMPTZ NOT NULL,
    status           TEXT NOT NULL DEFAULT 'open',
    fund_tx          TEXT,
    settle_tx        TEXT
);

CREATE TABLE IF NOT EXISTS vouchers (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id       UUID NOT NULL REFERENCES channels(id),
    cumulative_wei   TEXT NOT NULL,
    nonce            BIGINT NOT NULL,
    last_receipt_hash TEXT NOT NULL,
    eip712_digest    TEXT NOT NULL,
    caller_sig       TEXT NOT NULL,
    redeemed_in      UUID REFERENCES settlements(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS spend_grants (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    caller_did       TEXT NOT NULL,
    service_id       UUID REFERENCES services(id),
    max_per_call_wei TEXT NOT NULL,
    max_total_wei    TEXT NOT NULL,
    spent_wei        TEXT NOT NULL DEFAULT '0',
    expires_at       TIMESTAMPTZ NOT NULL,
    source           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS deployments (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id           UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    appwrite_function_id TEXT,
    site_id              TEXT,
    runtime              TEXT NOT NULL,
    deployment_id        TEXT,
    exec_endpoint        TEXT,
    status               TEXT NOT NULL DEFAULT 'pending',
    region               TEXT,
    last_invoked_at      TIMESTAMPTZ,
    always_warm          BOOLEAN NOT NULL DEFAULT false,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS index_cursor (
    id             INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    last_block     BIGINT NOT NULL DEFAULT 0,
    last_log_index INT NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO index_cursor (id, last_block, last_log_index)
VALUES (1, 0, 0)
ON CONFLICT (id) DO NOTHING;

COMMIT;
