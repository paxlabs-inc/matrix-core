CREATE TABLE workforce_commercial_executions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    work_order_hash CHAR(64) NOT NULL,
    product_execution_id TEXT NOT NULL,
    customer_connection_id TEXT NOT NULL,
    customer_connection_version BIGINT NOT NULL CHECK (customer_connection_version > 0),
    financial_connection_id TEXT NOT NULL,
    financial_connection_version BIGINT NOT NULL CHECK (financial_connection_version > 0),
    gate_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    metric_version BIGINT NOT NULL CHECK (metric_version > 0),
    metric_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('pending_external','reconciling','completed','failed')
    ),
    current_phase TEXT NOT NULL CHECK (
        current_phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    version BIGINT NOT NULL CHECK (version > 0),
    idempotency_key TEXT NOT NULL,
    plan_hash CHAR(64) NOT NULL,
    controller_key_id TEXT NOT NULL,
    sealed_plan BYTEA NOT NULL CHECK (octet_length(sealed_plan) > 0),
    deadline TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    UNIQUE (tenant_id, organization_id, plan_hash),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, work_order_id)
        REFERENCES workforce_company_work_orders (
            tenant_id, organization_id, work_order_id
        ),
    FOREIGN KEY (tenant_id, organization_id, product_execution_id)
        REFERENCES workforce_product_executions (
            tenant_id, organization_id, execution_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, customer_connection_id,
        customer_connection_version
    ) REFERENCES workforce_customer_connections (
        tenant_id, organization_id, connection_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, financial_connection_id,
        financial_connection_version
    ) REFERENCES workforce_financial_connections (
        tenant_id, organization_id, connection_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, metric_id, metric_version, metric_hash
    ) REFERENCES workforce_business_metric_definitions (
        tenant_id, organization_id, metric_id, version, definition_hash
    ),
    CHECK (deadline > created_at),
    CHECK (updated_at >= created_at)
);

CREATE INDEX workforce_commercial_executions_state_idx
    ON workforce_commercial_executions (
        tenant_id, organization_id, state, current_phase, updated_at, execution_id
    );

CREATE TABLE workforce_commercial_execution_steps (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    ordinal SMALLINT NOT NULL CHECK (ordinal BETWEEN 1 AND 7),
    state TEXT NOT NULL CHECK (
        state IN ('pending_external','reconciling','completed','failed')
    ),
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    active_evidence_id TEXT,
    safe_code TEXT,
    updated_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, execution_id, phase),
    UNIQUE (tenant_id, organization_id, execution_id, ordinal),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_commercial_executions (
            tenant_id, organization_id, execution_id
        ),
    CHECK ((state = 'completed') = (completed_at IS NOT NULL))
);

CREATE TABLE workforce_commercial_execution_evidence (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    disposition TEXT NOT NULL CHECK (
        disposition IN ('pending_external','reconciling','completed','failed')
    ),
    subject_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    issuer_key_id TEXT NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    reason_code TEXT,
    idempotency_key TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, evidence_id),
    UNIQUE (tenant_id, organization_id, execution_id, evidence_id),
    UNIQUE (tenant_id, organization_id, execution_id, idempotency_key),
    UNIQUE (
        tenant_id, organization_id, execution_id, evidence_id, phase, canonical_hash
    ),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_commercial_executions (
            tenant_id, organization_id, execution_id
        ),
    CHECK (captured_at >= observed_at),
    CHECK ((disposition = 'completed') = (reason_code IS NULL))
);

ALTER TABLE workforce_commercial_execution_steps
    ADD CONSTRAINT workforce_commercial_execution_steps_active_evidence_fk
    FOREIGN KEY (tenant_id, organization_id, execution_id, active_evidence_id)
    REFERENCES workforce_commercial_execution_evidence (
        tenant_id, organization_id, execution_id, evidence_id
    );

CREATE TABLE workforce_commercial_execution_evidence_sources (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (
        role IN (
            'launched_product','acquisition_event','qualified_customer','crm_record',
            'sales_order','contract','financial_intent','financial_settlement',
            'balanced_accounting','support_cycle','metric_definition',
            'metric_observation','commercial_outcome','business_gate','correction_evidence'
        )
    ),
    source_kind TEXT NOT NULL CHECK (
        source_kind IN (
            'product_execution','customer_scope','customer_observation',
            'financial_reservation','financial_observation','financial_accounting',
            'business_metric','business_observation','business_outcome',
            'business_gate','business_correction'
        )
    ),
    source_id_hash CHAR(64) NOT NULL,
    source_version BIGINT NOT NULL CHECK (source_version >= 0),
    content_hash CHAR(64) NOT NULL,
    operation TEXT,
    provider TEXT,
    account_ref_hash CHAR(64),
    external_ref_hash CHAR(64),
    related_id_hash CHAR(64),
    valuation_time TIMESTAMPTZ,
    source_state TEXT NOT NULL CHECK (
        source_state IN (
            'pending','ambiguous','completed','reconciled','failed','reversed','satisfied'
        )
    ),
    authority TEXT NOT NULL CHECK (
        authority IN (
            'untrusted_external_data','internal_verified','provider_authoritative',
            'control_plane_authoritative','reconciled_financial','independent_outcome'
        )
    ),
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, evidence_id, role, source_kind,
        source_id_hash, source_version, content_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, evidence_id)
        REFERENCES workforce_commercial_execution_evidence (
            tenant_id, organization_id, evidence_id
        )
);

CREATE INDEX workforce_commercial_execution_source_hash_idx
    ON workforce_commercial_execution_evidence_sources (
        tenant_id, organization_id, source_kind, source_id_hash, content_hash
    );

