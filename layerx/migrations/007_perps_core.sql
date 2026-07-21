-- layerx 007_perps_core — perps domain core: markets, pools, orders, positions,
-- margin reservations, fills, funding, liquidations, batches, idempotency.
-- See spec/layerx-perps design.body.md. All money/price/rate/quantity values are
-- integers at the locked scales (micro-USDX 1e6, price cents 1e2, rates ppb 1e9,
-- whole contracts). Forward-only, idempotent (CREATE IF NOT EXISTS).

BEGIN;

-- Markets: one row per locked registry market. Numeric parameters mirror the
-- source-locked registry (internal/perps/market) and are validated against it
-- at boot; mode is the durable runtime market mode (boot config remains the
-- fail-closed floor until runtime changes are wired through perp_events).
CREATE TABLE IF NOT EXISTS perp_markets (
    symbol                     TEXT PRIMARY KEY,
    class                      TEXT NOT NULL,
    tick_price_units           BIGINT NOT NULL CHECK (tick_price_units > 0),
    min_order_contracts        BIGINT NOT NULL CHECK (min_order_contracts > 0),
    min_position_contracts     BIGINT NOT NULL CHECK (min_position_contracts > 0),
    initial_margin_bps         BIGINT NOT NULL CHECK (initial_margin_bps > 0),
    maintenance_margin_bps     BIGINT NOT NULL CHECK (maintenance_margin_bps > 0 AND maintenance_margin_bps < initial_margin_bps),
    max_leverage_x             BIGINT NOT NULL CHECK (max_leverage_x > 0),
    max_position_usdx          BIGINT NOT NULL CHECK (max_position_usdx > 0),
    max_protocol_oi_usdx       BIGINT NOT NULL CHECK (max_protocol_oi_usdx >= max_position_usdx),
    maker_fee_bps              BIGINT NOT NULL CHECK (maker_fee_bps >= 0),
    taker_fee_bps              BIGINT NOT NULL CHECK (taker_fee_bps >= maker_fee_bps),
    liquidation_fee_bps        BIGINT NOT NULL CHECK (liquidation_fee_bps >= 0),
    base_spread_bps            BIGINT NOT NULL CHECK (base_spread_bps >= 0),
    max_skew_impact_bps        BIGINT NOT NULL CHECK (max_skew_impact_bps >= 0),
    divergence_limit_bps       BIGINT NOT NULL CHECK (divergence_limit_bps > 0),
    stress_loss_bps            BIGINT NOT NULL CHECK (stress_loss_bps > 0),
    session                    TEXT NOT NULL CHECK (session IN ('24x7', 'us_equity', 'cme_index', 'comex_gold')),
    mode                       TEXT NOT NULL DEFAULT 'OFF'
                               CHECK (mode IN ('OFF', 'SHADOW', 'CANARY', 'ACTIVE', 'REDUCE_ONLY', 'PAUSED')),
    paused_cause               TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_markets_paused_cause CHECK (mode <> 'PAUSED' OR paused_cause IS NOT NULL)
);

