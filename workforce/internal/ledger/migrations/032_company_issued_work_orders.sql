CREATE TABLE workforce_company_initiative_plans (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    initiative_version BIGINT NOT NULL CHECK (initiative_version > 0),
    blueprint_id TEXT NOT NULL,
    blueprint_version BIGINT NOT NULL CHECK (blueprint_version > 0),
    mission_version BIGINT NOT NULL CHECK (mission_version > 0),
    constitution_version BIGINT NOT NULL CHECK (constitution_version > 0),
    capital_envelope_version BIGINT NOT NULL CHECK (capital_envelope_version > 0),
    issuer_policy_version BIGINT NOT NULL CHECK (issuer_policy_version > 0),
    portfolio_decision_id TEXT NOT NULL,
    capital_allocation_id TEXT NOT NULL,
    capability_plan_id TEXT NOT NULL,
    capital_microunits BIGINT NOT NULL CHECK (capital_microunits >= 0),
    risk_microunits BIGINT NOT NULL CHECK (risk_microunits >= 0),
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_plan BYTEA NOT NULL CHECK (octet_length(sealed_plan) > 0),
    compiled_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, version),
    UNIQUE (tenant_id, organization_id, plan_id),
    UNIQUE (tenant_id, organization_id, initiative_id, plan_id, version),
    UNIQUE (tenant_id, organization_id, initiative_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_portfolio_allocations (tenant_id, organization_id, initiative_id)
);

CREATE TABLE workforce_company_initiative_plan_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    state TEXT NOT NULL CHECK (state IN ('active','cancelled','superseded','contested')),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, plan_id, version)
        REFERENCES workforce_company_initiative_plans (
            tenant_id, organization_id, initiative_id, plan_id, version
        )
);

CREATE TABLE workforce_company_initiative_plan_nodes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    node_id TEXT NOT NULL,
    node_kind TEXT NOT NULL CHECK (
        node_kind IN (
            'work_order','intent','decision_gate','evidence_gate','approval_gate',
            'effect_gate','outcome_gate','branch','terminal_success',
            'terminal_failure','terminal_cancelled'
        )
    ),
    state TEXT NOT NULL CHECK (state IN ('pending','preserved','invalidated','cancelled')),
    node_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, plan_version, node_id),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, plan_version)
        REFERENCES workforce_company_initiative_plans (
            tenant_id, organization_id, initiative_id, version
        )
);

CREATE TABLE workforce_company_initiative_plan_edges (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    prerequisite_node_id TEXT NOT NULL,
    successor_node_id TEXT NOT NULL,
    branch_outcome TEXT CHECK (branch_outcome IN ('satisfied','failed','expired')),
    not_before TIMESTAMPTZ NOT NULL,
    deadline TIMESTAMPTZ NOT NULL,
    priority_delta INTEGER NOT NULL CHECK (priority_delta BETWEEN -1000 AND 1000),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, initiative_id, plan_version,
        prerequisite_node_id, successor_node_id
    ),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, plan_version, prerequisite_node_id
    ) REFERENCES workforce_company_initiative_plan_nodes (
        tenant_id, organization_id, initiative_id, plan_version, node_id
    ),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, plan_version, successor_node_id
    ) REFERENCES workforce_company_initiative_plan_nodes (
        tenant_id, organization_id, initiative_id, plan_version, node_id
    ),
    CHECK (prerequisite_node_id <> successor_node_id),
    CHECK (deadline >= not_before)
);

CREATE TABLE workforce_company_work_orders (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    plan_node_id TEXT NOT NULL,
    controller_id TEXT NOT NULL,
    issuer_kind TEXT NOT NULL CHECK (issuer_kind = 'company_controller'),
    issuer_key_id TEXT NOT NULL,
    issuer_policy_version BIGINT NOT NULL CHECK (issuer_policy_version > 0),
    work_order_class TEXT NOT NULL,
    issue_identity TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    capital_microunits BIGINT NOT NULL CHECK (capital_microunits > 0),
    risk_microunits BIGINT NOT NULL CHECK (risk_microunits >= 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_order BYTEA NOT NULL CHECK (octet_length(sealed_order) > 0),
    deadline TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, work_order_id),
    UNIQUE (tenant_id, organization_id, issue_identity),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, plan_version, plan_node_id
    ) REFERENCES workforce_company_initiative_plan_nodes (
        tenant_id, organization_id, initiative_id, plan_version, node_id
    ),
    CHECK (deadline > created_at)
);

CREATE TABLE workforce_company_work_order_bindings (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    mission_id TEXT NOT NULL,
    mission_version BIGINT NOT NULL CHECK (mission_version > 0),
    constitution_id TEXT NOT NULL,
    constitution_version BIGINT NOT NULL CHECK (constitution_version > 0),
    initiative_id TEXT NOT NULL,
    portfolio_decision_id TEXT NOT NULL,
    capital_allocation_id TEXT NOT NULL,
    capital_envelope_version BIGINT NOT NULL CHECK (capital_envelope_version > 0),
    issuer_policy_version BIGINT NOT NULL CHECK (issuer_policy_version > 0),
    capability_plan_id TEXT NOT NULL,
    capability_plan_hash CHAR(64) NOT NULL,
    initiative_plan_id TEXT NOT NULL,
    initiative_plan_version BIGINT NOT NULL CHECK (initiative_plan_version > 0),
    initiative_execution_criteria TEXT[] NOT NULL CHECK (cardinality(initiative_execution_criteria) > 0),
    business_success_criteria TEXT[] NOT NULL CHECK (cardinality(business_success_criteria) > 0),
    business_outcome_gate_ids TEXT[] NOT NULL CHECK (cardinality(business_outcome_gate_ids) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, work_order_id),
    FOREIGN KEY (tenant_id, organization_id, work_order_id)
        REFERENCES workforce_company_work_orders (
            tenant_id, organization_id, work_order_id
        )
);

