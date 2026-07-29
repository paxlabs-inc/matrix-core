-- 004_provider_neutral.sql — provider-neutral environment storage.
--
-- The per-user environment is moving from Fly Machines to Railway
-- behind the router's provision.Provisioner seam. The fly_-prefixed
-- users columns stay (existing rows keep working, fly remains the
-- fallback provider); this migration ADDS neutral columns:
--
--   provider       'fly' | 'railway' — which provisioner owns the env
--   env_id         provider environment id (fly machine id / railway service id)
--   env_volume_id  provider volume id
--
-- The router writes both column families for fly rows and only the
-- neutral ones for railway rows; reads COALESCE(env_id, fly_machine_id)
-- so pre-backfill rows resolve identically.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS); safe to re-run.

BEGIN;

ALTER TABLE users ADD COLUMN IF NOT EXISTS provider      TEXT NOT NULL DEFAULT 'fly';
ALTER TABLE users ADD COLUMN IF NOT EXISTS env_id        TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS env_volume_id TEXT;

-- Backfill: existing rows are all Fly machines.
UPDATE users SET env_id        = fly_machine_id WHERE env_id        IS NULL AND fly_machine_id IS NOT NULL;
UPDATE users SET env_volume_id = fly_volume_id  WHERE env_volume_id IS NULL AND fly_volume_id  IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_users_env ON users(env_id);

COMMIT;
