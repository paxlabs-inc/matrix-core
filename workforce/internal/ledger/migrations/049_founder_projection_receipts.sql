CREATE TABLE workforce_founder_projection_receipts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    server_snapshot_hash CHAR(64) NOT NULL,
    rendered_snapshot_hash CHAR(64) NOT NULL,
    snapshot_cursor CHAR(64) NOT NULL,
    rendered_cursor CHAR(64) NOT NULL,
    resource_counts JSONB NOT NULL CHECK (jsonb_typeof(resource_counts) = 'object'),
    resource_versions JSONB NOT NULL CHECK (jsonb_typeof(resource_versions) = 'object'),
    owner_id TEXT NOT NULL,
    process_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    process_runtime TEXT NOT NULL CHECK (process_runtime = 'browser'),
    process_role TEXT NOT NULL CHECK (process_role = 'founder_renderer'),
    fresh_process BOOLEAN NOT NULL CHECK (fresh_process),
    render_evidence_id TEXT NOT NULL,
    render_evidence_hash CHAR(64) NOT NULL,
    evidence_observed_at TIMESTAMPTZ NOT NULL,
    evidence_fresh_until TIMESTAMPTZ NOT NULL,
    rendered_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signer_key_id TEXT NOT NULL,
    signature BYTEA NOT NULL CHECK (octet_length(signature) = 64),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, receipt_id, version),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK (server_snapshot_hash = rendered_snapshot_hash),
    CHECK (snapshot_cursor = rendered_cursor),
    CHECK (render_evidence_hash = rendered_snapshot_hash),
    CHECK (evidence_observed_at = rendered_at),
    CHECK (evidence_fresh_until = expires_at),
    CHECK (expires_at > rendered_at),
    CHECK (created_at >= rendered_at)
);

CREATE TABLE workforce_founder_projection_receipt_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_hash CHAR(64) NOT NULL,
    rendered_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id),
    UNIQUE (tenant_id, organization_id, receipt_id),
    FOREIGN KEY (tenant_id, organization_id, receipt_id, version)
        REFERENCES workforce_founder_projection_receipts (
            tenant_id, organization_id, receipt_id, version
        ),
    CHECK (expires_at > rendered_at)
);

CREATE INDEX workforce_founder_projection_receipt_freshness_idx
    ON workforce_founder_projection_receipt_heads (
        tenant_id, organization_id, expires_at, updated_at DESC
    );

CREATE TRIGGER workforce_founder_projection_receipts_immutable
    BEFORE UPDATE OR DELETE ON workforce_founder_projection_receipts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
