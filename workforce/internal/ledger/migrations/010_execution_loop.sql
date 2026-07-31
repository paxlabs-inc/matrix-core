CREATE TABLE workforce_wake_checkpoints (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    stage TEXT NOT NULL,
    version BIGINT NOT NULL,
    disposition TEXT,
    state_hash CHAR(64) NOT NULL,
    sealed_state BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, wake_id)
);

CREATE TABLE workforce_wake_transitions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    sequence BIGINT NOT NULL,
    from_stage TEXT NOT NULL,
    to_stage TEXT NOT NULL,
    decision TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    state_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, wake_id, sequence),
    UNIQUE (tenant_id, organization_id, wake_id, idempotency_key)
);

CREATE TABLE workforce_wake_commits (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    effect_id TEXT,
    committed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, wake_id),
    UNIQUE (tenant_id, organization_id, receipt_id)
);

CREATE INDEX workforce_wake_checkpoints_stage_idx
    ON workforce_wake_checkpoints (tenant_id, organization_id, stage, updated_at);
