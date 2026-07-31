CREATE TABLE workforce_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    author_seat_id TEXT NOT NULL,
    department_id TEXT,
    access_seat_id TEXT,
    project_id TEXT,
    purpose TEXT NOT NULL,
    parent_intent_id TEXT NOT NULL,
    classification TEXT NOT NULL,
    validity TEXT NOT NULL,
    schema_version TEXT NOT NULL,
    content_hash_algorithm TEXT NOT NULL,
    content_hash_digest CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id),
    CHECK (classification IN ('organization', 'department', 'seat', 'project', 'restricted')),
    CHECK (validity IN ('active', 'contested', 'superseded', 'retracted', 'expired')),
    CHECK (octet_length(sealed_record) > 0)
);

CREATE INDEX workforce_records_parent_intent_idx
    ON workforce_records (tenant_id, organization_id, parent_intent_id);

CREATE INDEX workforce_records_project_idx
    ON workforce_records (tenant_id, organization_id, project_id)
    WHERE project_id IS NOT NULL;

CREATE TABLE workforce_append_keys (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    record_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id)
);

CREATE TABLE workforce_provenance_edges (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    source_record_id TEXT NOT NULL,
    consumer_record_id TEXT NOT NULL,
    edge_kind TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id,
        organization_id,
        source_record_id,
        consumer_record_id,
        edge_kind
    ),
    FOREIGN KEY (tenant_id, organization_id, source_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    FOREIGN KEY (tenant_id, organization_id, consumer_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    CHECK (source_record_id <> consumer_record_id),
    CHECK (edge_kind IN ('delivery', 'open', 'citation', 'derivation'))
);

CREATE TABLE workforce_access_edges (
    access_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    source_record_id TEXT NOT NULL,
    consumer_record_id TEXT,
    consumer_seat_id TEXT NOT NULL,
    action TEXT NOT NULL,
    purpose TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, source_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    FOREIGN KEY (tenant_id, organization_id, consumer_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    CHECK (action IN ('delivery', 'open', 'citation', 'derivation'))
);

CREATE TABLE workforce_access_denials (
    denial_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    requested_record_id_hash CHAR(64) NOT NULL,
    requester_seat_id TEXT NOT NULL,
    purpose TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE workforce_corrections (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    correction_record_id TEXT NOT NULL,
    source_record_id TEXT NOT NULL,
    status TEXT NOT NULL,
    materially_unsafe BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, correction_id),
    UNIQUE (tenant_id, organization_id, correction_record_id),
    FOREIGN KEY (tenant_id, organization_id, correction_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    FOREIGN KEY (tenant_id, organization_id, source_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    CHECK (status IN ('open', 'closed', 'escalated'))
);

CREATE TABLE workforce_correction_targets (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    affected_record_id TEXT NOT NULL,
    consumer_seat_id TEXT NOT NULL,
    state TEXT NOT NULL,
    materially_unsafe BOOLEAN NOT NULL,
    paused BOOLEAN NOT NULL,
    evidence_record_id TEXT,
    resolution_idempotency_key TEXT,
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, correction_id, affected_record_id),
    FOREIGN KEY (tenant_id, organization_id, correction_id)
        REFERENCES workforce_corrections (tenant_id, organization_id, correction_id),
    FOREIGN KEY (tenant_id, organization_id, affected_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    FOREIGN KEY (tenant_id, organization_id, evidence_record_id)
        REFERENCES workforce_records (tenant_id, organization_id, record_id),
    CHECK (state IN ('pending', 'applied', 'rejected', 'escalated')),
    CHECK (paused = (materially_unsafe AND state = 'pending'))
);

CREATE UNIQUE INDEX workforce_correction_resolution_key_idx
    ON workforce_correction_targets (
        tenant_id,
        organization_id,
        correction_id,
        resolution_idempotency_key
    )
    WHERE resolution_idempotency_key IS NOT NULL;

CREATE TABLE workforce_correction_notices (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    notice_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    affected_record_id TEXT NOT NULL,
    recipient_seat_id TEXT NOT NULL,
    state TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, notice_id),
    UNIQUE (tenant_id, organization_id, correction_id, affected_record_id),
    FOREIGN KEY (tenant_id, organization_id, correction_id, affected_record_id)
        REFERENCES workforce_correction_targets (
            tenant_id,
            organization_id,
            correction_id,
            affected_record_id
        ),
    CHECK (state IN ('pending', 'delivered'))
);

CREATE OR REPLACE FUNCTION workforce_reject_immutable_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'immutable Workforce ledger relation % cannot be mutated', TG_TABLE_NAME
        USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_append_keys_immutable
    BEFORE UPDATE OR DELETE ON workforce_append_keys
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_provenance_edges_immutable
    BEFORE UPDATE OR DELETE ON workforce_provenance_edges
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_access_edges_immutable
    BEFORE UPDATE OR DELETE ON workforce_access_edges
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
