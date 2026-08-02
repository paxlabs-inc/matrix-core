CREATE TABLE workforce_company_runtime_configs (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    config_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    mission_version BIGINT NOT NULL CHECK (mission_version > 0),
    mission_hash CHAR(64) NOT NULL,
    constitution_version BIGINT NOT NULL CHECK (constitution_version > 0),
    constitution_hash CHAR(64) NOT NULL,
    capital_envelope_version BIGINT NOT NULL CHECK (capital_envelope_version > 0),
    capital_envelope_hash CHAR(64) NOT NULL,
    issuer_policy_version BIGINT NOT NULL CHECK (issuer_policy_version > 0),
    issuer_policy_hash CHAR(64) NOT NULL,
    procedure_id TEXT NOT NULL,
    procedure_version BIGINT NOT NULL CHECK (procedure_version > 0),
    procedure_hash CHAR(64) NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_config BYTEA NOT NULL CHECK (octet_length(sealed_config) > 0),
    signature_key_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, config_id, version),
    UNIQUE (tenant_id, organization_id, canonical_hash)
);

CREATE TABLE workforce_company_runtime_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    config_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    state TEXT NOT NULL CHECK (state IN ('active','paused','expired')),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id),
    FOREIGN KEY (tenant_id, organization_id, config_id, version)
        REFERENCES workforce_company_runtime_configs (
            tenant_id, organization_id, config_id, version
        )
);

CREATE TABLE workforce_company_correction_snapshots (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    unresolved_material_count BIGINT NOT NULL CHECK (unresolved_material_count >= 0),
    unresolved_contaminated_count BIGINT NOT NULL CHECK (unresolved_contaminated_count >= 0),
    content_hash CHAR(64) NOT NULL,
    checked_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, snapshot_id, version),
    UNIQUE (tenant_id, organization_id, content_hash)
);

CREATE TABLE workforce_lifecycle_gate_authorizations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    transition_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    policy_decision_id TEXT NOT NULL,
    policy_decision_hash CHAR(64) NOT NULL,
    authority_clause_id TEXT NOT NULL,
    capital_limits_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_authorization BYTEA NOT NULL CHECK (octet_length(sealed_authorization) > 0),
    signature_key_id TEXT NOT NULL,
    authorized_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > authorized_at),
    PRIMARY KEY (tenant_id, organization_id, transition_id),
    UNIQUE (tenant_id, organization_id, request_hash)
);

CREATE TABLE workforce_lifecycle_gate_consumptions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    transition_id TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    consumed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, transition_id),
    FOREIGN KEY (tenant_id, organization_id, transition_id)
        REFERENCES workforce_lifecycle_gate_authorizations (
            tenant_id, organization_id, transition_id
        )
);

CREATE TABLE workforce_company_cycle_runs (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    cycle_id TEXT NOT NULL,
    cadence_kind TEXT NOT NULL,
    due_at TIMESTAMPTZ NOT NULL,
    next_at TIMESTAMPTZ NOT NULL,
    departments TEXT[] NOT NULL,
    required_capabilities TEXT[] NOT NULL,
    independent_audit BOOLEAN NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('planned','dispatched','completed','failed')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, cycle_id),
    CHECK (next_at > due_at),
    CHECK (cardinality(departments) > 0),
    CHECK (cardinality(required_capabilities) > 0)
);

CREATE TABLE workforce_company_cycle_orders (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    cycle_id TEXT NOT NULL,
    runtime_config_id TEXT NOT NULL,
    runtime_config_version BIGINT NOT NULL CHECK (runtime_config_version > 0),
    runtime_config_hash CHAR(64) NOT NULL,
    controller_id TEXT NOT NULL,
    controller_key_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_order BYTEA NOT NULL CHECK (octet_length(sealed_order) > 0),
    deadline TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, work_order_id),
    UNIQUE (tenant_id, organization_id, cycle_id),
    FOREIGN KEY (tenant_id, organization_id, cycle_id)
        REFERENCES workforce_company_cycle_runs (
            tenant_id, organization_id, cycle_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, runtime_config_id, runtime_config_version
    ) REFERENCES workforce_company_runtime_configs (
        tenant_id, organization_id, config_id, version
    ),
    CHECK (deadline > created_at)
);

