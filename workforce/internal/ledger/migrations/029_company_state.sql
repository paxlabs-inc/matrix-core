CREATE TABLE workforce_company_state_schema (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    active_version TEXT NOT NULL CHECK (
        active_version IN ('workforce.v1','workforce.company-state.store.v1')
    ),
    state TEXT NOT NULL CHECK (state IN ('active','staged')),
    staged_manifest_id TEXT,
    activated_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id),
    CHECK ((state = 'staged') = (staged_manifest_id IS NOT NULL)),
    CHECK (active_version <> 'workforce.company-state.store.v1' OR activated_at IS NOT NULL)
);

CREATE TABLE workforce_company_state_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    kind TEXT NOT NULL,
    domain TEXT NOT NULL CHECK (
        domain IN ('market','customer','product','portfolio','commercial','financial','operations','learning')
    ),
    initiative_id TEXT,
    author_seat_id TEXT NOT NULL,
    observation_kind TEXT NOT NULL CHECK (
        observation_kind IN (
            'provider_reported','customer_reported','reconciled_financial',
            'analytically_derived','model_proposed'
        )
    ),
    truth_status TEXT NOT NULL CHECK (truth_status IN ('observed','verified','proposal')),
    observed_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    validity TEXT NOT NULL CHECK (validity IN ('active','contested','retracted')),
    classification TEXT NOT NULL CHECK (
        classification IN ('organization','department','seat','project','restricted')
    ),
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id, version),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    CHECK (effective_at >= observed_at),
    CHECK (expires_at IS NULL OR expires_at > effective_at),
    CHECK ((observation_kind = 'model_proposed') = (truth_status = 'proposal'))
);

CREATE INDEX workforce_company_state_records_kind_idx
    ON workforce_company_state_records (tenant_id, organization_id, kind, effective_at, record_id);

CREATE INDEX workforce_company_state_records_initiative_idx
    ON workforce_company_state_records (tenant_id, organization_id, initiative_id, kind, effective_at)
    WHERE initiative_id IS NOT NULL;

CREATE TABLE workforce_company_state_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    domain TEXT NOT NULL,
    latest_version BIGINT NOT NULL CHECK (latest_version > 0),
    latest_content_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('active','contested','retracted')),
    expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id),
    FOREIGN KEY (tenant_id, organization_id, record_id, latest_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        )
);

CREATE INDEX workforce_company_state_heads_kind_idx
    ON workforce_company_state_heads (tenant_id, organization_id, kind, state, record_id);

CREATE TABLE workforce_company_state_derivations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    source_record_id TEXT NOT NULL,
    source_version BIGINT NOT NULL CHECK (source_version > 0),
    consumer_record_id TEXT NOT NULL,
    consumer_version BIGINT NOT NULL CHECK (consumer_version > 0),
    relation TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, source_record_id, source_version,
        consumer_record_id, consumer_version, relation, evidence_id
    ),
    FOREIGN KEY (tenant_id, organization_id, source_record_id, source_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        ),
    FOREIGN KEY (tenant_id, organization_id, consumer_record_id, consumer_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        ),
    CHECK (source_record_id <> consumer_record_id OR source_version <> consumer_version)
);

CREATE INDEX workforce_company_state_derivations_source_idx
    ON workforce_company_state_derivations (
        tenant_id, organization_id, source_record_id, source_version
    );

CREATE INDEX workforce_company_state_derivations_consumer_idx
    ON workforce_company_state_derivations (
        tenant_id, organization_id, consumer_record_id, consumer_version
    );

CREATE TABLE workforce_company_state_access (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    record_version BIGINT NOT NULL CHECK (record_version > 0),
    principal_kind TEXT NOT NULL CHECK (
        principal_kind IN (
            'organization','initiative','department','seat','project',
            'customer','financial_account','capability'
        )
    ),
    principal_id TEXT NOT NULL,
    classification TEXT NOT NULL CHECK (
        classification IN ('organization','department','seat','project','restricted')
    ),
    purpose TEXT NOT NULL,
    consent_ref TEXT,
    jurisdictions TEXT[] NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, record_id, record_version,
        principal_kind, principal_id, purpose
    ),
    FOREIGN KEY (tenant_id, organization_id, record_id, record_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        )
);

CREATE INDEX workforce_company_state_access_principal_idx
    ON workforce_company_state_access (
        tenant_id, organization_id, principal_kind, principal_id, purpose, record_id
    );

CREATE TABLE workforce_company_state_access_denials (
    denial_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    requested_record_hash CHAR(64) NOT NULL,
    principal_kind TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    purpose TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    denied_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE workforce_company_state_corrections (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    target_record_id TEXT NOT NULL,
    target_version BIGINT NOT NULL CHECK (target_version > 0),
    replacement_record_id TEXT,
    replacement_version BIGINT,
    materially_unsafe BOOLEAN NOT NULL,
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_correction BYTEA NOT NULL CHECK (octet_length(sealed_correction) > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, correction_id),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, target_record_id, target_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        ),
    FOREIGN KEY (tenant_id, organization_id, replacement_record_id, replacement_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        ),
    CHECK ((replacement_record_id IS NULL) = (replacement_version IS NULL))
);

