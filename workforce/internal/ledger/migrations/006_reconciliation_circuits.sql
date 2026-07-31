CREATE TABLE workforce_circuit_breakers (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    breaker_kind TEXT NOT NULL CHECK (breaker_kind IN ('provider','skill','effect_class')),
    breaker_name TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('closed','open','half_open')),
    failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    success_count INTEGER NOT NULL DEFAULT 0 CHECK (success_count >= 0),
    window_started_at TIMESTAMPTZ NOT NULL,
    opened_at TIMESTAMPTZ,
    retry_at TIMESTAMPTZ,
    reason TEXT,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, breaker_kind, breaker_name)
);

CREATE INDEX workforce_circuit_retry_idx
    ON workforce_circuit_breakers (tenant_id, state, retry_at);

CREATE TABLE workforce_circuit_trials (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    breaker_kind TEXT NOT NULL,
    breaker_name TEXT NOT NULL,
    permit_id TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, breaker_kind, breaker_name, permit_id
    ),
    FOREIGN KEY (
        tenant_id, organization_id, breaker_kind, breaker_name
    ) REFERENCES workforce_circuit_breakers (
        tenant_id, organization_id, breaker_kind, breaker_name
    ) ON DELETE CASCADE
);

CREATE INDEX workforce_circuit_trials_expiry_idx
    ON workforce_circuit_trials (tenant_id, expires_at);

ALTER TABLE workforce_effect_operations
    ADD COLUMN skill_id TEXT,
    ADD COLUMN graph_node_id TEXT,
    ADD COLUMN effect_class TEXT,
    ADD COLUMN irreversible BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN deadline TIMESTAMPTZ,
    ADD COLUMN proposal_sealed BYTEA,
    ADD COLUMN last_probe_outcome TEXT,
    ADD COLUMN last_probe_at TIMESTAMPTZ;

CREATE TABLE workforce_reconciliation_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    proposal_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (
        outcome IN (
            'unchanged','completed_out_of_band','reversed',
            'drifted','conflicted','unknown'
        )
    ),
    effect_state TEXT NOT NULL,
    evidence_hash TEXT,
    safe_reason TEXT,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, proposal_id, observed_at)
);

CREATE INDEX workforce_reconciliation_intent_idx
    ON workforce_reconciliation_events (
        tenant_id, organization_id, intent_id, observed_at DESC
    );
