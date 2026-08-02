CREATE TABLE workforce_product_capability_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    kind TEXT NOT NULL CHECK (
        kind IN (
            'product_design_handoff','engineering_result',
            'metric_definition','reliability_incident'
        )
    ),
    initiative_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    author_seat_id TEXT NOT NULL,
    verifier_seat_id TEXT NOT NULL,
    supersedes TEXT,
    record_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    author_key_id TEXT NOT NULL,
    verifier_key_id TEXT NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id),
    UNIQUE (tenant_id, organization_id, chain_id, version),
    UNIQUE (tenant_id, organization_id, chain_id, record_id, version),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, supersedes)
        REFERENCES workforce_product_capability_records (
            tenant_id, organization_id, record_id
        ),
    CHECK (author_seat_id <> verifier_seat_id),
    CHECK (effective_at >= created_at),
    CHECK (fresh_until > effective_at),
    CHECK (verified_at >= effective_at),
    CHECK (
        (version = 1 AND supersedes IS NULL) OR
        (version > 1 AND supersedes IS NOT NULL)
    )
);

CREATE TABLE workforce_product_capability_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    kind TEXT NOT NULL CHECK (
        kind IN (
            'product_design_handoff','engineering_result',
            'metric_definition','reliability_incident'
        )
    ),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, chain_id),
    FOREIGN KEY (tenant_id, organization_id, chain_id, record_id, version)
        REFERENCES workforce_product_capability_records (
            tenant_id, organization_id, chain_id, record_id, version
        )
);

CREATE TABLE workforce_product_capability_gate_bindings (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    record_version BIGINT NOT NULL CHECK (record_version > 0),
    initiative_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    evidence_kind TEXT NOT NULL CHECK (
        evidence_kind IN (
            'customer_problem','requirements','user_journey','implementation_plan',
            'source_state','deployment_state','quality','security',
            'operations_readiness','claims','legal','pricing','launch_readiness',
            'product_usage','customer','independent_review'
        )
    ),
    evidence_hash CHAR(64) NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    verdict_id TEXT NOT NULL,
    verdict_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, record_id, evidence_id, evidence_kind
    ),
    FOREIGN KEY (tenant_id, organization_id, record_id)
        REFERENCES workforce_product_capability_records (
            tenant_id, organization_id, record_id
        ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK (fresh_until > created_at)
);

CREATE INDEX workforce_product_capability_gate_due_idx
    ON workforce_product_capability_gate_bindings (
        tenant_id, organization_id, initiative_id, evidence_kind, fresh_until
    );

CREATE TABLE workforce_product_capability_checkpoints (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    checkpoint_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    handoff_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'intake','planned','implementing','implemented','verified',
            'release_ready','deployment_pending','deployed','observed',
            'incident','rolled_back','closed'
        )
    ),
    idempotency_key TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_checkpoint BYTEA NOT NULL CHECK (octet_length(sealed_checkpoint) > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, initiative_id, checkpoint_id, version
    ),
    UNIQUE (tenant_id, organization_id, initiative_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        )
);

CREATE TABLE workforce_product_capability_checkpoint_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    checkpoint_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    phase TEXT NOT NULL CHECK (
        phase IN (
            'intake','planned','implementing','implemented','verified',
            'release_ready','deployment_pending','deployed','observed',
            'incident','rolled_back','closed'
        )
    ),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, checkpoint_id, version
    ) REFERENCES workforce_product_capability_checkpoints (
        tenant_id, organization_id, initiative_id, checkpoint_id, version
    )
);

