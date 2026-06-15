-- layerx 001_init — the off-chain ledger: accounts, transfers (Merkle leaves),
-- batches (settlement commitments), deposits, withdrawals.
-- See layerx.frozen.kvx [data_model]. Amounts are micro-USDX (1 USDX = 1e6).
-- Forward-only, idempotent (CREATE IF NOT EXISTS). Safe to re-run.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Accounts: one row per agent DID (the DID IS the account).
CREATE TABLE IF NOT EXISTS accounts (
    did          TEXT PRIMARY KEY,
    evm_address  TEXT,                         -- mapped Paxeer payout address
    balance_usdx BIGINT NOT NULL DEFAULT 0,    -- spendable balance, micro-USDX
    escrow_usdx  BIGINT NOT NULL DEFAULT 0,    -- on-chain-funded reserve bound, micro-USDX
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT accounts_balance_nonneg CHECK (balance_usdx >= 0),
    CONSTRAINT accounts_escrow_nonneg  CHECK (escrow_usdx >= 0)
);

-- Batches: a settlement commitment over a window's transfers.
CREATE TABLE IF NOT EXISTS batches (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    root         TEXT NOT NULL,                -- hex Merkle root over the batch leaves
    window_start TIMESTAMPTZ NOT NULL,
    window_end   TIMESTAMPTZ NOT NULL,
    status       TEXT NOT NULL DEFAULT 'sealed'
                 CHECK (status IN ('sealed', 'submitted', 'anchored', 'failed')),
    anchor_tx    TEXT,                         -- Paxeer settlement-anchor tx hash
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Transfers: one accepted value movement = one Merkle leaf.
CREATE TABLE IF NOT EXISTS transfers (
    seq         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, -- strict monotonic ordering key
    batch_id    UUID REFERENCES batches(id),                     -- NULL until sealed into a batch
    from_did    TEXT NOT NULL,
    to_did      TEXT NOT NULL,
    amount_usdx BIGINT NOT NULL,                                 -- micro-USDX
    tier        TEXT NOT NULL CHECK (tier IN ('micropayment', 'material')),
    leaf_hash   TEXT,                                            -- hex domain-separated leaf
    sig         TEXT,                                            -- hex sequencer signature
    ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT transfers_amount_pos CHECK (amount_usdx > 0)
);
CREATE INDEX IF NOT EXISTS transfers_from_idx ON transfers (from_did, seq DESC);
CREATE INDEX IF NOT EXISTS transfers_to_idx   ON transfers (to_did, seq DESC);
CREATE INDEX IF NOT EXISTS transfers_unsettled_idx ON transfers (seq) WHERE batch_id IS NULL;
CREATE INDEX IF NOT EXISTS transfers_batch_idx ON transfers (batch_id);

-- Deposits: confirmed on-chain funding events that minted USDX.
CREATE TABLE IF NOT EXISTS deposits (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    did         TEXT NOT NULL,
    evm_address TEXT,
    amount_usdx BIGINT NOT NULL,               -- micro-USDX minted (USDL received)
    deposit_tx  TEXT UNIQUE,                   -- Paxeer deposit tx hash (idempotency)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Withdrawals: queued USDX burns to be paid out as USDL at settlement.
CREATE TABLE IF NOT EXISTS withdrawals (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    did         TEXT NOT NULL,
    amount_usdx BIGINT NOT NULL,               -- micro-USDX
    swap_out    TEXT,                          -- NULL = USDL out; else target asset
    tier        TEXT NOT NULL DEFAULT 'material' CHECK (tier IN ('micropayment', 'material')),
    status      TEXT NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued', 'settled', 'failed')),
    payout_tx   TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT withdrawals_amount_pos CHECK (amount_usdx > 0)
);
CREATE INDEX IF NOT EXISTS withdrawals_did_idx ON withdrawals (did, created_at DESC);

COMMIT;
