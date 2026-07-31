CREATE TABLE workforce_authority_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL CHECK (
        authority_kind IN ('organization', 'mandate', 'seat', 'policy')
    ),
    authority_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    owner_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    material_change BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, authority_kind, authority_id, version
    )
);

CREATE TABLE workforce_authority_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL,
    authority_id TEXT NOT NULL,
    latest_version BIGINT NOT NULL CHECK (latest_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, authority_kind, authority_id),
    FOREIGN KEY (
        tenant_id, organization_id, authority_kind, authority_id, latest_version
    ) REFERENCES workforce_authority_records (
        tenant_id, organization_id, authority_kind, authority_id, version
    )
);

CREATE TABLE workforce_authority_revocations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL,
    authority_id TEXT NOT NULL,
    version BIGINT NOT NULL,
    reason TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    signature TEXT NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, authority_kind, authority_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, authority_kind, authority_id, version
    ) REFERENCES workforce_authority_records (
        tenant_id, organization_id, authority_kind, authority_id, version
    )
);

CREATE TABLE workforce_authority_leases (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    mandate_id TEXT NOT NULL,
    mandate_version BIGINT NOT NULL CHECK (mandate_version > 0),
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_lease BYTEA NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, lease_id)
);

CREATE TABLE workforce_authority_lease_policies (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    PRIMARY KEY (tenant_id, organization_id, lease_id, policy_id),
    FOREIGN KEY (tenant_id, organization_id, lease_id)
        REFERENCES workforce_authority_leases (
            tenant_id, organization_id, lease_id
        )
);

CREATE TABLE workforce_authority_lease_invalidations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL,
    authority_id TEXT NOT NULL,
    authority_version BIGINT NOT NULL,
    reason TEXT NOT NULL,
    invalidated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, lease_id,
        authority_kind, authority_id, authority_version
    ),
    FOREIGN KEY (tenant_id, organization_id, lease_id)
        REFERENCES workforce_authority_leases (
            tenant_id, organization_id, lease_id
        )
);

CREATE TRIGGER workforce_authority_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_authority_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_authority_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_authority_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_authority_leases_immutable
    BEFORE UPDATE OR DELETE ON workforce_authority_leases
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_authority_lease_policies_immutable
    BEFORE UPDATE OR DELETE ON workforce_authority_lease_policies
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_authority_lease_invalidations_immutable
    BEFORE UPDATE OR DELETE ON workforce_authority_lease_invalidations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
