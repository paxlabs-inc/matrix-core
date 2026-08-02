CREATE TABLE workforce_capability_definitions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    capability_kind TEXT NOT NULL CHECK (
        capability_kind IN (
            'analysis','decision','execution','observation','effect_proposal','verification'
        )
    ),
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_definition BYTEA NOT NULL CHECK (octet_length(sealed_definition) > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, capability_id, version),
    CHECK (expires_at IS NULL OR expires_at > effective_at)
);

CREATE TABLE workforce_capability_definition_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    capability_id TEXT NOT NULL,
    latest_version BIGINT NOT NULL CHECK (latest_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, capability_id),
    FOREIGN KEY (tenant_id, organization_id, capability_id, latest_version)
        REFERENCES workforce_capability_definitions (
            tenant_id, organization_id, capability_id, version
        )
);

CREATE TABLE workforce_organization_templates (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    template_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    owner_id TEXT NOT NULL,
    template_mode TEXT NOT NULL CHECK (
        template_mode IN ('legacy_v1_projection','full_company')
    ),
    department_count SMALLINT NOT NULL CHECK (department_count BETWEEN 1 AND 16),
    seat_count SMALLINT NOT NULL CHECK (
        seat_count BETWEEN 3 AND 48 AND seat_count = department_count * 3
    ),
    capability_registry_digest CHAR(64) NOT NULL,
    receipt_schema_versions TEXT[] NOT NULL CHECK (cardinality(receipt_schema_versions) > 0),
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_template BYTEA NOT NULL CHECK (octet_length(sealed_template) > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, template_id, version),
    CHECK (expires_at IS NULL OR expires_at > effective_at)
);

CREATE TABLE workforce_organization_template_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    template_id TEXT NOT NULL,
    latest_version BIGINT NOT NULL CHECK (latest_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, template_id),
    FOREIGN KEY (tenant_id, organization_id, template_id, latest_version)
        REFERENCES workforce_organization_templates (
            tenant_id, organization_id, template_id, version
        )
);

CREATE TABLE workforce_organization_seat_mandates (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    mandate_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    seat_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    seat_role TEXT NOT NULL CHECK (seat_role IN ('lead','executor','auditor')),
    mandate_origin TEXT NOT NULL CHECK (
        mandate_origin IN ('legacy_v1_projection','owner_native_v2')
    ),
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_mandate BYTEA NOT NULL CHECK (octet_length(sealed_mandate) > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, mandate_id, version),
    UNIQUE (tenant_id, organization_id, mandate_id, version, canonical_hash),
    CHECK (expires_at IS NULL OR expires_at > effective_at)
);

CREATE TABLE workforce_organization_seat_mandate_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    mandate_id TEXT NOT NULL,
    latest_version BIGINT NOT NULL CHECK (latest_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, mandate_id),
    FOREIGN KEY (tenant_id, organization_id, mandate_id, latest_version)
        REFERENCES workforce_organization_seat_mandates (
            tenant_id, organization_id, mandate_id, version
        )
);

CREATE TABLE workforce_organization_template_departments (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    template_id TEXT NOT NULL,
    template_version BIGINT NOT NULL CHECK (template_version > 0),
    department_id TEXT NOT NULL,
    department_key TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, template_id, template_version, department_id
    ),
    UNIQUE (
        tenant_id, organization_id, template_id, template_version, department_key
    ),
    FOREIGN KEY (tenant_id, organization_id, template_id, template_version)
        REFERENCES workforce_organization_templates (
            tenant_id, organization_id, template_id, version
        )
);

