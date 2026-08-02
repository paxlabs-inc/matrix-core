CREATE TABLE workforce_autonomous_company_property_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    property_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    property_kind TEXT NOT NULL CHECK (
        property_kind IN (
            'COMPANY-CONTROL','PRODUCT-EXECUTION',
            'COMMERCIAL-EXECUTION','AUTONOMOUS-COMPANY'
        )
    ),
    initiative_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('pending','running','blocked','passed','failed','uncertain')
    ),
    request_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    key_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    evaluated_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, property_id, version),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK (evaluated_at >= started_at),
    CHECK (fresh_until > evaluated_at),
    CHECK ((state IN ('passed','failed')) = (completed_at IS NOT NULL)),
    CHECK (completed_at IS NULL OR completed_at >= evaluated_at)
);

CREATE TABLE workforce_autonomous_company_property_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    property_id TEXT NOT NULL,
    property_kind TEXT NOT NULL CHECK (
        property_kind IN (
            'COMPANY-CONTROL','PRODUCT-EXECUTION',
            'COMMERCIAL-EXECUTION','AUTONOMOUS-COMPANY'
        )
    ),
    initiative_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    state TEXT NOT NULL CHECK (
        state IN ('pending','running','blocked','passed','failed','uncertain')
    ),
    canonical_hash CHAR(64) NOT NULL,
    evaluated_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, property_kind, initiative_id),
    UNIQUE (tenant_id, organization_id, property_id),
    FOREIGN KEY (tenant_id, organization_id, property_id, version)
        REFERENCES workforce_autonomous_company_property_records (
            tenant_id, organization_id, property_id, version
        ),
    CHECK (fresh_until > evaluated_at)
);

CREATE INDEX workforce_autonomous_company_property_state_idx
    ON workforce_autonomous_company_property_heads (
        tenant_id, organization_id, state, fresh_until, updated_at DESC
    );

CREATE TABLE workforce_autonomous_company_property_evidence (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    property_id TEXT NOT NULL,
    property_version BIGINT NOT NULL CHECK (property_version > 0),
    evidence_kind TEXT NOT NULL CHECK (
        evidence_kind IN (
            'mission_authority','company_funding','company_lifecycle','mail_receipt',
            'approval_receipt','product_execution','deployment_receipt',
            'independent_audit','commercial_execution','customer_transaction',
            'customer_operation','financial_reconciliation','business_outcome',
            'learning_conclusion','company_control_property',
            'product_execution_property','commercial_execution_property',
            'security_qualification','recovery_qualification','clean_restore_receipt',
            'external_ambiguity_receipt','financial_ambiguity_receipt',
            'correction_receipt','cross_audit_receipt','offline_coalescing_receipt',
            'restart_receipt','fresh_process_receipt','memoryless_auditor_receipt',
            'founder_ui_projection_receipt',
            'next_cycle_dispatch_receipt','next_cycle_completion_receipt'
        )
    ),
    initiative_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    record_version BIGINT NOT NULL CHECK (record_version > 0),
    record_hash CHAR(64) NOT NULL,
    authority TEXT NOT NULL,
    source_state TEXT NOT NULL,
    validity TEXT NOT NULL CHECK (
        validity IN ('active','contested','superseded','retracted','expired')
    ),
    reconciliation TEXT NOT NULL CHECK (
        reconciliation IN ('not_applicable','reconciled','unreconciled','ambiguous')
    ),
    contaminated BOOLEAN NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, property_id, property_version,
        evidence_kind, record_id, record_version
    ),
    FOREIGN KEY (tenant_id, organization_id, property_id, property_version)
        REFERENCES workforce_autonomous_company_property_records (
            tenant_id, organization_id, property_id, version
        ),
    CHECK (fresh_until > observed_at)
);

CREATE INDEX workforce_autonomous_company_evidence_source_idx
    ON workforce_autonomous_company_property_evidence (
        tenant_id, organization_id, initiative_id, evidence_kind,
        record_id, record_version, record_hash
    );