-- Pools: typed protocol capital buckets. Funded ONLY via ordinary USDX
-- transfers out of account spendable balances (store.FundPerpPool); no
-- administrative mint/credit path exists.
CREATE TABLE IF NOT EXISTS perp_pools (
    id           TEXT PRIMARY KEY CHECK (id IN ('liquidity', 'insurance')),
    capital_usdx BIGINT NOT NULL DEFAULT 0 CHECK (capital_usdx >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO perp_pools (id) VALUES ('liquidity'), ('insurance') ON CONFLICT (id) DO NOTHING;

-- Orders: one signed intent's order row. (owner_did, idempotency_key) is the
-- exactly-once identity across retries/restarts.
CREATE TABLE IF NOT EXISTS perp_orders (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_did           TEXT NOT NULL,
    acting_did          TEXT NOT NULL,
    market_symbol       TEXT NOT NULL REFERENCES perp_markets(symbol),
    side                TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    order_type          TEXT NOT NULL CHECK (order_type IN
                        ('MARKET', 'LIMIT', 'STOP_MARKET', 'STOP_LIMIT', 'TAKE_PROFIT', 'STOP_LOSS')),
    contracts           BIGINT NOT NULL CHECK (contracts > 0),
    filled_contracts    BIGINT NOT NULL DEFAULT 0 CHECK (filled_contracts >= 0 AND filled_contracts <= contracts),
    limit_price_cents   BIGINT CHECK (limit_price_cents IS NULL OR limit_price_cents > 0),
    stop_price_cents    BIGINT CHECK (stop_price_cents IS NULL OR stop_price_cents > 0),
    time_in_force       TEXT NOT NULL CHECK (time_in_force IN ('GTC', 'IOC', 'FOK')),
    reduce_only         BOOLEAN NOT NULL DEFAULT FALSE,
    client_order_id     TEXT,
    idempotency_key     TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'RECEIVED' CHECK (status IN
                        ('RECEIVED', 'SHADOW_RECORDED', 'REJECTED', 'ACCEPTED', 'RESTING',
                         'PARTIALLY_FILLED', 'FILLED', 'CANCELLED', 'EXPIRED')),
    snapshot_id         TEXT,
    orderbook_seq       BIGINT,
    stats_seq           BIGINT,
    source_timestamp_ms BIGINT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_orders_owner_idem UNIQUE (owner_did, idempotency_key)
);
CREATE INDEX IF NOT EXISTS perp_orders_owner_idx ON perp_orders (owner_did, created_at DESC);
CREATE INDEX IF NOT EXISTS perp_orders_open_idx ON perp_orders (market_symbol, status)
    WHERE status IN ('ACCEPTED', 'RESTING', 'PARTIALLY_FILLED');

-- Positions: at most one open position per (owner, market). CLOSED rows are
-- immutable history; new exposure creates a new position id.
CREATE TABLE IF NOT EXISTS perp_positions (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_did              TEXT NOT NULL,
    market_symbol          TEXT NOT NULL REFERENCES perp_markets(symbol),
    side                   TEXT NOT NULL CHECK (side IN ('LONG', 'SHORT')),
    contracts              BIGINT NOT NULL CHECK (contracts >= 0),
    entry_price_cents      BIGINT NOT NULL CHECK (entry_price_cents > 0),
    margin_usdx            BIGINT NOT NULL DEFAULT 0 CHECK (margin_usdx >= 0),
    realized_pnl_usdx      BIGINT NOT NULL DEFAULT 0,
    unsettled_funding_usdx BIGINT NOT NULL DEFAULT 0,
    status                 TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'LIQUIDATING', 'CLOSED')),
    snapshot_id            TEXT,
    orderbook_seq          BIGINT,
    stats_seq              BIGINT,
    source_timestamp_ms    BIGINT,
    opened_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at              TIMESTAMPTZ,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_positions_open_contracts CHECK (status = 'CLOSED' OR contracts > 0),
    CONSTRAINT perp_positions_closed_flat CHECK (status <> 'CLOSED' OR (contracts = 0 AND margin_usdx = 0))
);
CREATE UNIQUE INDEX IF NOT EXISTS perp_positions_one_open_uidx
    ON perp_positions (owner_did, market_symbol) WHERE status IN ('OPEN', 'LIQUIDATING');
CREATE INDEX IF NOT EXISTS perp_positions_owner_idx ON perp_positions (owner_did, opened_at DESC);

-- Margin reservations: user initial margin moved out of spendable balance while
-- an order is in flight/resting. 'held' rows are part of the user-perps-margin
-- conservation bucket; 'applied' moved into position margin; 'released' moved
-- back to the spendable balance.
CREATE TABLE IF NOT EXISTS perp_margin_reservations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_did   TEXT NOT NULL,
    order_id    UUID NOT NULL REFERENCES perp_orders(id),
    amount_usdx BIGINT NOT NULL CHECK (amount_usdx > 0),
    status      TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held', 'applied', 'released')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS perp_margin_reservations_owner_idx ON perp_margin_reservations (owner_did) WHERE status = 'held';
CREATE INDEX IF NOT EXISTS perp_margin_reservations_order_idx ON perp_margin_reservations (order_id);

