-- layerx 006_holds_ref — the holds primitive (authorize -> capture/release) and
-- the ref binding column on transfers (DEUS-LAYERX / LXP).
--
-- Holds lock funds inside the payer's OWN account: CreateHold debits spendable
-- balance_usdx into an open hold row, so held funds are unspendable by pay and
-- withdraw without touching escrow_usdx (which stays the net-reserve audit
-- counter). Capture consumes the hold and emits a STANDARD transfer through the
-- same seq/leaf/sig path as Pay; release/expiry returns the full amount to the
-- payer. Open holds count as circulating in the supply/reserve proof.
--
-- ref is an optional 32-byte binding digest (0x + 64 hex) carried on the
-- payer-signed intent preimage and stored on the transfer/hold row + receipt
-- JSON (row-level v1; folding ref into the Merkle leaf preimage is a v2 domain
-- bump, deliberately deferred).
--
-- Forward-only, idempotent. Safe to re-run.

BEGIN;

CREATE TABLE IF NOT EXISTS holds (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payer_did     TEXT NOT NULL,
    payee_did     TEXT NOT NULL,
    captor_did    TEXT NOT NULL,
    amount_usdx   BIGINT NOT NULL,              -- micro-USDX locked
    ref           TEXT,                         -- optional 0x + 64 hex binding digest
    expires_at    TIMESTAMPTZ NOT NULL,
    status        TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open', 'captured', 'released', 'expired')),
    captured_usdx BIGINT,                       -- micro-USDX actually captured
    capture_seq   BIGINT,                       -- transfer seq emitted by the capture
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT holds_amount_pos CHECK (amount_usdx > 0)
);
CREATE INDEX IF NOT EXISTS holds_payer_idx ON holds (payer_did, created_at DESC);
CREATE INDEX IF NOT EXISTS holds_open_expiry_idx ON holds (expires_at) WHERE status = 'open';

ALTER TABLE transfers ADD COLUMN IF NOT EXISTS ref TEXT;

COMMIT;
