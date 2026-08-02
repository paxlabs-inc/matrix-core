CREATE TABLE workforce_founder_key_rotations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    expected_version BIGINT NOT NULL CHECK (expected_version > 0),
    old_key_id TEXT NOT NULL,
    new_key_id TEXT NOT NULL,
    new_public_key BYTEA NOT NULL CHECK (octet_length(new_public_key) = 32),
    canonical_hash CHAR(64) NOT NULL,
    old_signature TEXT NOT NULL,
    new_signature TEXT NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, expected_version),
    CHECK (old_key_id <> new_key_id)
);

CREATE TABLE workforce_owner_control_key_revocations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    rotation_version BIGINT NOT NULL CHECK (rotation_version > 0),
    reason TEXT NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, key_id)
);

CREATE TRIGGER workforce_founder_key_rotations_immutable
    BEFORE UPDATE OR DELETE ON workforce_founder_key_rotations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_owner_control_key_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_owner_control_key_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
