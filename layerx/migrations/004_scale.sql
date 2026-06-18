-- layerx 004_scale — read-path + write-path scaling prep (PHASE 8B). The ledger
-- stays Postgres (ACID money semantics); these are the non-invasive fixes that
-- keep the explorer feed + the hot Pay path cheap as the transfers table grows
-- into the millions/day range. Forward-only, idempotent.
--
--   * batches.transfer_count : denormalized child count so the batch feed
--       (GET /v1/batches) and crash-recovery enumeration stop re-aggregating
--       all child transfers via a LEFT JOIN + GROUP BY on every page.
--   * drop transfers_seq_desc_idx : redundant. The PRIMARY KEY btree on `seq`
--       already serves `ORDER BY seq DESC LIMIT n` via a backward index scan,
--       so the separate descending index only added write cost on the hottest
--       table. (The partitioned rebuild in 005 does NOT recreate it.)
--   * transfers_batch_idx -> (batch_id, seq) : lets ListBatchLeaves' ORDER BY
--       seq come straight from the index (no sort step). (005 recreates this on
--       the partitioned table; dropping it here keeps a clean intermediate state
--       if 005 has not run yet.)

BEGIN;

ALTER TABLE batches ADD COLUMN IF NOT EXISTS transfer_count INTEGER NOT NULL DEFAULT 0;

-- Backfill any batches that predate the column.
UPDATE batches b
SET transfer_count = sub.c
FROM (
    SELECT batch_id, COUNT(*) AS c
    FROM transfers
    WHERE batch_id IS NOT NULL
    GROUP BY batch_id
) sub
WHERE b.id = sub.batch_id
  AND b.transfer_count <> sub.c;

DROP INDEX IF EXISTS transfers_seq_desc_idx;

DROP INDEX IF EXISTS transfers_batch_idx;
CREATE INDEX IF NOT EXISTS transfers_batch_seq_idx ON transfers (batch_id, seq);

COMMIT;
