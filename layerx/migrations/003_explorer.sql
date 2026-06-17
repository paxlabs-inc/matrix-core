-- layerx 003_explorer — supporting indexes for the public RPC / explorer read
-- surface (PHASE 4, full-transparency rollup model). All endpoints here are
-- READ-only enumerations over the existing tables; this migration only adds the
-- indexes that keep their pagination + lookups cheap. Forward-only, idempotent.

BEGIN;

-- ListBatches orders newest-first; GetBatchByRoot (GET /v1/anchor/{root}) and
-- the idempotency pre-check both look a batch up by its Merkle root.
CREATE INDEX IF NOT EXISTS batches_created_idx ON batches (created_at DESC);
CREATE INDEX IF NOT EXISTS batches_root_idx    ON batches (root);

-- The global transfer feed (GET /v1/transfers) pages newest-first by seq. The
-- primary key already covers seq ASC; this descending index serves the feed
-- without a sort. The (from_did, seq DESC) / (to_did, seq DESC) indexes from
-- 001_init already cover the optional ?did= filter.
CREATE INDEX IF NOT EXISTS transfers_seq_desc_idx ON transfers (seq DESC);

COMMIT;
