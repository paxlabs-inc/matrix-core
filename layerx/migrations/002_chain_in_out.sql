-- layerx 002_chain_in_out — the chain-in (deposit watcher) + chain-out
-- (withdrawal payout) plumbing for PHASE 3.
--   * did_claims    : maps the on-chain bytes32 deposit claim back to the full
--                     off-chain DID so the watcher credits the right account.
--   * chain_cursors : last-processed block per chain watcher, so restarts resume
--                     without re-scanning or double-crediting.
--   * withdrawals   : gains a payout_root (the deterministic commitment over the
--                     sealed withdrawal set, making the on-chain settle idempotent
--                     + crash-safe), a last_error surface, and a 'submitted'
--                     in-flight status between seal and on-chain confirm.
-- Forward-only, idempotent. Amounts are micro-USDX (1 USDX = 1e6).

BEGIN;

-- DID claim registry: claim = lowercase 64-hex keccak256(did) (no 0x). An agent
-- registers its claim when it fetches GET /v1/deposit; the on-chain depositor
-- passes the same value as the Vault Deposit `did` bytes32, and the watcher
-- reverses it here (keccak is one-way, so this registry is the only resolver).
CREATE TABLE IF NOT EXISTS did_claims (
    claim       TEXT PRIMARY KEY,                 -- hex(keccak256(did)), lowercase, no 0x
    did         TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS did_claims_did_idx ON did_claims (did);

-- Chain cursors: the last block a named watcher fully processed. The deposit
-- watcher persists 'deposit_watcher' here so a restart resumes from last_block+1.
CREATE TABLE IF NOT EXISTS chain_cursors (
    name        TEXT PRIMARY KEY,
    last_block  BIGINT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Withdrawal payout grouping + honest-failure surface.
ALTER TABLE withdrawals ADD COLUMN IF NOT EXISTS payout_root TEXT;  -- deterministic batch commitment
ALTER TABLE withdrawals ADD COLUMN IF NOT EXISTS payout_evm  TEXT;  -- recipient EVM frozen at seal (crash-safe recovery)
ALTER TABLE withdrawals ADD COLUMN IF NOT EXISTS last_error  TEXT;  -- surfaced submit failure (still retried)

-- Allow the in-flight 'submitted' state (between seal and on-chain confirm).
ALTER TABLE withdrawals DROP CONSTRAINT IF EXISTS withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check
    CHECK (status IN ('queued', 'submitted', 'settled', 'failed'));

CREATE INDEX IF NOT EXISTS withdrawals_status_idx       ON withdrawals (status);
CREATE INDEX IF NOT EXISTS withdrawals_payout_root_idx  ON withdrawals (payout_root) WHERE payout_root IS NOT NULL;

COMMIT;
