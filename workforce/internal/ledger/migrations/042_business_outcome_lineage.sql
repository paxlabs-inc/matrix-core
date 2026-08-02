CREATE TABLE workforce_business_metric_definitions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    initiative_id TEXT NOT NULL,
    outcome_kind TEXT NOT NULL CHECK (
        outcome_kind IN (
            'activity','output','customer_outcome','commercial_outcome',
            'economic_outcome','risk_outcome','strategic_learning'
        )
    ),
    aggregation TEXT NOT NULL CHECK (
        aggregation IN ('latest','sum','rate','minimum','maximum')
    ),
    unit TEXT NOT NULL,
    scale SMALLINT NOT NULL CHECK (scale BETWEEN 0 AND 12),
    definition_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    author_seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    sealed_definition BYTEA NOT NULL CHECK (octet_length(sealed_definition) > 0),
    registered_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, metric_id, version),
    UNIQUE (tenant_id, organization_id, metric_id, version, definition_hash),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK (effective_at >= registered_at),
    CHECK (expires_at > effective_at)
);

CREATE TABLE workforce_business_metric_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    initiative_id TEXT NOT NULL,
    outcome_kind TEXT NOT NULL CHECK (
        outcome_kind IN (
            'activity','output','customer_outcome','commercial_outcome',
            'economic_outcome','risk_outcome','strategic_learning'
        )
    ),
    definition_hash CHAR(64) NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, metric_id),
    FOREIGN KEY (
        tenant_id, organization_id, metric_id, version, definition_hash
    ) REFERENCES workforce_business_metric_definitions (
        tenant_id, organization_id, metric_id, version, definition_hash
    ),
    CHECK (expires_at > effective_at)
);

CREATE TABLE workforce_business_observations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    metric_version BIGINT NOT NULL CHECK (metric_version > 0),
    definition_hash CHAR(64) NOT NULL,
    outcome_kind TEXT NOT NULL CHECK (
        outcome_kind IN (
            'activity','output','customer_outcome','commercial_outcome',
            'economic_outcome','risk_outcome','strategic_learning'
        )
    ),
    status TEXT NOT NULL CHECK (
        status IN ('proposed','pending','observed','reconciled','contested','retracted')
    ),
    source_family TEXT NOT NULL CHECK (
        source_family IN (
            'product','product_telemetry','deployment','external_provider',
            'customer','crm','channel','support','commercial','billing',
            'accounting','paxeer','layerx','operational','legal','analytical'
        )
    ),
    source_event_id TEXT NOT NULL,
    source_hash CHAR(64) NOT NULL,
    value_micros BIGINT NOT NULL,
    numerator_micros BIGINT NOT NULL,
    denominator BIGINT NOT NULL CHECK (denominator > 0),
    unit TEXT NOT NULL,
    scale SMALLINT NOT NULL CHECK (scale BETWEEN 0 AND 12),
    uncertainty_bps INTEGER NOT NULL CHECK (uncertainty_bps BETWEEN 0 AND 10000),
    author_seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_observation BYTEA NOT NULL CHECK (octet_length(sealed_observation) > 0),
    observed_at TIMESTAMPTZ NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    supersedes TEXT,
    PRIMARY KEY (tenant_id, organization_id, observation_id),
    UNIQUE (tenant_id, organization_id, observation_id, content_hash),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (
        tenant_id, organization_id, metric_id, metric_version, definition_hash
    ) REFERENCES workforce_business_metric_definitions (
        tenant_id, organization_id, metric_id, version, definition_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, supersedes)
        REFERENCES workforce_business_observations (
            tenant_id, organization_id, observation_id
        ),
    CHECK (captured_at >= observed_at),
    CHECK (fresh_until > observed_at),
    CHECK (supersedes IS NULL OR supersedes <> observation_id)
);