CREATE TABLE workforce_company_state_contamination (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    affected_record_id TEXT NOT NULL,
    affected_version BIGINT NOT NULL CHECK (affected_version > 0),
    derivation_depth INTEGER NOT NULL CHECK (derivation_depth >= 0),
    materially_unsafe BOOLEAN NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','reconciled','escalated')),
    contaminated_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    resolution_record_id TEXT,
    resolution_version BIGINT,
    PRIMARY KEY (
        tenant_id, organization_id, correction_id,
        affected_record_id, affected_version
    ),
    FOREIGN KEY (tenant_id, organization_id, correction_id)
        REFERENCES workforce_company_state_corrections (
            tenant_id, organization_id, correction_id
        ),
    FOREIGN KEY (tenant_id, organization_id, affected_record_id, affected_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        ),
    FOREIGN KEY (tenant_id, organization_id, resolution_record_id, resolution_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        ),
    CHECK ((resolved_at IS NULL) = (state = 'open')),
    CHECK ((resolution_record_id IS NULL) = (resolution_version IS NULL))
);

CREATE INDEX workforce_company_state_contamination_open_idx
    ON workforce_company_state_contamination (
        tenant_id, organization_id, affected_record_id, affected_version, materially_unsafe
    ) WHERE state = 'open';

CREATE TABLE workforce_company_state_reconciliations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    reconciliation_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('reconciled','escalated')),
    resolution_record_id TEXT,
    resolution_version BIGINT,
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_reconciliation BYTEA NOT NULL CHECK (octet_length(sealed_reconciliation) > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, reconciliation_id),
    UNIQUE (tenant_id, organization_id, correction_id),
    FOREIGN KEY (tenant_id, organization_id, correction_id)
        REFERENCES workforce_company_state_corrections (
            tenant_id, organization_id, correction_id
        ),
    FOREIGN KEY (tenant_id, organization_id, resolution_record_id, resolution_version)
        REFERENCES workforce_company_state_records (
            tenant_id, organization_id, record_id, version
        ),
    CHECK ((resolution_record_id IS NULL) = (resolution_version IS NULL))
);

CREATE TABLE workforce_company_state_migration_manifests (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    source_version TEXT NOT NULL CHECK (source_version = 'workforce.v1'),
    target_version TEXT NOT NULL CHECK (target_version = 'workforce.company-state.store.v1'),
    entry_count BIGINT NOT NULL CHECK (entry_count > 0),
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_manifest BYTEA NOT NULL CHECK (octet_length(sealed_manifest) > 0),
    staged_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, manifest_id),
    UNIQUE (tenant_id, organization_id, canonical_hash)
);

CREATE TABLE workforce_company_state_migration_entries (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    ordinal BIGINT NOT NULL CHECK (ordinal > 0),
    legacy_kind TEXT NOT NULL,
    canonical_id TEXT NOT NULL,
    source_hash CHAR(64) NOT NULL,
    projected_hash CHAR(64) NOT NULL,
    projection TEXT NOT NULL CHECK (projection = 'preserve'),
    PRIMARY KEY (tenant_id, organization_id, manifest_id, ordinal),
    UNIQUE (tenant_id, organization_id, manifest_id, legacy_kind, canonical_id),
    FOREIGN KEY (tenant_id, organization_id, manifest_id)
        REFERENCES workforce_company_state_migration_manifests (
            tenant_id, organization_id, manifest_id
        ),
    CHECK (source_hash = projected_hash)
);

CREATE TABLE workforce_company_state_migration_activations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, manifest_id),
    FOREIGN KEY (tenant_id, organization_id, manifest_id)
        REFERENCES workforce_company_state_migration_manifests (
            tenant_id, organization_id, manifest_id
        )
);

CREATE OR REPLACE FUNCTION workforce_require_company_state_schema()
RETURNS TRIGGER AS $$
DECLARE
    selected_version TEXT;
    selected_state TEXT;
BEGIN
    SELECT active_version, state INTO selected_version, selected_state
    FROM workforce_company_state_schema
    WHERE tenant_id = NEW.tenant_id AND organization_id = NEW.organization_id;
    IF selected_version IS DISTINCT FROM 'workforce.company-state.store.v1'
       OR selected_state IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'mixed or inactive Company State schema for tenant %, organization %',
            NEW.tenant_id, NEW.organization_id USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_company_state_records_schema
    BEFORE INSERT ON workforce_company_state_records
    FOR EACH ROW EXECUTE FUNCTION workforce_require_company_state_schema();

CREATE TRIGGER workforce_company_state_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_state_derivations_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_derivations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_state_access_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_access
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_state_corrections_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_corrections
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_state_reconciliations_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_reconciliations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_state_migration_manifests_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_migration_manifests
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_state_migration_entries_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_migration_entries
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_state_migration_activations_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_state_migration_activations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
