CREATE TABLE workforce_verdict_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    verdict_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    executing_seat_id TEXT NOT NULL,
    executing_department_id TEXT NOT NULL,
    auditor_seat_id TEXT NOT NULL,
    auditor_department_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    procedure_id TEXT NOT NULL,
    procedure_version BIGINT NOT NULL,
    procedure_digest CHAR(64) NOT NULL,
    packet_hash CHAR(64) NOT NULL,
    verdict_hash CHAR(64) NOT NULL,
    sealed_packet BYTEA NOT NULL,
    sealed_verdict BYTEA NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, verdict_id)
);

CREATE TABLE workforce_cross_audit_epochs (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    epoch_id TEXT NOT NULL,
    cutoff_at TIMESTAMPTZ NOT NULL,
    population_root CHAR(64) NOT NULL,
    seed_commitment CHAR(64) NOT NULL,
    sealed_seed BYTEA NOT NULL,
    numerator INTEGER NOT NULL,
    denominator INTEGER NOT NULL,
    minimum_count INTEGER NOT NULL,
    maximum_count INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, epoch_id)
);

CREATE TABLE workforce_cross_audit_selections (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    epoch_id TEXT NOT NULL,
    verdict_id TEXT NOT NULL,
    reauditor_seat_id TEXT NOT NULL,
    reauditor_department_id TEXT NOT NULL,
    score CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, epoch_id, verdict_id)
);

CREATE TABLE workforce_cross_audit_results (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    epoch_id TEXT NOT NULL,
    original_verdict_id TEXT NOT NULL,
    reaudit_verdict_id TEXT NOT NULL,
    original_outcome TEXT NOT NULL,
    reaudit_outcome TEXT NOT NULL,
    disagreement BOOLEAN NOT NULL,
    sealed_verdict BYTEA NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, epoch_id, original_verdict_id)
);

CREATE TABLE workforce_cross_audit_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    epoch_id TEXT NOT NULL,
    original_verdict_id TEXT NOT NULL,
    reaudit_verdict_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, incident_id)
);
