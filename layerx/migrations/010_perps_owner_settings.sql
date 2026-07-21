-- 010_perps_owner_settings.sql — per-owner perps settings (forward-only).
-- The owner's IANA timezone anchors delegation daily-counter windows: the
-- boundary is materialized as a UTC instant at check time, so a DST shift can
-- never double a window.

CREATE TABLE IF NOT EXISTS perp_owner_settings (
    owner_did  TEXT PRIMARY KEY,
    timezone   TEXT NOT NULL DEFAULT 'UTC',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
