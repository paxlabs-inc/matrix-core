-- 008_developer_payee.sql
-- DEUS-LAYERX req.8: developers carry a LayerX payee DID — the identity LXP
-- settlements pay directly. Synced from the service manifest at registration.

ALTER TABLE developers ADD COLUMN IF NOT EXISTS payee_did TEXT;
