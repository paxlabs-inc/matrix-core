-- layerx 009_perps_events — the append-only perps event journal.
-- seq is the gap-free global journal sequence; owner_event_id is the gap-free
-- per-owner private-stream sequence (both allocated MAX+1 under a transaction
-- advisory lock in store.appendPerpEvent, so rollbacks cannot create gaps).
-- Market-scoped events (e.g. market.mode_changed) have NULL owner_did.
-- Rows are immutable: UPDATE and DELETE are rejected by trigger.

BEGIN;

CREATE TABLE IF NOT EXISTS perp_events (
    seq            BIGINT PRIMARY KEY CHECK (seq > 0),
    owner_did      TEXT,
    owner_event_id BIGINT CHECK (owner_event_id IS NULL OR owner_event_id > 0),
    acting_did     TEXT,
    event_type     TEXT NOT NULL,
    payload        JSONB NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_events_owner_pairing CHECK ((owner_did IS NULL) = (owner_event_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS perp_events_owner_uidx
    ON perp_events (owner_did, owner_event_id) WHERE owner_did IS NOT NULL;
CREATE INDEX IF NOT EXISTS perp_events_type_idx ON perp_events (event_type, seq);

CREATE OR REPLACE FUNCTION perp_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'perp_events is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS perp_events_no_mutate ON perp_events;
CREATE TRIGGER perp_events_no_mutate
    BEFORE UPDATE OR DELETE ON perp_events
    FOR EACH ROW EXECUTE FUNCTION perp_events_append_only();

COMMIT;
