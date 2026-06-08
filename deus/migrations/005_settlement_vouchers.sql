-- 005_settlement_vouchers.sql
-- Make the caller-co-signed voucher load-bearing at settlement (docs/08 §8.3).
--
-- The net rail is the only rail that batches into settlements: direct/hosted are
-- paid inline (wallet agent/send) and stream is metered on 0x0906, so net
-- settlement must be able to select ONLY net-rail rows and tie each one back to
-- the per-caller payment channel that funds it. We also track how much of each
-- channel's escrow has already been redeemed across windows so a single
-- co-signed voucher cumulative bounds payout no matter how many developers it
-- splits across.

ALTER TABLE invocations
    ADD COLUMN IF NOT EXISTS rail TEXT NOT NULL DEFAULT 'direct';

ALTER TABLE invocations
    ADD COLUMN IF NOT EXISTS channel_id UUID REFERENCES channels(id);

CREATE INDEX IF NOT EXISTS idx_invocations_rail_settlement
    ON invocations (rail, settlement_id);

CREATE INDEX IF NOT EXISTS idx_invocations_channel
    ON invocations (channel_id);

-- Mirror of the on-chain PaymentChannel.redeemedWei: the cumulative PAX already
-- paid out of this channel's escrow across all settlement windows/developers.
ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS redeemed_wei TEXT NOT NULL DEFAULT '0';
