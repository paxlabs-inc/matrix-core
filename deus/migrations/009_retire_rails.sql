-- 009_retire_rails.sql
-- DEUS-LAYERX req.11.3: the direct/net/stream payment rails are deleted; LXP
-- on LayerX is the only rail. Channel/voucher/stream tables are RETIRED —
-- data preserved for audit history, no live code path reads or writes them.
-- Forward-only, idempotent.

COMMENT ON TABLE channels IS 'RETIRED (deus-layerx 6.1): net-rail payment channels; superseded by LXP holds on LayerX. Data preserved, no code paths.';
COMMENT ON TABLE vouchers IS 'RETIRED (deus-layerx 6.1): net-rail cumulative vouchers; superseded by payer-signed LayerX intents. Data preserved, no code paths.';
COMMENT ON TABLE streams IS 'RETIRED (deus-layerx 6.1): PaymentStreams rail sessions; rail deleted. Data preserved, no code paths.';
