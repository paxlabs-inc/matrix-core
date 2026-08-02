CREATE TABLE workforce_commercial_capability_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    domain TEXT NOT NULL CHECK (
        domain IN ('sales','growth','customer_operations','pricing','finance','treasury')
    ),
    kind TEXT NOT NULL CHECK (
        kind IN (
            'lead','qualification','outreach_plan','pipeline','sales_conversation',
            'proposal_handoff','contract_handoff','acquisition','growth_experiment',
            'growth_acquisition','growth_retention','growth_economics','onboarding',
            'support_case','incident_communication','feature_request','customer_health',
            'retention','churn','sla_resolution','pricing','packaging','unit_economics',
            'cash_position','runway','capital_allocation','revenue_forecast',
            'initiative_profitability'
        )
    ),
    initiative_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    author_seat_id TEXT NOT NULL,
    verifier_seat_id TEXT NOT NULL,
    supersedes TEXT,
    outcome_kind TEXT NOT NULL CHECK (
        outcome_kind IN (
            'activity','output','customer_outcome','commercial_outcome',
            'economic_outcome','risk_outcome','strategic_learning'
        )
    ),
    customer_boundary_hash CHAR(64),
    economic_boundary_hash CHAR(64),
    record_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    author_key_id TEXT NOT NULL,
    verifier_key_id TEXT NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    review_expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id),
    UNIQUE (tenant_id, organization_id, chain_id, version),
    UNIQUE (tenant_id, organization_id, chain_id, record_id, version),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, department_id)
        REFERENCES workforce_organization_departments (
            tenant_id, organization_id, department_id
        ),
    FOREIGN KEY (tenant_id, organization_id, supersedes)
        REFERENCES workforce_commercial_capability_records (
            tenant_id, organization_id, record_id
        ),
    CHECK (author_seat_id <> verifier_seat_id),
    CHECK (effective_at >= created_at),
    CHECK (fresh_until > effective_at),
    CHECK (verified_at >= effective_at),
    CHECK (review_expires_at > verified_at),
    CHECK (
        (version = 1 AND supersedes IS NULL) OR
        (version > 1 AND supersedes IS NOT NULL)
    )
);

CREATE TABLE workforce_commercial_capability_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    domain TEXT NOT NULL,
    kind TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, chain_id),
    FOREIGN KEY (tenant_id, organization_id, chain_id, record_id, version)
        REFERENCES workforce_commercial_capability_records (
            tenant_id, organization_id, chain_id, record_id, version
        )
);

CREATE TABLE workforce_commercial_observation_bindings (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    observation_kind TEXT NOT NULL,
    primary_source_class TEXT NOT NULL CHECK (
        primary_source_class IN (
            'consent_registry','crm','contract_repository','support_system',
            'product_analytics','billing_ledger','accounting_ledger',
            'bank_ledger','provider_api'
        )
    ),
    primary_evidence_hash CHAR(64) NOT NULL,
    reconciliation_source_class TEXT,
    reconciliation_evidence_hash CHAR(64),
    value_hash CHAR(64) NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    uncertainty_bps INTEGER NOT NULL CHECK (uncertainty_bps BETWEEN 0 AND 10000),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id, observation_id),
    FOREIGN KEY (tenant_id, organization_id, record_id)
        REFERENCES workforce_commercial_capability_records (
            tenant_id, organization_id, record_id
        ),
    CHECK (fresh_until > observed_at),
    CHECK (
        (reconciliation_source_class IS NULL AND reconciliation_evidence_hash IS NULL) OR
        (reconciliation_source_class IS NOT NULL AND reconciliation_evidence_hash IS NOT NULL)
    )
);

CREATE INDEX workforce_commercial_observation_freshness_idx
    ON workforce_commercial_observation_bindings (
        tenant_id, organization_id, observation_kind, fresh_until
    );

CREATE TABLE workforce_commercial_metric_bindings (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    metric_version BIGINT NOT NULL CHECK (metric_version > 0),
    definition_hash CHAR(64) NOT NULL,
    source_class TEXT NOT NULL,
    source_provider TEXT NOT NULL,
    freshness_micros BIGINT NOT NULL CHECK (freshness_micros > 0),
    maximum_uncertainty_bps INTEGER NOT NULL CHECK (maximum_uncertainty_bps BETWEEN 0 AND 10000),
    PRIMARY KEY (tenant_id, organization_id, record_id, metric_id, metric_version),
    FOREIGN KEY (tenant_id, organization_id, record_id)
        REFERENCES workforce_commercial_capability_records (
            tenant_id, organization_id, record_id
        )
);

CREATE TABLE workforce_commercial_handoffs (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    handoff_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (
        kind IN (
            'growth_to_sales','sales_to_customer_operations',
            'sales_to_contract_review','customer_operations_to_product',
            'pricing_to_sales','finance_to_executive','treasury_to_finance'
        )
    ),
    from_domain TEXT NOT NULL,
    to_domain TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, handoff_id),
    FOREIGN KEY (tenant_id, organization_id, record_id)
        REFERENCES workforce_commercial_capability_records (
            tenant_id, organization_id, record_id
        ),
    CHECK (expires_at > created_at)
);

CREATE INDEX workforce_commercial_handoff_due_idx
    ON workforce_commercial_handoffs (
        tenant_id, organization_id, to_domain, expires_at
    );

CREATE TABLE workforce_commercial_checkpoints (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    checkpoint_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    skill_id TEXT NOT NULL,
    record_chain_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'intake','observing','analyzing','review_pending','reviewed',
            'handoff_ready','closed','blocked'
        )
    ),
    idempotency_key TEXT NOT NULL,
    source_generation BIGINT NOT NULL CHECK (source_generation > 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_checkpoint BYTEA NOT NULL CHECK (octet_length(sealed_checkpoint) > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, initiative_id, workflow_id,
        checkpoint_id, version
    ),
    UNIQUE (
        tenant_id, organization_id, initiative_id, workflow_id, idempotency_key
    ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        )
);

CREATE TABLE workforce_commercial_checkpoint_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    checkpoint_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    phase TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, workflow_id),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, workflow_id,
        checkpoint_id, version
    ) REFERENCES workforce_commercial_checkpoints (
        tenant_id, organization_id, initiative_id, workflow_id,
        checkpoint_id, version
    )
);

CREATE TABLE workforce_commercial_qualifications (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    checkpoint_id TEXT NOT NULL,
    checkpoint_version BIGINT NOT NULL CHECK (checkpoint_version > 0),
    skill_id TEXT NOT NULL,
    author_wake_id TEXT NOT NULL,
    verifier_wake_id TEXT NOT NULL,
    evidence_digest CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_qualification BYTEA NOT NULL CHECK (octet_length(sealed_qualification) > 0),
    qualified_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id, evidence_digest),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, record_id)
        REFERENCES workforce_commercial_capability_records (
            tenant_id, organization_id, record_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, workflow_id,
        checkpoint_id, checkpoint_version
    ) REFERENCES workforce_commercial_checkpoints (
        tenant_id, organization_id, initiative_id, workflow_id,
        checkpoint_id, version
    ),
    CHECK (author_wake_id <> verifier_wake_id)
);
