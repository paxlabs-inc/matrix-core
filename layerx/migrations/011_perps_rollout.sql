-- layerx 011_perps_rollout -- non-economic shadow evidence and durable
-- operator rollout/cutover controls. No table in this migration participates
-- in USDX conservation or execution.

BEGIN;

CREATE TABLE IF NOT EXISTS perp_shadow_observations (
    id                              BIGSERIAL PRIMARY KEY,
    order_id                        UUID NOT NULL UNIQUE REFERENCES perp_orders(id),
    owner_did                       TEXT NOT NULL,
    acting_did                      TEXT NOT NULL,
    market_symbol                   TEXT NOT NULL REFERENCES perp_markets(symbol),
    order_type                      TEXT NOT NULL,
    side                            TEXT NOT NULL,
    contracts                       BIGINT NOT NULL CHECK (contracts > 0),
    snapshot_id                     TEXT NOT NULL,
    orderbook_seq                   BIGINT NOT NULL,
    stats_seq                       BIGINT NOT NULL,
    source_timestamp_ms             BIGINT NOT NULL,
    engine_execution_price_cents    BIGINT NOT NULL,
    reference_execution_price_cents BIGINT NOT NULL,
    engine_margin_usdx              BIGINT NOT NULL,
    reference_margin_usdx           BIGINT NOT NULL,
    engine_fee_usdx                 BIGINT NOT NULL,
    reference_fee_usdx              BIGINT NOT NULL,
    engine_funding_usdx             BIGINT NOT NULL,
    reference_funding_usdx          BIGINT NOT NULL,
    engine_liquidation_price_cents  BIGINT NOT NULL,
    reference_liquidation_price_cents BIGINT NOT NULL,
    engine_pnl_usdx                 BIGINT NOT NULL DEFAULT 0,
    reference_pnl_usdx              BIGINT NOT NULL DEFAULT 0,
    engine_error                    TEXT NOT NULL DEFAULT '',
    reference_error                 TEXT NOT NULL DEFAULT '',
    engine_error_code               TEXT NOT NULL DEFAULT '',
    reference_error_code            TEXT NOT NULL DEFAULT '',
    execution_tolerance_cents       BIGINT NOT NULL CHECK (execution_tolerance_cents >= 0),
    mismatch_fields                 TEXT[] NOT NULL DEFAULT '{}',
    feed_gap_detected               BOOLEAN NOT NULL DEFAULT FALSE,
    account_balance_before_usdx     BIGINT NOT NULL,
    account_balance_after_usdx      BIGINT NOT NULL,
    position_count_before           BIGINT NOT NULL,
    position_count_after            BIGINT NOT NULL,
    fill_count_before               BIGINT NOT NULL,
    fill_count_after                BIGINT NOT NULL,
    matched                         BOOLEAN NOT NULL,
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS perp_shadow_observations_created_idx
    ON perp_shadow_observations (created_at);
CREATE INDEX IF NOT EXISTS perp_shadow_observations_mismatch_idx
    ON perp_shadow_observations (created_at) WHERE NOT matched;
CREATE INDEX IF NOT EXISTS perp_shadow_observations_coverage_idx
    ON perp_shadow_observations (market_symbol, order_type, created_at);

CREATE TABLE IF NOT EXISTS perp_pool_funding_intents (
    owner_did       TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    transfer_seq    BIGINT UNIQUE REFERENCES transfers(seq),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_did, idempotency_key)
);