CREATE TABLE workforce_organization_template_seats (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    template_id TEXT NOT NULL,
    template_version BIGINT NOT NULL CHECK (template_version > 0),
    department_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    seat_role TEXT NOT NULL CHECK (seat_role IN ('lead','executor','auditor')),
    mandate_id TEXT NOT NULL,
    mandate_version BIGINT NOT NULL CHECK (mandate_version > 0),
    mandate_digest CHAR(64) NOT NULL,
    binding_id TEXT NOT NULL,
    binding_version BIGINT NOT NULL CHECK (binding_version > 0),
    independence_domain TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, template_id, template_version, seat_id
    ),
    UNIQUE (
        tenant_id, organization_id, template_id, template_version,
        department_id, seat_role
    ),
    FOREIGN KEY (
        tenant_id, organization_id, template_id, template_version, department_id
    ) REFERENCES workforce_organization_template_departments (
        tenant_id, organization_id, template_id, template_version, department_id
    ),
    FOREIGN KEY (
        tenant_id, organization_id, mandate_id, mandate_version, mandate_digest
    ) REFERENCES workforce_organization_seat_mandates (
        tenant_id, organization_id, mandate_id, version, canonical_hash
    )
);

CREATE TABLE workforce_organization_migrations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    migration_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    owner_id TEXT NOT NULL,
    from_template_id TEXT NOT NULL,
    from_template_version BIGINT NOT NULL CHECK (from_template_version > 0),
    to_template_id TEXT NOT NULL,
    to_template_version BIGINT NOT NULL CHECK (to_template_version > 0),
    manifest_digest CHAR(64) NOT NULL,
    capability_registry_digest CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_manifest BYTEA NOT NULL CHECK (octet_length(sealed_manifest) > 0),
    prepared_at TIMESTAMPTZ NOT NULL,
    activate_not_before TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, migration_id, version),
    UNIQUE (tenant_id, organization_id, manifest_digest),
    FOREIGN KEY (
        tenant_id, organization_id, to_template_id, to_template_version
    ) REFERENCES workforce_organization_templates (
        tenant_id, organization_id, template_id, version
    ),
    CHECK (activate_not_before >= prepared_at),
    CHECK (expires_at > activate_not_before)
);

CREATE TABLE workforce_organization_migration_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    migration_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    state TEXT NOT NULL CHECK (state IN ('staged','activated','rolled_back')),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, migration_id),
    FOREIGN KEY (tenant_id, organization_id, migration_id, version)
        REFERENCES workforce_organization_migrations (
            tenant_id, organization_id, migration_id, version
        )
);

CREATE UNIQUE INDEX workforce_one_staged_organization_migration
    ON workforce_organization_migration_heads (tenant_id, organization_id)
    WHERE state = 'staged';

CREATE TABLE workforce_organization_migration_activations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    activation_id TEXT NOT NULL,
    migration_id TEXT NOT NULL,
    migration_version BIGINT NOT NULL CHECK (migration_version > 0),
    manifest_digest CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_activation BYTEA NOT NULL CHECK (octet_length(sealed_activation) > 0),
    activated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, activation_id),
    UNIQUE (tenant_id, organization_id, migration_id, migration_version),
    FOREIGN KEY (tenant_id, organization_id, migration_id, migration_version)
        REFERENCES workforce_organization_migrations (
            tenant_id, organization_id, migration_id, version
        )
);

CREATE TABLE workforce_organization_migration_rollbacks (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    rollback_id TEXT NOT NULL,
    migration_id TEXT NOT NULL,
    migration_version BIGINT NOT NULL CHECK (migration_version > 0),
    manifest_digest CHAR(64) NOT NULL,
    reason TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_rollback BYTEA NOT NULL CHECK (octet_length(sealed_rollback) > 0),
    rolled_back_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, rollback_id),
    UNIQUE (tenant_id, organization_id, migration_id, migration_version),
    FOREIGN KEY (tenant_id, organization_id, migration_id, migration_version)
        REFERENCES workforce_organization_migrations (
            tenant_id, organization_id, migration_id, version
        )
);

CREATE TABLE workforce_organization_migration_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    migration_id TEXT NOT NULL,
    migration_version BIGINT NOT NULL CHECK (migration_version > 0),
    event_id TEXT NOT NULL,
    event_kind TEXT NOT NULL CHECK (event_kind IN ('staged','activated','rolled_back')),
    event_hash CHAR(64) NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, migration_id, migration_version, event_id),
    FOREIGN KEY (tenant_id, organization_id, migration_id, migration_version)
        REFERENCES workforce_organization_migrations (
            tenant_id, organization_id, migration_id, version
        )
);