-- Fills: immutable execution records, each bound to its exact Crossverse
-- snapshot reference.
CREATE TABLE IF NOT EXISTS perp_fills (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id            UUID NOT NULL REFERENCES perp_orders(id),
    position_id         UUID NOT NULL REFERENCES perp_positions(id),
    owner_did           TEXT NOT NULL,
    acting_did          TEXT NOT NULL,
    market_symbol       TEXT NOT NULL REFERENCES perp_markets(symbol),
    side                TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    contracts           BIGINT NOT NULL CHECK (contracts > 0),
    price_cents         BIGINT NOT NULL CHECK (price_cents > 0),
    notional_usdx       BIGINT NOT NULL CHECK (notional_usdx > 0),
    fee_usdx            BIGINT NOT NULL CHECK (fee_usdx >= 0),
    realized_pnl_usdx   BIGINT NOT NULL DEFAULT 0,
    maker               BOOLEAN NOT NULL DEFAULT FALSE,
    liquidation         BOOLEAN NOT NULL DEFAULT FALSE,
    snapshot_id         TEXT NOT NULL,
    orderbook_seq       BIGINT NOT NULL,
    stats_seq           BIGINT NOT NULL,
    source_timestamp_ms BIGINT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS perp_fills_owner_idx ON perp_fills (owner_did, created_at DESC);
CREATE INDEX IF NOT EXISTS perp_fills_order_idx ON perp_fills (order_id);

-- Funding entries: one signed conserved transfer per position per interval.
CREATE TABLE IF NOT EXISTS perp_funding_entries (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position_id         UUID NOT NULL REFERENCES perp_positions(id),
    owner_did           TEXT NOT NULL,
    market_symbol       TEXT NOT NULL REFERENCES perp_markets(symbol),
    interval_start_ms   BIGINT NOT NULL,
    interval_end_ms     BIGINT NOT NULL CHECK (interval_end_ms > interval_start_ms),
    applied_ppb         BIGINT NOT NULL,
    transfer_usdx       BIGINT NOT NULL,
    snapshot_id         TEXT,
    stats_seq           BIGINT,
    source_timestamp_ms BIGINT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_funding_once UNIQUE (position_id, interval_start_ms)
);

-- Liquidations: the waterfall record of each liquidation execution.
CREATE TABLE IF NOT EXISTS perp_liquidations (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    position_id           UUID NOT NULL REFERENCES perp_positions(id),
    owner_did             TEXT NOT NULL,
    market_symbol         TEXT NOT NULL REFERENCES perp_markets(symbol),
    closed_contracts      BIGINT NOT NULL CHECK (closed_contracts > 0),
    price_cents           BIGINT NOT NULL CHECK (price_cents > 0),
    fee_usdx              BIGINT NOT NULL CHECK (fee_usdx >= 0),
    margin_absorbed_usdx  BIGINT NOT NULL DEFAULT 0 CHECK (margin_absorbed_usdx >= 0),
    insurance_paid_usdx   BIGINT NOT NULL DEFAULT 0 CHECK (insurance_paid_usdx >= 0),
    pool_paid_usdx        BIGINT NOT NULL DEFAULT 0 CHECK (pool_paid_usdx >= 0),
    deficit_usdx          BIGINT NOT NULL DEFAULT 0 CHECK (deficit_usdx >= 0),
    snapshot_id           TEXT NOT NULL,
    orderbook_seq         BIGINT NOT NULL,
    stats_seq             BIGINT NOT NULL,
    source_timestamp_ms   BIGINT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS perp_liquidations_owner_idx ON perp_liquidations (owner_did, created_at DESC);

-- Batches: settlement commitments over ranges of the global perps event journal
-- (separate Merkle root, anchored through the existing SettlementAnchor).
CREATE TABLE IF NOT EXISTS perp_batches (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    root         TEXT NOT NULL,
    event_seq_lo BIGINT NOT NULL CHECK (event_seq_lo > 0),
    event_seq_hi BIGINT NOT NULL CHECK (event_seq_hi >= event_seq_lo),
    status       TEXT NOT NULL DEFAULT 'sealed'
                 CHECK (status IN ('sealed', 'submitted', 'anchored', 'failed')),
    anchor_tx    TEXT,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotency: exactly-once results for every perps write operation.
CREATE TABLE IF NOT EXISTS perp_idempotency (
    owner_did       TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    operation       TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'in_flight' CHECK (status IN ('in_flight', 'done')),
    response        JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_idempotency_pk PRIMARY KEY (owner_did, idempotency_key),
    CONSTRAINT perp_idempotency_done_response CHECK (status <> 'done' OR response IS NOT NULL)
);

COMMIT;