CREATE TABLE IF NOT EXISTS perp_rollout_state (
    singleton              BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    stage                  TEXT NOT NULL DEFAULT 'OFF'
                           CHECK (stage IN ('OFF','SHADOW','STAFF','PERCENT','FULL','RETIRE_READY','RETIRED')),
    traffic_percent        INTEGER NOT NULL DEFAULT 0 CHECK (traffic_percent IN (0,1,5,25,50,100)),
    agents_enabled         BOOLEAN NOT NULL DEFAULT FALSE,
    legacy_cutover_block   BIGINT CHECK (legacy_cutover_block IS NULL OR legacy_cutover_block > 0),
    legacy_close_only      BOOLEAN NOT NULL DEFAULT FALSE,
    diamond_writes_retired BOOLEAN NOT NULL DEFAULT FALSE,
    changed_by             TEXT NOT NULL DEFAULT 'bootstrap',
    change_reason          TEXT NOT NULL DEFAULT 'initial fail-closed state',
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_rollout_retired_law
      CHECK (NOT diamond_writes_retired OR (stage IN ('RETIRE_READY','RETIRED') AND legacy_close_only))
);
INSERT INTO perp_rollout_state (singleton) VALUES (TRUE)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS perp_rollout_changes (
    id                      BIGSERIAL PRIMARY KEY,
    from_stage              TEXT NOT NULL,
    to_stage                TEXT NOT NULL,
    from_percent            INTEGER NOT NULL,
    to_percent              INTEGER NOT NULL,
    agents_enabled          BOOLEAN NOT NULL,
    legacy_cutover_block    BIGINT,
    legacy_close_only       BOOLEAN NOT NULL,
    diamond_writes_retired  BOOLEAN NOT NULL,
    changed_by              TEXT NOT NULL,
    reason                  TEXT NOT NULL,
    evidence                JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS perp_rollout_gate_samples (
    id                         BIGSERIAL PRIMARY KEY,
    accounting_drift_usdx      BIGINT NOT NULL,
    duplicate_fill_count       BIGINT NOT NULL CHECK (duplicate_fill_count >= 0),
    reconciliation_drift_usdx  BIGINT NOT NULL,
    max_feed_age_ms            BIGINT NOT NULL CHECK (max_feed_age_ms >= 0),
    liquidation_p99_ms         BIGINT NOT NULL CHECK (liquidation_p99_ms >= 0),
    insurance_capital_usdx     BIGINT NOT NULL CHECK (insurance_capital_usdx >= 0),
    insurance_floor_usdx       BIGINT NOT NULL CHECK (insurance_floor_usdx >= 0),
    private_replay_missing     BIGINT NOT NULL CHECK (private_replay_missing >= 0),
    accounting_exact           BOOLEAN GENERATED ALWAYS AS (accounting_drift_usdx = 0) STORED,
    duplicate_fills_zero       BOOLEAN GENERATED ALWAYS AS (duplicate_fill_count = 0) STORED,
    reconciliation_clean       BOOLEAN GENERATED ALWAYS AS (reconciliation_drift_usdx = 0) STORED,
    feed_fresh                 BOOLEAN NOT NULL,
    liquidation_latency_green BOOLEAN GENERATED ALWAYS AS (liquidation_p99_ms <= 1000) STORED,
    insurance_above_floor      BOOLEAN GENERATED ALWAYS AS (insurance_capital_usdx >= insurance_floor_usdx) STORED,
    private_replay_complete    BOOLEAN GENERATED ALWAYS AS (private_replay_missing = 0) STORED,
    manual_trading_stable      BOOLEAN NOT NULL,
    details                    JSONB NOT NULL DEFAULT '{}'::jsonb,
    recorded_by                TEXT NOT NULL,
    recorded_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS perp_failure_drills (
    name          TEXT PRIMARY KEY CHECK (name IN (
      'response_loss','layerx_restart','postgres_restart','crossverse_gap',
      'liquidator_restart','l1_outage','reduce_only','global_pause',
      'delegation_revocation','frontend_rollback'
    )),
    status        TEXT NOT NULL DEFAULT 'PENDING'
                  CHECK (status IN ('PENDING','RUNNING','PASSED','FAILED')),
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    evidence      JSONB NOT NULL DEFAULT '{}'::jsonb,
    changed_by    TEXT NOT NULL DEFAULT 'bootstrap',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO perp_failure_drills (name) VALUES
 ('response_loss'),('layerx_restart'),('postgres_restart'),('crossverse_gap'),
 ('liquidator_restart'),('l1_outage'),('reduce_only'),('global_pause'),
 ('delegation_revocation'),('frontend_rollback')
ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS perp_legacy_cutovers (
    cutover_block             BIGINT PRIMARY KEY CHECK (cutover_block > 0),
    block_hash                TEXT NOT NULL,
    indexer_block             BIGINT NOT NULL CHECK (indexer_block > 0),
    indexer_block_hash        TEXT NOT NULL,
    diamond_address           TEXT NOT NULL,
    snapshot_uri              TEXT NOT NULL,
    snapshot_sha256           TEXT NOT NULL CHECK (length(snapshot_sha256) = 64),
    indexer_reconciled        BOOLEAN NOT NULL DEFAULT FALSE,
    entry_orders_cancelled    BOOLEAN NOT NULL DEFAULT FALSE,
    contract_close_only       BOOLEAN NOT NULL DEFAULT FALSE,
    close_only_tx_hash        TEXT NOT NULL,
    close_only_proof_uri      TEXT NOT NULL,
    cancellation_tx_hashes    TEXT[] NOT NULL DEFAULT '{}',
    positions                 BIGINT NOT NULL CHECK (positions >= 0),
    orders                    BIGINT NOT NULL CHECK (orders = 0),
    locked_collateral_usdx    NUMERIC(78,0) NOT NULL CHECK (locked_collateral_usdx >= 0),
    unsettled_funding_usdx    NUMERIC(78,0) NOT NULL,
    owner_approved_by         TEXT NOT NULL,
    owner_authorization_uri   TEXT NOT NULL,
    owner_authorization_sha256 TEXT NOT NULL CHECK (length(owner_authorization_sha256) = 64),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS perp_legacy_zero_checks (
    id                       BIGSERIAL PRIMARY KEY,
    positions                BIGINT NOT NULL CHECK (positions >= 0),
    orders                   BIGINT NOT NULL CHECK (orders >= 0),
    locked_collateral_usdx   NUMERIC(78,0) NOT NULL CHECK (locked_collateral_usdx >= 0),
    unsettled_funding_usdx   NUMERIC(78,0) NOT NULL,
    history_available_since TIMESTAMPTZ,
    source_uri              TEXT NOT NULL,
    source_sha256           TEXT NOT NULL CHECK (length(source_sha256) = 64),
    observed_by             TEXT NOT NULL,
    all_zero                 BOOLEAN GENERATED ALWAYS AS (
      positions = 0 AND orders = 0 AND locked_collateral_usdx = 0 AND unsettled_funding_usdx = 0
    ) STORED,
    observed_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS perp_legacy_retirements (
    retire_tx_hash             TEXT PRIMARY KEY,
    proof_uri                  TEXT NOT NULL,
    owner_approved_by          TEXT NOT NULL,
    owner_authorization_uri    TEXT NOT NULL,
    owner_authorization_sha256 TEXT NOT NULL CHECK (length(owner_authorization_sha256) = 64),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
