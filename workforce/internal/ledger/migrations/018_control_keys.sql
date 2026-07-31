CREATE TABLE workforce_owner_control_keys (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    public_key BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    registered_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, key_id)
);

CREATE UNIQUE INDEX workforce_owner_control_keys_active_idx
    ON workforce_owner_control_keys (
        tenant_id, organization_id, owner_id, key_id
    ) WHERE revoked_at IS NULL;

CREATE TRIGGER workforce_owner_control_keys_immutable
    BEFORE UPDATE OR DELETE ON workforce_owner_control_keys
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
