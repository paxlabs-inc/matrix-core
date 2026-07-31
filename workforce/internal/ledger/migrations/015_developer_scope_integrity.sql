ALTER TABLE workforce_developer_change_scopes
    ADD COLUMN scope_payload BYTEA;

ALTER TABLE workforce_developer_change_claims
    ADD COLUMN resource_key TEXT;

CREATE TABLE workforce_developer_scope_events (
    event_id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    event_kind TEXT NOT NULL CHECK (
        event_kind IN ('authorized','denied','effect_started','effect_committed','effect_failed')
    ),
    operation TEXT NOT NULL,
    scope_hash CHAR(64) NOT NULL,
    evidence_hash CHAR(64),
    reason_code TEXT,
    occurred_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (tenant_id, organization_id, lease_id)
        REFERENCES workforce_developer_change_scopes (
            tenant_id, organization_id, lease_id
        )
);

CREATE INDEX workforce_developer_scope_events_scope_idx
    ON workforce_developer_scope_events (
        tenant_id, organization_id, lease_id, occurred_at, event_id
    );

CREATE FUNCTION workforce_developer_scope_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workforce developer scope authority is immutable';
END;
$$;

CREATE TRIGGER workforce_developer_scopes_no_update
BEFORE UPDATE OR DELETE ON workforce_developer_change_scopes
FOR EACH ROW EXECUTE FUNCTION workforce_developer_scope_immutable();

CREATE TRIGGER workforce_developer_claims_no_update
BEFORE UPDATE OR DELETE ON workforce_developer_change_claims
FOR EACH ROW EXECUTE FUNCTION workforce_developer_scope_immutable();

CREATE TRIGGER workforce_developer_events_no_update
BEFORE UPDATE OR DELETE ON workforce_developer_scope_events
FOR EACH ROW EXECUTE FUNCTION workforce_developer_scope_immutable();
