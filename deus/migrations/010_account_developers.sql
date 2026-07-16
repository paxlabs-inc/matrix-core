-- 010_account_developers.sql
-- Deus Markets accounts own listings by their Supabase subject and stable DID.
-- Legacy SIWE wallet developers remain valid during the account migration.

BEGIN;

ALTER TABLE developers ADD COLUMN IF NOT EXISTS account_did TEXT;
ALTER TABLE developers ALTER COLUMN wallet_address DROP NOT NULL;
ALTER TABLE developers ALTER COLUMN payout_address DROP NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_developers_supabase_user_id
    ON developers (supabase_user_id)
    WHERE supabase_user_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_developers_account_did
    ON developers (lower(account_did))
    WHERE account_did IS NOT NULL;

COMMIT;
