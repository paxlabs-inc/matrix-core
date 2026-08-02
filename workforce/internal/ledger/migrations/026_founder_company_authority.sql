CREATE TABLE workforce_company_authority_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL CHECK (
        authority_kind IN (
            'founder_mission',
            'company_constitution',
            'capital_envelope',
            'company_issuer_policy'
        )
    ),
    authority_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    owner_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, authority_kind, authority_id, version
    )
);

CREATE TABLE workforce_company_authority_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL,
    authority_id TEXT NOT NULL,
    latest_version BIGINT NOT NULL CHECK (latest_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, authority_kind, authority_id),
    FOREIGN KEY (
        tenant_id, organization_id, authority_kind, authority_id, latest_version
    ) REFERENCES workforce_company_authority_records (
        tenant_id, organization_id, authority_kind, authority_id, version
    )
);

CREATE TABLE workforce_organization_v2_projection (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    schema_version TEXT NOT NULL CHECK (schema_version = 'workforce.organization.v2'),
    template_id TEXT NOT NULL,
    template_version BIGINT NOT NULL CHECK (template_version > 0),
    legacy_organization_version BIGINT NOT NULL CHECK (legacy_organization_version > 0),
    mission_version BIGINT NOT NULL CHECK (mission_version > 0),
    constitution_version BIGINT NOT NULL CHECK (constitution_version > 0),
    capital_envelope_version BIGINT NOT NULL CHECK (capital_envelope_version > 0),
    issuer_policy_version BIGINT NOT NULL CHECK (issuer_policy_version > 0),
    state TEXT NOT NULL CHECK (state IN ('active','paused')),
    paused_at TIMESTAMPTZ,
    issuer_revoked_at TIMESTAMPTZ,
    activated_at TIMESTAMPTZ NOT NULL,
	CHECK ((state = 'paused') = (paused_at IS NOT NULL)),
    PRIMARY KEY (tenant_id, organization_id)
);

CREATE TABLE workforce_company_authority_revocations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL,
    authority_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    reason TEXT NOT NULL,
    key_id TEXT NOT NULL,
    signature TEXT NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, authority_kind, authority_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, authority_kind, authority_id, version
    ) REFERENCES workforce_company_authority_records (
        tenant_id, organization_id, authority_kind, authority_id, version
    )
);

CREATE TABLE workforce_company_authority_change_receipts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL,
    authority_id TEXT NOT NULL,
    authority_version BIGINT NOT NULL CHECK (authority_version > 0),
    affected_lease_count BIGINT NOT NULL CHECK (affected_lease_count >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, receipt_id)
);

CREATE TRIGGER workforce_company_authority_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_authority_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_authority_change_receipts_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_authority_change_receipts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_authority_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_authority_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