CREATE TABLE workforce_organization_template_activations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    activation_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    from_template_id TEXT NOT NULL,
    from_template_version BIGINT NOT NULL CHECK (from_template_version > 0),
    to_template_id TEXT NOT NULL,
    to_template_version BIGINT NOT NULL CHECK (to_template_version > 0),
    expected_projection_version BIGINT NOT NULL CHECK (expected_projection_version > 0),
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_activation BYTEA NOT NULL CHECK (octet_length(sealed_activation) > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, activation_id),
    FOREIGN KEY (
        tenant_id, organization_id, from_template_id, from_template_version
    ) REFERENCES workforce_organization_templates (
        tenant_id, organization_id, template_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, to_template_id, to_template_version
    ) REFERENCES workforce_organization_templates (
        tenant_id, organization_id, template_id, version
    ),
    CHECK (expires_at > effective_at),
    CHECK (activated_at >= effective_at AND activated_at < expires_at)
);

CREATE TABLE workforce_active_organization_template (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    schema_version TEXT NOT NULL CHECK (
        schema_version = 'workforce.organization-template.v2'
    ),
    template_id TEXT NOT NULL,
    template_version BIGINT NOT NULL CHECK (template_version > 0),
    activation_kind TEXT NOT NULL CHECK (activation_kind IN ('migration','template')),
    activation_id TEXT NOT NULL,
    migration_id TEXT,
    migration_version BIGINT CHECK (migration_version > 0),
    projection_version BIGINT NOT NULL CHECK (projection_version > 0),
    activated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id),
    FOREIGN KEY (tenant_id, organization_id, template_id, template_version)
        REFERENCES workforce_organization_templates (
            tenant_id, organization_id, template_id, version
        ),
    FOREIGN KEY (tenant_id, organization_id, migration_id, migration_version)
        REFERENCES workforce_organization_migrations (
            tenant_id, organization_id, migration_id, version
        ),
    CHECK (
        (activation_kind = 'migration' AND migration_id IS NOT NULL AND migration_version IS NOT NULL)
        OR
        (activation_kind = 'template' AND migration_id IS NULL AND migration_version IS NULL)
    )
);

CREATE TABLE workforce_squad_seat_runtime_states (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    runtime_state_id TEXT NOT NULL,
    template_id TEXT NOT NULL,
    template_version BIGINT NOT NULL CHECK (template_version > 0),
    mandate_digest CHAR(64) NOT NULL,
    availability TEXT NOT NULL CHECK (availability IN ('available','unavailable')),
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_state BYTEA NOT NULL CHECK (octet_length(sealed_state) > 0),
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, seat_id, version),
    UNIQUE (tenant_id, organization_id, runtime_state_id),
    FOREIGN KEY (tenant_id, organization_id, template_id, template_version, seat_id)
        REFERENCES workforce_organization_template_seats (
            tenant_id, organization_id, template_id, template_version, seat_id
        ),
    CHECK (expires_at > observed_at)
);

CREATE TABLE workforce_squad_seat_runtime_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    latest_version BIGINT NOT NULL CHECK (latest_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, seat_id),
    FOREIGN KEY (tenant_id, organization_id, seat_id, latest_version)
        REFERENCES workforce_squad_seat_runtime_states (
            tenant_id, organization_id, seat_id, version
        )
);

