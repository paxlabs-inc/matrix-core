-- deus 007_lxp_invocations — the LayerX (LXP) rail's metering columns
-- (DEUS-LAYERX req.6.2). The invocations ledger stays the idempotency/replay
-- spine; layerx-rail rows carry the USDX charge, the LayerX transfer seq that
-- settled them, and (hold mode) the hold id. Forward-only, idempotent.

BEGIN;

ALTER TABLE invocations ADD COLUMN IF NOT EXISTS price_usdx TEXT;
ALTER TABLE invocations ADD COLUMN IF NOT EXISTS layerx_seq BIGINT;
ALTER TABLE invocations ADD COLUMN IF NOT EXISTS hold_id TEXT;

COMMIT;
