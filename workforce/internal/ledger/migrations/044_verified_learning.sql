CREATE TABLE workforce_learning_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    record_kind TEXT NOT NULL CHECK (
        record_kind IN ('hypothesis','observation','evaluation','review','conclusion')
    ),
    hypothesis_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    author_seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, record_id),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        )
);

CREATE INDEX workforce_learning_records_hypothesis_idx
    ON workforce_learning_records (
        tenant_id, organization_id, hypothesis_id, record_kind, created_at, record_id
    );

CREATE TABLE workforce_learning_hypothesis_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    hypothesis_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','concluded','cancelled')),
    inconclusive_count INTEGER NOT NULL DEFAULT 0 CHECK (inconclusive_count >= 0),
    review_at TIMESTAMPTZ NOT NULL,
    maximum_duration_at TIMESTAMPTZ NOT NULL,
    conclusion_id TEXT,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, hypothesis_id),
    FOREIGN KEY (tenant_id, organization_id, hypothesis_id)
        REFERENCES workforce_learning_records (
            tenant_id, organization_id, record_id
        ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, conclusion_id)
        REFERENCES workforce_learning_records (
            tenant_id, organization_id, record_id
        ),
    CHECK (maximum_duration_at >= review_at),
    CHECK ((state = 'concluded') = (conclusion_id IS NOT NULL))
);

CREATE INDEX workforce_learning_hypothesis_due_idx
    ON workforce_learning_hypothesis_heads (
        tenant_id, organization_id, state, review_at, hypothesis_id
    );

CREATE TABLE workforce_learning_observation_index (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    hypothesis_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    metric_version BIGINT NOT NULL CHECK (metric_version > 0),
    evidence_id TEXT NOT NULL,
    evidence_hash CHAR(64) NOT NULL,
    authority TEXT NOT NULL CHECK (
        authority IN (
            'provider_reported','customer_reported',
            'reconciled_financial','analytically_derived'
        )
    ),
    observed_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, observation_id),
    UNIQUE (
        tenant_id, organization_id, hypothesis_id,
        metric_id, evidence_id, evidence_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, observation_id)
        REFERENCES workforce_learning_records (
            tenant_id, organization_id, record_id
        ),
    FOREIGN KEY (tenant_id, organization_id, hypothesis_id)
        REFERENCES workforce_learning_hypothesis_heads (
            tenant_id, organization_id, hypothesis_id
        ),
    CHECK (fresh_until > observed_at)
);

CREATE INDEX workforce_learning_observation_metric_idx
    ON workforce_learning_observation_index (
        tenant_id, organization_id, hypothesis_id,
        metric_id, observed_at DESC, observation_id
    );

CREATE TABLE workforce_learning_next_cycles (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    conclusion_id TEXT NOT NULL,
    hypothesis_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    next_action TEXT NOT NULL CHECK (
        next_action IN ('SCALE','PIVOT','MAINTAIN','TERMINATE','DISCOVER','REQUIRES_HUMAN')
    ),
    portfolio_feedback_id TEXT NOT NULL,
    due_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('planned','claimed','dispatched','completed','cancelled')
    ),
    claimed_at TIMESTAMPTZ,
    dispatched_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, conclusion_id),
    UNIQUE (tenant_id, organization_id, portfolio_feedback_id),
    FOREIGN KEY (tenant_id, organization_id, conclusion_id)
        REFERENCES workforce_learning_records (
            tenant_id, organization_id, record_id
        ),
    FOREIGN KEY (tenant_id, organization_id, hypothesis_id)
        REFERENCES workforce_learning_hypothesis_heads (
            tenant_id, organization_id, hypothesis_id
        )
);

CREATE INDEX workforce_learning_next_cycle_due_idx
    ON workforce_learning_next_cycles (
        tenant_id, organization_id, state, due_at, conclusion_id
    );

CREATE TABLE workforce_learning_supersessions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    conclusion_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending','applied','requires_human')),
    correction_id TEXT,
    applied_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, conclusion_id, record_id),
    FOREIGN KEY (tenant_id, organization_id, conclusion_id)
        REFERENCES workforce_learning_records (
            tenant_id, organization_id, record_id
        ),
    CHECK ((state = 'applied') = (correction_id IS NOT NULL)),
    CHECK ((state = 'applied') = (applied_at IS NOT NULL))
);

CREATE INDEX workforce_learning_supersession_pending_idx
    ON workforce_learning_supersessions (
        tenant_id, organization_id, state, created_at, conclusion_id, record_id
    );

CREATE TRIGGER workforce_learning_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_learning_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_learning_observation_index_immutable
    BEFORE UPDATE OR DELETE ON workforce_learning_observation_index
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