CREATE TABLE workforce_company_effect_identities (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    effect_identity TEXT NOT NULL,
    first_plan_version BIGINT NOT NULL CHECK (first_plan_version > 0),
    first_node_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    reserved_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, effect_identity),
    FOREIGN KEY (tenant_id, organization_id, work_order_id)
        REFERENCES workforce_company_work_orders (
            tenant_id, organization_id, work_order_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, first_plan_version, first_node_id
    ) REFERENCES workforce_company_initiative_plan_nodes (
        tenant_id, organization_id, initiative_id, plan_version, node_id
    )
);

CREATE TABLE workforce_company_effect_identity_commits (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    effect_identity TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, effect_identity),
    UNIQUE (tenant_id, organization_id, receipt_id, effect_identity),
    FOREIGN KEY (tenant_id, organization_id, effect_identity)
        REFERENCES workforce_company_effect_identities (
            tenant_id, organization_id, effect_identity
        )
);

CREATE TABLE workforce_company_plan_mutations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    mutation_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('replan','cancel','invalidate')),
    from_plan_version BIGINT NOT NULL CHECK (from_plan_version > 0),
    to_plan_version BIGINT NOT NULL CHECK (to_plan_version > from_plan_version),
    reason TEXT NOT NULL,
    authority_ref TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_mutation BYTEA NOT NULL CHECK (octet_length(sealed_mutation) > 0),
    mutated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, mutation_id),
    UNIQUE (tenant_id, organization_id, initiative_id, to_plan_version),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, from_plan_version)
        REFERENCES workforce_company_initiative_plans (
            tenant_id, organization_id, initiative_id, version
        ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, to_plan_version)
        REFERENCES workforce_company_initiative_plans (
            tenant_id, organization_id, initiative_id, version
        )
);

CREATE TABLE workforce_company_plan_invalidations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    mutation_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    authority_ref TEXT NOT NULL,
    evidence_record_ids TEXT[] NOT NULL CHECK (cardinality(evidence_record_ids) > 0),
    invalidated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, mutation_id, node_id),
    FOREIGN KEY (tenant_id, organization_id, mutation_id)
        REFERENCES workforce_company_plan_mutations (
            tenant_id, organization_id, mutation_id
        )
);

CREATE TABLE workforce_company_plan_preserved_receipts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    mutation_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    preserved_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, mutation_id, node_id, receipt_id),
    FOREIGN KEY (tenant_id, organization_id, mutation_id)
        REFERENCES workforce_company_plan_mutations (
            tenant_id, organization_id, mutation_id
        )
);

CREATE TABLE workforce_company_outcome_gates (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    gate_id TEXT NOT NULL,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    node_id TEXT NOT NULL,
    predicate_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','satisfied','failed','expired','contested')),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, gate_id, plan_version),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, plan_version, node_id
    ) REFERENCES workforce_company_initiative_plan_nodes (
        tenant_id, organization_id, initiative_id, plan_version, node_id
    )
);

CREATE TABLE workforce_company_outcome_gate_commits (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    gate_id TEXT NOT NULL,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    outcome TEXT NOT NULL CHECK (outcome IN ('satisfied','failed','expired')),
    authoritative_record_ids TEXT[] NOT NULL CHECK (cardinality(authoritative_record_ids) > 0),
    receipt_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, gate_id, plan_version),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, gate_id, plan_version)
        REFERENCES workforce_company_outcome_gates (
            tenant_id, organization_id, initiative_id, gate_id, plan_version
        )
);

CREATE INDEX workforce_company_plan_heads_state_idx
    ON workforce_company_initiative_plan_heads (tenant_id, organization_id, state, updated_at);
CREATE INDEX workforce_company_plan_edges_due_idx
    ON workforce_company_initiative_plan_edges (
        tenant_id, organization_id, initiative_id, plan_version, not_before, deadline
    );
CREATE INDEX workforce_company_outcome_gates_open_idx
    ON workforce_company_outcome_gates (
        tenant_id, organization_id, initiative_id, expires_at
    ) WHERE state = 'open';

CREATE TRIGGER workforce_company_initiative_plans_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_initiative_plans
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_initiative_plan_nodes_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_initiative_plan_nodes
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_initiative_plan_edges_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_initiative_plan_edges
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_work_orders_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_work_orders
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_work_order_bindings_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_work_order_bindings
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_effect_identities_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_effect_identities
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_effect_identity_commits_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_effect_identity_commits
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_plan_mutations_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_plan_mutations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_plan_invalidations_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_plan_invalidations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_plan_preserved_receipts_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_plan_preserved_receipts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_outcome_gate_commits_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_outcome_gate_commits
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