CREATE TABLE workforce_company_cycle_dispatches (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    cycle_id TEXT NOT NULL,
    goal_id TEXT NOT NULL,
    initial_intent_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    dispatched_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, work_order_id),
    UNIQUE (tenant_id, organization_id, cycle_id),
    UNIQUE (tenant_id, organization_id, goal_id),
    UNIQUE (tenant_id, organization_id, wake_id),
    FOREIGN KEY (tenant_id, organization_id, work_order_id)
        REFERENCES workforce_company_cycle_orders (
            tenant_id, organization_id, work_order_id
        ),
    FOREIGN KEY (tenant_id, organization_id, cycle_id)
        REFERENCES workforce_company_cycle_runs (
            tenant_id, organization_id, cycle_id
        ),
    FOREIGN KEY (tenant_id, organization_id, goal_id)
        REFERENCES workforce_work_nodes (tenant_id, organization_id, node_id),
    FOREIGN KEY (tenant_id, organization_id, wake_id)
        REFERENCES workforce_scheduled_wakes (tenant_id, organization_id, wake_id)
);

CREATE TABLE workforce_company_funding_runs (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    funding_id TEXT NOT NULL,
    opportunity_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    decision_id TEXT NOT NULL,
    decision_hash CHAR(64) NOT NULL,
    lifecycle_version BIGINT NOT NULL CHECK (lifecycle_version >= 0),
    plan_id TEXT,
    plan_version BIGINT,
    state TEXT NOT NULL CHECK (
        state IN ('decided','lifecycle_started','funded','plan_committed','failed')
    ),
    error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, funding_id),
    UNIQUE (tenant_id, organization_id, opportunity_id),
    UNIQUE (tenant_id, organization_id, initiative_id),
    CHECK ((plan_id IS NULL) = (plan_version IS NULL))
);

CREATE TABLE workforce_company_work_order_dispatches (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    plan_node_id TEXT NOT NULL,
    goal_id TEXT NOT NULL,
    initial_intent_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    dispatched_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, work_order_id),
    UNIQUE (tenant_id, organization_id, goal_id),
    UNIQUE (tenant_id, organization_id, wake_id),
    FOREIGN KEY (tenant_id, organization_id, work_order_id)
        REFERENCES workforce_company_work_orders (
            tenant_id, organization_id, work_order_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, plan_version, plan_node_id
    ) REFERENCES workforce_company_initiative_plan_nodes (
        tenant_id, organization_id, initiative_id, plan_version, node_id
    ),
    FOREIGN KEY (tenant_id, organization_id, goal_id)
        REFERENCES workforce_work_nodes (tenant_id, organization_id, node_id),
    FOREIGN KEY (tenant_id, organization_id, wake_id)
        REFERENCES workforce_scheduled_wakes (tenant_id, organization_id, wake_id)
);

CREATE INDEX workforce_company_cycle_runs_state_idx
    ON workforce_company_cycle_runs (tenant_id, organization_id, state, due_at);

CREATE INDEX workforce_company_cycle_dispatches_cycle_idx
    ON workforce_company_cycle_dispatches (
        tenant_id, organization_id, cycle_id, dispatched_at
    );

CREATE INDEX workforce_company_funding_runs_state_idx
    ON workforce_company_funding_runs (tenant_id, organization_id, state, updated_at);

CREATE INDEX workforce_company_work_order_dispatches_plan_idx
    ON workforce_company_work_order_dispatches (
        tenant_id, organization_id, initiative_id, plan_version, plan_node_id
    );

CREATE TRIGGER workforce_company_runtime_configs_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_runtime_configs
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_cycle_orders_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_cycle_orders
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_cycle_dispatches_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_cycle_dispatches
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_correction_snapshots_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_correction_snapshots
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_lifecycle_gate_authorizations_immutable
    BEFORE UPDATE OR DELETE ON workforce_lifecycle_gate_authorizations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_lifecycle_gate_consumptions_immutable
    BEFORE UPDATE OR DELETE ON workforce_lifecycle_gate_consumptions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_company_work_order_dispatches_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_work_order_dispatches
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
