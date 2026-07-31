CREATE TABLE workforce_approval_batches (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    intent_set_hash CHAR(64) NOT NULL,
    aggregate_ceiling_microunits BIGINT NOT NULL
        CHECK (aggregate_ceiling_microunits >= 0),
    consumed_microunits BIGINT NOT NULL DEFAULT 0
        CHECK (consumed_microunits >= 0),
    expires_at TIMESTAMPTZ NOT NULL,
    owner_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    signature TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_batch BYTEA NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, batch_id),
    CHECK (consumed_microunits <= aggregate_ceiling_microunits)
);

CREATE TABLE workforce_approval_batch_intents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, batch_id, intent_id),
    FOREIGN KEY (tenant_id, organization_id, batch_id)
        REFERENCES workforce_approval_batches (
            tenant_id, organization_id, batch_id
        )
);

CREATE TABLE workforce_approval_batch_consumptions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    cost_microunits BIGINT NOT NULL CHECK (cost_microunits >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, batch_id, intent_id, idempotency_key
    ),
    FOREIGN KEY (tenant_id, organization_id, batch_id, intent_id)
        REFERENCES workforce_approval_batch_intents (
            tenant_id, organization_id, batch_id, intent_id
        )
);

CREATE TABLE workforce_approval_annotations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    annotation_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    executive_seat_id TEXT NOT NULL,
    annotation TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, annotation_id)
);

CREATE TABLE workforce_policy_change_receipts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    lease_family TEXT NOT NULL CHECK (
        lease_family IN ('authority','runtime')
    ),
    lease_id TEXT NOT NULL,
    authority_kind TEXT NOT NULL,
    authority_id TEXT NOT NULL,
    authority_version BIGINT NOT NULL CHECK (authority_version > 0),
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, receipt_id)
);

CREATE TRIGGER workforce_approval_batches_immutable
    BEFORE DELETE ON workforce_approval_batches
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_approval_consumptions_immutable
    BEFORE UPDATE OR DELETE ON workforce_approval_batch_consumptions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_approval_annotations_immutable
    BEFORE UPDATE OR DELETE ON workforce_approval_annotations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_policy_change_receipts_immutable
    BEFORE UPDATE OR DELETE ON workforce_policy_change_receipts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
