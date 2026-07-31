CREATE TABLE workforce_effect_operations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    proposal_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    fence BIGINT NOT NULL CHECK (fence > 0),
    provider TEXT NOT NULL,
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    proposal_hash CHAR(64) NOT NULL,
    skill_digest CHAR(64) NOT NULL,
    operation_digest CHAR(64) NOT NULL,
    state TEXT NOT NULL,
    external_id TEXT,
    evidence_hash CHAR(64),
    safe_error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, proposal_id),
    UNIQUE (tenant_id, organization_id, provider, idempotency_key),
    CHECK (state IN (
        'prepared', 'dispatching', 'succeeded', 'failed', 'externally_ambiguous'
    ))
);

CREATE TABLE workforce_effect_evidence (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    proposal_id TEXT NOT NULL,
    evidence_hash CHAR(64) NOT NULL,
    external_id TEXT NOT NULL,
    observation BYTEA NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, proposal_id, evidence_hash),
    FOREIGN KEY (tenant_id, organization_id, proposal_id)
        REFERENCES workforce_effect_operations (tenant_id, organization_id, proposal_id),
    CHECK (octet_length(observation) > 0)
);
