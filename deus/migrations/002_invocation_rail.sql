-- Track payment rail per invocation for rail-specific settlement batches.
ALTER TABLE invocations ADD COLUMN IF NOT EXISTS payment_rail TEXT NOT NULL DEFAULT 'direct';

CREATE INDEX IF NOT EXISTS idx_invocations_rail_settlement
    ON invocations (settlement_id)
    WHERE outcome = 'ok' AND settlement_id IS NULL;