CREATE TABLE workforce_squad_assignments (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    assignment_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    lifecycle_stage TEXT NOT NULL CHECK (
        lifecycle_stage IN (
            'DISCOVER','SCREEN','VALIDATE','DECIDE','FUND','DESIGN','BUILD','VERIFY',
            'LAUNCH','ACQUIRE','MONETIZE','OPERATE','MEASURE','SCALE','PIVOT',
            'MAINTAIN','TERMINATE','PAUSED'
        )
    ),
    template_id TEXT NOT NULL,
    template_version BIGINT NOT NULL CHECK (template_version > 0),
    template_digest CHAR(64) NOT NULL,
    requirement_digest CHAR(64) NOT NULL,
    graph_scopes TEXT[] NOT NULL CHECK (cardinality(graph_scopes) > 0),
    conflict_domains TEXT[] NOT NULL,
    member_count SMALLINT NOT NULL CHECK (member_count BETWEEN 2 AND 48),
    authority_effect TEXT NOT NULL CHECK (authority_effect = 'none'),
    receipt_schema_versions TEXT[] NOT NULL CHECK (cardinality(receipt_schema_versions) > 0),
    idempotency_key TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    signature_key_id TEXT NOT NULL,
    sealed_assignment BYTEA NOT NULL CHECK (octet_length(sealed_assignment) > 0),
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, assignment_id),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, template_id, template_version)
        REFERENCES workforce_organization_templates (
            tenant_id, organization_id, template_id, version
        ),
    CHECK (expires_at > issued_at),
    CHECK (expires_at <= issued_at + INTERVAL '24 hours')
);

CREATE TABLE workforce_squad_assignment_members (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    assignment_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    seat_role TEXT NOT NULL CHECK (seat_role IN ('lead','executor','auditor')),
    mandate_id TEXT NOT NULL,
    mandate_version BIGINT NOT NULL CHECK (mandate_version > 0),
    mandate_digest CHAR(64) NOT NULL,
    binding_id TEXT NOT NULL,
    binding_version BIGINT NOT NULL CHECK (binding_version > 0),
    independence_domain TEXT NOT NULL,
    need_ids TEXT[] NOT NULL CHECK (cardinality(need_ids) > 0),
    model_calls BIGINT NOT NULL CHECK (model_calls >= 0),
    tool_calls BIGINT NOT NULL CHECK (tool_calls >= 0),
    effect_dispatches INTEGER NOT NULL CHECK (effect_dispatches >= 0),
    memory_bytes BIGINT NOT NULL CHECK (memory_bytes >= 0),
    cost_minor BIGINT NOT NULL CHECK (cost_minor >= 0),
    currency TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, assignment_id, seat_id),
    FOREIGN KEY (tenant_id, organization_id, assignment_id)
        REFERENCES workforce_squad_assignments (
            tenant_id, organization_id, assignment_id
        )
);

CREATE TABLE workforce_squad_assignment_revocations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    assignment_id TEXT NOT NULL,
    activation_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, assignment_id),
    FOREIGN KEY (tenant_id, organization_id, assignment_id)
        REFERENCES workforce_squad_assignments (
            tenant_id, organization_id, assignment_id
        ),
    FOREIGN KEY (tenant_id, organization_id, activation_id)
        REFERENCES workforce_organization_template_activations (
            tenant_id, organization_id, activation_id
        )
);

CREATE INDEX workforce_active_squad_assignments_idx
    ON workforce_squad_assignments (tenant_id, organization_id, expires_at);

CREATE INDEX workforce_active_squad_seat_reservations_idx
    ON workforce_squad_assignment_members (tenant_id, organization_id, seat_id);

CREATE TRIGGER workforce_capability_definitions_immutable
    BEFORE UPDATE OR DELETE ON workforce_capability_definitions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_templates_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_templates
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_seat_mandates_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_seat_mandates
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_template_departments_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_template_departments
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_template_seats_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_template_seats
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_migrations_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_migrations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_migration_activations_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_migration_activations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_migration_rollbacks_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_migration_rollbacks
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_migration_events_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_migration_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_organization_template_activations_immutable
    BEFORE UPDATE OR DELETE ON workforce_organization_template_activations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_squad_seat_runtime_states_immutable
    BEFORE UPDATE OR DELETE ON workforce_squad_seat_runtime_states
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_squad_assignments_immutable
    BEFORE UPDATE OR DELETE ON workforce_squad_assignments
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_squad_assignment_members_immutable
    BEFORE UPDATE OR DELETE ON workforce_squad_assignment_members
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_squad_assignment_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_squad_assignment_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