CREATE TABLE workforce_commercial_execution_transitions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    transition_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    kind TEXT NOT NULL CHECK (
        kind IN (
            'started','evidence_pending_external','evidence_reconciling',
            'evidence_completed','evidence_failed','correction_applied','recovery_started'
        )
    ),
    from_state TEXT CHECK (
        from_state IN ('pending_external','reconciling','completed','failed')
    ),
    to_state TEXT NOT NULL CHECK (
        to_state IN ('pending_external','reconciling','completed','failed')
    ),
    evidence_id TEXT,
    lease_id TEXT NOT NULL,
    fence BIGINT NOT NULL CHECK (fence > 0),
    seat_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, transition_id),
    UNIQUE (tenant_id, organization_id, execution_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_commercial_executions (
            tenant_id, organization_id, execution_id
        ),
    FOREIGN KEY (tenant_id, organization_id, execution_id, evidence_id)
        REFERENCES workforce_commercial_execution_evidence (
            tenant_id, organization_id, execution_id, evidence_id
        )
);

CREATE TABLE workforce_commercial_execution_corrections (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    target_phase TEXT NOT NULL CHECK (
        target_phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    target_evidence_id TEXT NOT NULL,
    target_hash CHAR(64) NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('invalidate','supersede','retry','compensate')),
    canonical_hash CHAR(64) NOT NULL,
    controller_key_id TEXT NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    reason TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, correction_id),
    UNIQUE (tenant_id, organization_id, execution_id, correction_id, target_phase),
    UNIQUE (tenant_id, organization_id, execution_id, idempotency_key),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_commercial_executions (
            tenant_id, organization_id, execution_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, execution_id, target_evidence_id,
        target_phase, target_hash
    ) REFERENCES workforce_commercial_execution_evidence (
        tenant_id, organization_id, execution_id, evidence_id, phase, canonical_hash
    ),
    CHECK (applied_at >= issued_at)
);

CREATE TABLE workforce_commercial_execution_correction_impacts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    correction_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    evidence_id TEXT NOT NULL,
    evidence_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, correction_id, execution_id, phase, evidence_id
    ),
    FOREIGN KEY (tenant_id, organization_id, correction_id)
        REFERENCES workforce_commercial_execution_corrections (
            tenant_id, organization_id, correction_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, execution_id, evidence_id, phase, evidence_hash
    ) REFERENCES workforce_commercial_execution_evidence (
        tenant_id, organization_id, execution_id, evidence_id, phase, canonical_hash
    )
);

CREATE TABLE workforce_commercial_execution_recoveries (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    recovery_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    target_phase TEXT NOT NULL CHECK (
        target_phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    correction_id TEXT NOT NULL,
    strategy TEXT NOT NULL CHECK (strategy IN ('retry','reconcile','compensate')),
    state TEXT NOT NULL CHECK (state IN ('in_progress','completed','failed')),
    canonical_hash CHAR(64) NOT NULL,
    controller_key_id TEXT NOT NULL,
    sealed_record BYTEA NOT NULL CHECK (octet_length(sealed_record) > 0),
    idempotency_key TEXT NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, recovery_id),
    UNIQUE (tenant_id, organization_id, execution_id, idempotency_key),
    UNIQUE (tenant_id, organization_id, canonical_hash),
    FOREIGN KEY (
        tenant_id, organization_id, execution_id, correction_id, target_phase
    ) REFERENCES workforce_commercial_execution_corrections (
        tenant_id, organization_id, execution_id, correction_id, target_phase
    ),
    CHECK (started_at >= issued_at),
    CHECK ((state = 'in_progress') = (resolved_at IS NULL))
);

CREATE UNIQUE INDEX workforce_commercial_execution_recovery_active_idx
    ON workforce_commercial_execution_recoveries (
        tenant_id, organization_id, execution_id, target_phase
    ) WHERE state = 'in_progress';

CREATE TABLE workforce_commercial_execution_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'acquisition','customer_qualification','sale','financial_intent',
            'financial_reconciliation','support','measurement'
        )
    ),
    kind TEXT NOT NULL CHECK (
        kind IN (
            'external_ambiguity','commercial_step_failed','correction_required',
            'provider_outage','revocation','compensation_failed'
        )
    ),
    safe_code TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','escalated','resolved')),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, incident_id),
    FOREIGN KEY (tenant_id, organization_id, execution_id)
        REFERENCES workforce_commercial_executions (
            tenant_id, organization_id, execution_id
        ),
    CHECK ((state = 'resolved') = (resolved_at IS NOT NULL))
);

CREATE INDEX workforce_commercial_execution_incidents_open_idx
    ON workforce_commercial_execution_incidents (
        tenant_id, organization_id, state, created_at, incident_id
    ) WHERE state IN ('open','escalated');

CREATE TRIGGER workforce_commercial_execution_evidence_immutable
    BEFORE UPDATE OR DELETE ON workforce_commercial_execution_evidence
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_commercial_execution_evidence_sources_immutable
    BEFORE UPDATE OR DELETE ON workforce_commercial_execution_evidence_sources
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_commercial_execution_transitions_immutable
    BEFORE UPDATE OR DELETE ON workforce_commercial_execution_transitions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_commercial_execution_corrections_immutable
    BEFORE UPDATE OR DELETE ON workforce_commercial_execution_corrections
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_commercial_execution_correction_impacts_immutable
    BEFORE UPDATE OR DELETE ON workforce_commercial_execution_correction_impacts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