CREATE TABLE workforce_business_observation_sources (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    source_role TEXT NOT NULL CHECK (source_role IN ('primary','supporting')),
    source_family TEXT NOT NULL CHECK (
        source_family IN (
            'product','product_telemetry','deployment','external_provider',
            'customer','crm','channel','support','commercial','billing',
            'accounting','paxeer','layerx','operational','legal','analytical'
        )
    ),
    authority TEXT NOT NULL CHECK (
        authority IN (
            'provider_reported','customer_reported','reconciled_financial',
            'analytically_derived','internal_verified','model_proposed'
        )
    ),
    record_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    source_hash CHAR(64) NOT NULL,
    provider TEXT NOT NULL,
    account_ref TEXT NOT NULL,
    object_ref TEXT NOT NULL,
    source_state TEXT NOT NULL CHECK (
        source_state IN (
            'completed','reconciled','pending','proposed','contested',
            'reversed','retracted'
        )
    ),
    observed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, observation_id, source_role,
        source_family, event_id, source_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, observation_id)
        REFERENCES workforce_business_observations (
            tenant_id, organization_id, observation_id
        )
);

CREATE UNIQUE INDEX workforce_business_observation_primary_source_idx
    ON workforce_business_observation_sources (
        tenant_id, organization_id, observation_id
    ) WHERE source_role = 'primary';

CREATE TABLE workforce_business_source_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    source_family TEXT NOT NULL,
    event_id TEXT NOT NULL,
    source_hash CHAR(64) NOT NULL,
    observation_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, source_family, event_id),
    FOREIGN KEY (tenant_id, organization_id, observation_id)
        REFERENCES workforce_business_observations (
            tenant_id, organization_id, observation_id
        )
);

CREATE INDEX workforce_business_observations_freshness_idx
    ON workforce_business_observations (
        tenant_id, organization_id, initiative_id, outcome_kind,
        status, fresh_until, observation_id
    );

CREATE INDEX workforce_business_observations_metric_idx
    ON workforce_business_observations (
        tenant_id, organization_id, metric_id, metric_version,
        observed_at DESC, observation_id
    );

CREATE TABLE workforce_business_outcomes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    outcome_id TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    initiative_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    metric_version BIGINT NOT NULL CHECK (metric_version > 0),
    definition_hash CHAR(64) NOT NULL,
    outcome_kind TEXT NOT NULL CHECK (
        outcome_kind IN (
            'activity','output','customer_outcome','commercial_outcome',
            'economic_outcome','risk_outcome','strategic_learning'
        )
    ),
    threshold_result TEXT NOT NULL CHECK (
        threshold_result IN ('success','stop','neither')
    ),
    value_micros BIGINT NOT NULL,
    numerator_micros BIGINT NOT NULL,
    denominator BIGINT NOT NULL CHECK (denominator > 0),
    unit TEXT NOT NULL,
    scale SMALLINT NOT NULL CHECK (scale BETWEEN 0 AND 12),
    record_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    author_seat_id TEXT NOT NULL,
    author_key_id TEXT NOT NULL,
    verifier_seat_id TEXT NOT NULL,
    verifier_key_id TEXT NOT NULL,
    sealed_outcome BYTEA NOT NULL CHECK (octet_length(sealed_outcome) > 0),
    derived_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    review_expires_at TIMESTAMPTZ NOT NULL,
    supersedes TEXT,
    PRIMARY KEY (tenant_id, organization_id, outcome_id),
    UNIQUE (tenant_id, organization_id, outcome_id, record_hash),
    UNIQUE (tenant_id, organization_id, chain_id, version),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (
        tenant_id, organization_id, metric_id, metric_version, definition_hash
    ) REFERENCES workforce_business_metric_definitions (
        tenant_id, organization_id, metric_id, version, definition_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, supersedes)
        REFERENCES workforce_business_outcomes (
            tenant_id, organization_id, outcome_id
        ),
    CHECK (author_seat_id <> verifier_seat_id),
    CHECK (fresh_until > derived_at),
    CHECK (verified_at >= derived_at),
    CHECK (review_expires_at > verified_at),
    CHECK (review_expires_at <= fresh_until),
    CHECK (
        (version = 1 AND supersedes IS NULL) OR
        (version > 1 AND supersedes IS NOT NULL)
    )
);

