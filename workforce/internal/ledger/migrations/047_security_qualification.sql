CREATE TABLE workforce_security_qualification_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    record_kind TEXT NOT NULL CHECK (
        record_kind IN ('threat_model','boundary_review','qualification')
    ),
    version BIGINT NOT NULL CHECK (version > 0),
    author_seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id, record_kind, version),
    UNIQUE (tenant_id, organization_id, record_id, version),
    UNIQUE (tenant_id, organization_id, canonical_hash)
);

CREATE TABLE workforce_security_qualification_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    threat_model_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    threat_model_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reviewing','qualified','expired','revoked')),
    qualification_id TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, threat_model_id),
    FOREIGN KEY (
        tenant_id, organization_id, threat_model_id, version
    ) REFERENCES workforce_security_qualification_records (
        tenant_id, organization_id, record_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, qualification_id, version
    ) REFERENCES workforce_security_qualification_records (
        tenant_id, organization_id, record_id, version
    ),
    CHECK ((state = 'qualified') = (qualification_id IS NOT NULL))
);

CREATE INDEX workforce_security_qualification_current_idx
    ON workforce_security_qualification_heads (
        tenant_id, organization_id, state, expires_at, updated_at DESC
    );

CREATE TABLE workforce_security_review_coverage (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    threat_model_id TEXT NOT NULL,
    threat_model_version BIGINT NOT NULL CHECK (threat_model_version > 0),
    review_id TEXT NOT NULL,
    boundary TEXT NOT NULL CHECK (
        boundary IN (
            'policy','signature','migration','effect','customer_data',
            'financial','backup','recovery'
        )
    ),
    reviewer_seat_id TEXT NOT NULL,
    reviewer_department_id TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('approved','rejected','requires_human')),
    reviewed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, threat_model_id,
        threat_model_version, review_id, boundary
    ),
    UNIQUE (
        tenant_id, organization_id, threat_model_id,
        threat_model_version, boundary, reviewer_seat_id
    ),
    FOREIGN KEY (
        tenant_id, organization_id, review_id, threat_model_version
    ) REFERENCES workforce_security_qualification_records (
        tenant_id, organization_id, record_id, version
    )
);

CREATE INDEX workforce_security_review_boundary_idx
    ON workforce_security_review_coverage (
        tenant_id, organization_id, threat_model_id,
        threat_model_version, boundary, outcome, reviewer_department_id
    );

CREATE TRIGGER workforce_security_qualification_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_security_qualification_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_security_review_coverage_immutable
    BEFORE UPDATE OR DELETE ON workforce_security_review_coverage
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
