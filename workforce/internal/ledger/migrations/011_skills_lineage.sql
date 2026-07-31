CREATE TABLE workforce_skill_versions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    skill_id TEXT NOT NULL,
    version BIGINT NOT NULL,
    contract_digest CHAR(64) NOT NULL,
    compatibility_digest CHAR(64) NOT NULL,
    verifier_digest CHAR(64) NOT NULL,
    material_change BOOLEAN NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    key_id TEXT NOT NULL,
    sealed_contract BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, skill_id, version)
);

CREATE TABLE workforce_skill_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    skill_id TEXT NOT NULL,
    latest_version BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, skill_id)
);

CREATE TABLE workforce_compiled_plans (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    proposal_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    skill_id TEXT NOT NULL,
    skill_version BIGINT NOT NULL,
    skill_digest CHAR(64) NOT NULL,
    operation_digest CHAR(64) NOT NULL,
    verifier_digest CHAR(64) NOT NULL,
    plan_hash CHAR(64) NOT NULL,
    sealed_plan BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, proposal_id)
);

CREATE TABLE workforce_model_evidence (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    response_hash CHAR(64) NOT NULL,
    replay_retained BOOLEAN NOT NULL,
    envelope_hash CHAR(64) NOT NULL,
    sealed_envelope BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, evidence_id),
    UNIQUE (tenant_id, organization_id, wake_id, request_hash)
);

CREATE TABLE workforce_execution_receipts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    disposition TEXT NOT NULL,
    content_hash CHAR(64) NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, receipt_id),
    UNIQUE (tenant_id, organization_id, wake_id)
);