CREATE TABLE workforce_business_outcome_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    outcome_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    initiative_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    outcome_kind TEXT NOT NULL CHECK (
        outcome_kind IN (
            'activity','output','customer_outcome','commercial_outcome',
            'economic_outcome','risk_outcome','strategic_learning'
        )
    ),
    record_hash CHAR(64) NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, chain_id),
    UNIQUE (tenant_id, organization_id, initiative_id, metric_id),
    FOREIGN KEY (tenant_id, organization_id, outcome_id, record_hash)
        REFERENCES workforce_business_outcomes (
            tenant_id, organization_id, outcome_id, record_hash
        )
);

CREATE TABLE workforce_business_outcome_observations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    outcome_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    observation_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, outcome_id, observation_id),
    FOREIGN KEY (tenant_id, organization_id, outcome_id)
        REFERENCES workforce_business_outcomes (
            tenant_id, organization_id, outcome_id
        ),
    FOREIGN KEY (tenant_id, organization_id, observation_id, observation_hash)
        REFERENCES workforce_business_observations (
            tenant_id, organization_id, observation_id, content_hash
        )
);

CREATE INDEX workforce_business_outcomes_freshness_idx
    ON workforce_business_outcomes (
        tenant_id, organization_id, initiative_id, outcome_kind,
        fresh_until, outcome_id
    );

CREATE TABLE workforce_business_gate_decisions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    gate_id TEXT NOT NULL,
    decision_hash CHAR(64) NOT NULL,
    initiative_id TEXT NOT NULL,
    purpose TEXT NOT NULL CHECK (
        purpose IN (
            'business_success','lifecycle_transition',
            'risk_guardrail','learning_review'
        )
    ),
    outcome_id TEXT NOT NULL,
    outcome_hash CHAR(64) NOT NULL,
    metric_id TEXT NOT NULL,
    metric_version BIGINT NOT NULL CHECK (metric_version > 0),
    definition_hash CHAR(64) NOT NULL,
    outcome_kind TEXT NOT NULL CHECK (
        outcome_kind IN (
            'activity','output','customer_outcome','commercial_outcome',
            'economic_outcome','risk_outcome','strategic_learning'
        )
    ),
    state TEXT NOT NULL CHECK (state IN ('satisfied','open','blocked')),
    sealed_decision BYTEA NOT NULL CHECK (octet_length(sealed_decision) > 0),
    evaluated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, gate_id, decision_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, outcome_id, outcome_hash)
        REFERENCES workforce_business_outcomes (
            tenant_id, organization_id, outcome_id, record_hash
        ),
    FOREIGN KEY (
        tenant_id, organization_id, metric_id, metric_version, definition_hash
    ) REFERENCES workforce_business_metric_definitions (
        tenant_id, organization_id, metric_id, version, definition_hash
    )
);

CREATE TABLE workforce_business_gate_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    gate_id TEXT NOT NULL,
    decision_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('satisfied','open','blocked')),
    evaluated_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, gate_id),
    FOREIGN KEY (tenant_id, organization_id, gate_id, decision_hash)
        REFERENCES workforce_business_gate_decisions (
            tenant_id, organization_id, gate_id, decision_hash
        )
);

CREATE INDEX workforce_business_gate_state_idx
    ON workforce_business_gate_heads (
        tenant_id, organization_id, state, evaluated_at, gate_id
    );

CREATE TABLE workforce_business_lineage_edges (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    edge_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_id TEXT NOT NULL,
    source_hash CHAR(64) NOT NULL,
    consumer_kind TEXT NOT NULL,
    consumer_id TEXT NOT NULL,
    consumer_hash CHAR(64) NOT NULL,
    relation TEXT NOT NULL,
    material BOOLEAN NOT NULL,
    author_seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    canonical_hash CHAR(64),
    sealed_edge BYTEA,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, edge_id),
    UNIQUE (
        tenant_id, organization_id, source_kind, source_id, source_hash,
        consumer_kind, consumer_id, consumer_hash, relation
    ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK ((canonical_hash IS NULL) = (sealed_edge IS NULL)),
    CHECK (sealed_edge IS NULL OR octet_length(sealed_edge) > 0),
    CHECK (source_kind <> consumer_kind OR source_id <> consumer_id OR source_hash <> consumer_hash)
);

