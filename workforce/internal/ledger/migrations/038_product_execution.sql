CREATE TABLE workforce_product_executions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    plan_hash CHAR(64) NOT NULL,
    squad_assignment_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'product_queued','design_queued','handoff_verified','build_queued',
            'verify_queued','deployment_queued','deployment_pending',
            'deployment_ambiguous','deployed','telemetry_queued','launch_ready',
            'launched','correction_required','rollback_pending','rolled_back','failed'
        )
    ),
    version BIGINT NOT NULL CHECK (version > 0),
    product_record_id TEXT,
    engineering_record_id TEXT,
    deployment_effect_id TEXT,
    launch_transition_id TEXT,
    checkpoint_version BIGINT NOT NULL DEFAULT 0 CHECK (checkpoint_version >= 0),
    idempotency_key TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_start BYTEA NOT NULL CHECK (octet_length(sealed_start) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id),
    UNIQUE (tenant_id, organization_id, initiative_id),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, plan_id, plan_version)
        REFERENCES workforce_company_initiative_plans (
            tenant_id, organization_id, initiative_id, plan_id, version
        ),
    FOREIGN KEY (tenant_id, organization_id, squad_assignment_id)
        REFERENCES workforce_squad_assignments (
            tenant_id, organization_id, assignment_id
        ),
    FOREIGN KEY (tenant_id, organization_id, product_record_id)
        REFERENCES workforce_product_capability_records (
            tenant_id, organization_id, record_id
        ),
    FOREIGN KEY (tenant_id, organization_id, engineering_record_id)
        REFERENCES workforce_product_capability_records (
            tenant_id, organization_id, record_id
        )
);

CREATE TABLE workforce_product_execution_stage_bindings (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    stage TEXT NOT NULL CHECK (
        stage IN ('product','design','build','verification','deployment','telemetry')
    ),
    plan_node_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    need_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    seat_role TEXT NOT NULL CHECK (seat_role IN ('lead','executor','auditor')),
    mandate_id TEXT NOT NULL,
    mandate_version BIGINT NOT NULL CHECK (mandate_version > 0),
    mandate_digest CHAR(64) NOT NULL,
    goal_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id, stage),
    UNIQUE (tenant_id, organization_id, work_order_id),
    UNIQUE (tenant_id, organization_id, goal_id),
    UNIQUE (tenant_id, organization_id, intent_id),
    UNIQUE (tenant_id, organization_id, wake_id),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_product_executions (
            tenant_id, organization_id, execution_id
        ),
    FOREIGN KEY (tenant_id, organization_id, work_order_id)
        REFERENCES workforce_company_work_orders (
            tenant_id, organization_id, work_order_id
        )
);

CREATE TABLE workforce_product_execution_stage_receipts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    stage TEXT NOT NULL CHECK (
        stage IN ('product','design','build','verification','deployment','telemetry')
    ),
    receipt_id TEXT NOT NULL,
    receipt_hash CHAR(64) NOT NULL,
    verdict_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    accepted_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id, stage),
    UNIQUE (tenant_id, organization_id, receipt_id),
    FOREIGN KEY (tenant_id, organization_id, execution_id, stage)
        REFERENCES workforce_product_execution_stage_bindings (
            tenant_id, organization_id, execution_id, stage
        ),
    FOREIGN KEY (tenant_id, organization_id, receipt_id)
        REFERENCES workforce_execution_receipts (
            tenant_id, organization_id, receipt_id
        ),
    FOREIGN KEY (tenant_id, organization_id, verdict_id)
        REFERENCES workforce_verdict_records (
            tenant_id, organization_id, verdict_id
        )
);

CREATE TABLE workforce_product_execution_effects (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    effect_id TEXT NOT NULL,
    proposal_id TEXT NOT NULL,
    proposal_hash CHAR(64) NOT NULL,
    operation TEXT NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('prepared','ambiguous','committed','consumed','rolled_back','failed')
    ),
    external_id TEXT,
    evidence_hash CHAR(64),
    prepared_at TIMESTAMPTZ NOT NULL,
    reconciled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id, effect_id),
    UNIQUE (tenant_id, organization_id, proposal_id),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_product_executions (
            tenant_id, organization_id, execution_id
        ),
    CHECK ((state IN ('committed','consumed','rolled_back','failed')) = (reconciled_at IS NOT NULL)),
    CHECK ((state IN ('committed','consumed','rolled_back','failed')) = (evidence_hash IS NOT NULL))
);

CREATE TABLE workforce_product_execution_cross_audits (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    epoch_id TEXT NOT NULL,
    original_verdict_id TEXT NOT NULL,
    reaudit_verdict_id TEXT NOT NULL,
    disagreement BOOLEAN NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, execution_id, epoch_id, original_verdict_id
    ),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_product_executions (
            tenant_id, organization_id, execution_id
        ),
    FOREIGN KEY (tenant_id, organization_id, epoch_id, original_verdict_id)
        REFERENCES workforce_cross_audit_results (
            tenant_id, organization_id, epoch_id, original_verdict_id
        )
);

CREATE TABLE workforce_product_execution_correction_bindings (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    transition_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    snapshot_version BIGINT NOT NULL CHECK (snapshot_version > 0),
    snapshot_hash CHAR(64) NOT NULL,
    unresolved_material_count INTEGER NOT NULL CHECK (unresolved_material_count >= 0),
    unresolved_contaminated_count INTEGER NOT NULL CHECK (unresolved_contaminated_count >= 0),
    checked_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id, transition_id),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_product_executions (
            tenant_id, organization_id, execution_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, snapshot_id, snapshot_version
    ) REFERENCES workforce_company_correction_snapshots (
        tenant_id, organization_id, snapshot_id, version
    )
);

CREATE TABLE workforce_product_execution_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    phase TEXT NOT NULL,
    event_kind TEXT NOT NULL,
    stage TEXT,
    source_id TEXT,
    idempotency_key TEXT NOT NULL,
    content_hash CHAR(64) NOT NULL,
    sealed_event BYTEA NOT NULL CHECK (octet_length(sealed_event) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id, sequence),
    UNIQUE (tenant_id, organization_id, execution_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_product_executions (
            tenant_id, organization_id, execution_id
        )
);

CREATE INDEX workforce_product_executions_phase_idx
    ON workforce_product_executions (
        tenant_id, organization_id, phase, updated_at, execution_id
    );

CREATE INDEX workforce_product_execution_events_created_idx
    ON workforce_product_execution_events (
        tenant_id, organization_id, execution_id, created_at, sequence
    );

CREATE TRIGGER workforce_product_execution_stage_bindings_immutable
    BEFORE UPDATE OR DELETE ON workforce_product_execution_stage_bindings
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_product_execution_stage_receipts_immutable
    BEFORE UPDATE OR DELETE ON workforce_product_execution_stage_receipts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_product_execution_cross_audits_immutable
    BEFORE UPDATE OR DELETE ON workforce_product_execution_cross_audits
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_product_execution_correction_bindings_immutable
    BEFORE UPDATE OR DELETE ON workforce_product_execution_correction_bindings
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_product_execution_events_immutable
    BEFORE UPDATE OR DELETE ON workforce_product_execution_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