CREATE TABLE workforce_autonomous_company_property_lineage (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    property_id TEXT NOT NULL,
    property_version BIGINT NOT NULL CHECK (property_version > 0),
    position SMALLINT NOT NULL CHECK (position > 0),
    stage TEXT NOT NULL CHECK (
        stage IN ('mission','funding','product','commercial','outcome','learning','next_cycle')
    ),
    record_id TEXT NOT NULL,
    record_version BIGINT NOT NULL CHECK (record_version > 0),
    record_hash CHAR(64) NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, property_id, property_version, position
    ),
    UNIQUE (
        tenant_id, organization_id, property_id, property_version, stage
    ),
    FOREIGN KEY (tenant_id, organization_id, property_id, property_version)
        REFERENCES workforce_autonomous_company_property_records (
            tenant_id, organization_id, property_id, version
        )
);

CREATE TABLE workforce_autonomous_company_property_processes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    property_id TEXT NOT NULL,
    property_version BIGINT NOT NULL CHECK (property_version > 0),
    process_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('executor','auditor')),
    memoryless BOOLEAN NOT NULL,
    fresh_process BOOLEAN NOT NULL,
    evidence_id TEXT NOT NULL,
    evidence_version BIGINT NOT NULL CHECK (evidence_version > 0),
    evidence_hash CHAR(64) NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, property_id, property_version, process_id
    ),
    UNIQUE (
        tenant_id, organization_id, property_id, property_version, wake_id
    ),
    FOREIGN KEY (tenant_id, organization_id, property_id, property_version)
        REFERENCES workforce_autonomous_company_property_records (
            tenant_id, organization_id, property_id, version
    ),
    CHECK (observed_at >= started_at),
    CHECK (role <> 'auditor' OR memoryless)
);

CREATE TABLE workforce_autonomous_company_next_cycle_plans (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    conclusion_id TEXT NOT NULL,
    conclusion_hash CHAR(64) NOT NULL,
    hypothesis_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    selected_action TEXT NOT NULL CHECK (
        selected_action IN (
            'SCALE','PIVOT','MAINTAIN','TERMINATE','DISCOVER','REQUIRES_HUMAN'
        )
    ),
    portfolio_feedback_id TEXT NOT NULL,
    due_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_plan BYTEA NOT NULL CHECK (octet_length(sealed_plan) > 0),
    key_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, plan_id),
    UNIQUE (tenant_id, organization_id, conclusion_id),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, conclusion_id)
        REFERENCES workforce_learning_records (
            tenant_id, organization_id, record_id
        ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK (claimed_at >= due_at)
);

CREATE TABLE workforce_autonomous_company_next_cycle_operations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    sequence SMALLINT NOT NULL CHECK (sequence > 0),
    operation_kind TEXT NOT NULL CHECK (
        operation_kind IN (
            'lifecycle_transition','company_runtime_discovery','founder_required'
        )
    ),
    from_state TEXT,
    to_state TEXT,
    decision TEXT,
    runtime_action TEXT,
    PRIMARY KEY (tenant_id, organization_id, plan_id, sequence),
    FOREIGN KEY (tenant_id, organization_id, plan_id)
        REFERENCES workforce_autonomous_company_next_cycle_plans (
            tenant_id, organization_id, plan_id
        ),
    CHECK (
        (operation_kind = 'lifecycle_transition'
            AND from_state IS NOT NULL AND to_state IS NOT NULL
            AND decision IS NOT NULL AND runtime_action IS NULL)
        OR
        (operation_kind = 'company_runtime_discovery'
            AND from_state IS NULL AND to_state IS NULL AND decision IS NULL
            AND runtime_action = 'DISCOVER')
        OR
        (operation_kind = 'founder_required'
            AND from_state IS NULL AND to_state IS NULL AND decision IS NULL
            AND runtime_action = 'REQUIRES_HUMAN')
    )
);