CREATE INDEX workforce_business_lineage_source_idx
    ON workforce_business_lineage_edges (
        tenant_id, organization_id, source_kind, source_id, source_hash
    );

CREATE INDEX workforce_business_lineage_consumer_idx
    ON workforce_business_lineage_edges (
        tenant_id, organization_id, consumer_kind, consumer_id, consumer_hash
    );

CREATE TABLE workforce_business_corrections (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    target_id TEXT NOT NULL,
    target_hash CHAR(64) NOT NULL,
    replacement_kind TEXT,
    replacement_id TEXT,
    replacement_hash CHAR(64),
    material BOOLEAN NOT NULL,
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    author_seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    sealed_correction BYTEA NOT NULL CHECK (octet_length(sealed_correction) > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, correction_id),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK (
        (replacement_kind IS NULL AND replacement_id IS NULL AND replacement_hash IS NULL) OR
        (replacement_kind IS NOT NULL AND replacement_id IS NOT NULL AND replacement_hash IS NOT NULL)
    )
);

CREATE TABLE workforce_business_contamination (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    affected_kind TEXT NOT NULL,
    affected_id TEXT NOT NULL,
    affected_hash CHAR(64) NOT NULL,
    derivation_depth INTEGER NOT NULL CHECK (derivation_depth BETWEEN 0 AND 128),
    material BOOLEAN NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','reconciled')),
    contaminated_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    resolution_id TEXT,
    replacement_kind TEXT,
    replacement_id TEXT,
    replacement_hash CHAR(64),
    PRIMARY KEY (
        tenant_id, organization_id, correction_id,
        affected_kind, affected_id, affected_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, correction_id)
        REFERENCES workforce_business_corrections (
            tenant_id, organization_id, correction_id
        ),
    CHECK ((state = 'open') = (resolved_at IS NULL)),
    CHECK ((state = 'open') = (resolution_id IS NULL)),
    CHECK (
        (state = 'open' AND replacement_kind IS NULL AND replacement_id IS NULL AND replacement_hash IS NULL) OR
        (state = 'reconciled' AND replacement_kind IS NOT NULL AND replacement_id IS NOT NULL AND replacement_hash IS NOT NULL)
    )
);

CREATE INDEX workforce_business_contamination_open_idx
    ON workforce_business_contamination (
        tenant_id, organization_id, affected_kind, affected_id, affected_hash
    ) WHERE state = 'open';

CREATE TABLE workforce_business_correction_resolutions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    resolution_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    replacement_kind TEXT NOT NULL,
    replacement_id TEXT NOT NULL,
    replacement_hash CHAR(64) NOT NULL,
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    author_seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    sealed_resolution BYTEA NOT NULL CHECK (octet_length(sealed_resolution) > 0),
    resolved_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, resolution_id),
    UNIQUE (tenant_id, organization_id, correction_id),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, correction_id)
        REFERENCES workforce_business_corrections (
            tenant_id, organization_id, correction_id
        )
);

ALTER TABLE workforce_business_contamination
    ADD CONSTRAINT workforce_business_contamination_resolution_fk
    FOREIGN KEY (tenant_id, organization_id, resolution_id)
    REFERENCES workforce_business_correction_resolutions (
        tenant_id, organization_id, resolution_id
    );

CREATE TRIGGER workforce_business_metric_definitions_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_metric_definitions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_observations_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_observations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_observation_sources_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_observation_sources
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_source_events_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_source_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_outcomes_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_outcomes
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_outcome_observations_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_outcome_observations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_gate_decisions_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_gate_decisions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_lineage_edges_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_lineage_edges
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_corrections_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_corrections
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_business_correction_resolutions_immutable
    BEFORE UPDATE OR DELETE ON workforce_business_correction_resolutions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
