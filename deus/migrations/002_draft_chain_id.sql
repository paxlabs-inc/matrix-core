-- 002_draft_chain_id.sql — drafts exist off-chain before on-chain register (Phase 1)

BEGIN;

ALTER TABLE services ALTER COLUMN chain_id DROP NOT NULL;

COMMIT;