CREATE TABLE workforce_autonomous_company_next_cycle_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    conclusion_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('planned','running','blocked','passed','failed','uncertain')
    ),
    last_event_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_event_sequence >= 0),
    last_event_id TEXT,
    last_event_hash CHAR(64),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, plan_id),
    UNIQUE (tenant_id, organization_id, conclusion_id),
    FOREIGN KEY (tenant_id, organization_id, plan_id)
        REFERENCES workforce_autonomous_company_next_cycle_plans (
            tenant_id, organization_id, plan_id
        ),
    CHECK (
        (last_event_sequence = 0 AND last_event_id IS NULL
            AND last_event_hash IS NULL AND state = 'planned')
        OR
        (last_event_sequence > 0 AND last_event_id IS NOT NULL
            AND last_event_hash IS NOT NULL AND state <> 'planned')
    )
);

CREATE INDEX workforce_autonomous_company_next_cycle_state_idx
    ON workforce_autonomous_company_next_cycle_heads (
        tenant_id, organization_id, state, updated_at, plan_id
    );

CREATE TABLE workforce_autonomous_company_next_cycle_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    initiative_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('running','blocked','passed','failed','uncertain')
    ),
    canonical_hash CHAR(64) NOT NULL,
    sealed_event BYTEA NOT NULL CHECK (octet_length(sealed_event) > 0),
    key_id TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, event_id),
    UNIQUE (tenant_id, organization_id, plan_id, sequence),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, plan_id)
        REFERENCES workforce_autonomous_company_next_cycle_plans (
            tenant_id, organization_id, plan_id
        )
);

ALTER TABLE workforce_autonomous_company_next_cycle_heads
    ADD FOREIGN KEY (
        tenant_id, organization_id, last_event_id
    ) REFERENCES workforce_autonomous_company_next_cycle_events (
        tenant_id, organization_id, event_id
    );

CREATE TABLE workforce_autonomous_company_next_cycle_event_evidence (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    evidence_kind TEXT NOT NULL CHECK (
        evidence_kind IN (
            'mission_authority','company_funding','company_lifecycle','mail_receipt',
            'approval_receipt','product_execution','deployment_receipt',
            'independent_audit','commercial_execution','customer_transaction',
            'customer_operation','financial_reconciliation','business_outcome',
            'learning_conclusion','company_control_property',
            'product_execution_property','commercial_execution_property',
            'security_qualification','recovery_qualification','clean_restore_receipt',
            'external_ambiguity_receipt','financial_ambiguity_receipt',
            'correction_receipt','cross_audit_receipt','offline_coalescing_receipt',
            'restart_receipt','fresh_process_receipt','memoryless_auditor_receipt',
            'founder_ui_projection_receipt',
            'next_cycle_dispatch_receipt','next_cycle_completion_receipt'
        )
    ),
    initiative_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    record_version BIGINT NOT NULL CHECK (record_version > 0),
    record_hash CHAR(64) NOT NULL,
    authority TEXT NOT NULL,
    source_state TEXT NOT NULL,
    validity TEXT NOT NULL CHECK (
        validity IN ('active','contested','superseded','retracted','expired')
    ),
    reconciliation TEXT NOT NULL CHECK (
        reconciliation IN ('not_applicable','reconciled','unreconciled','ambiguous')
    ),
    contaminated BOOLEAN NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, event_id,
        evidence_kind, record_id, record_version
    ),
    FOREIGN KEY (tenant_id, organization_id, event_id)
        REFERENCES workforce_autonomous_company_next_cycle_events (
            tenant_id, organization_id, event_id
        ),
    CHECK (fresh_until > observed_at)
);

CREATE TRIGGER workforce_autonomous_company_property_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_property_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_autonomous_company_property_evidence_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_property_evidence
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_autonomous_company_property_lineage_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_property_lineage
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_autonomous_company_property_processes_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_property_processes
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_autonomous_company_next_cycle_plans_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_next_cycle_plans
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_autonomous_company_next_cycle_operations_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_next_cycle_operations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_autonomous_company_next_cycle_events_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_next_cycle_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_autonomous_company_next_cycle_event_evidence_immutable
    BEFORE UPDATE OR DELETE ON workforce_autonomous_company_next_cycle_event_evidence
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
