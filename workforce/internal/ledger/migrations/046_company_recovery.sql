CREATE TABLE workforce_recovery_limit_policies (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    scope_kind TEXT NOT NULL CHECK (scope_kind IN (
        'company','portfolio','initiative','department','squad','seat','browser',
        'customer','financial_account','mail','storage','process','model','effect'
    )),
    scope_id TEXT NOT NULL,
    resource TEXT NOT NULL CHECK (resource IN (
        'active_initiatives','active_departments','active_seats','active_squads','active_wakes',
        'browser_sessions','model_calls','tool_calls','effects','financial_exposure_microunits',
        'mail_messages','storage_bytes','latency_micros','memory_bytes','cpu_micros',
        'cost_microunits','customer_messages','processes','queue_depth'
    )),
    soft_limit BIGINT NOT NULL CHECK (soft_limit > 0),
    hard_limit BIGINT NOT NULL CHECK (hard_limit >= soft_limit),
    window_micros BIGINT NOT NULL CHECK (window_micros > 0),
    max_reservation_micros BIGINT NOT NULL CHECK (max_reservation_micros > 0),
    open_circuit BOOLEAN NOT NULL,
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_policy BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, policy_id, version),
    UNIQUE (tenant_id, organization_id, scope_kind, scope_id, resource, version)
);

CREATE TABLE workforce_recovery_limit_policy_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    resource TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    content_hash CHAR(64) NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, scope_kind, scope_id, resource),
    FOREIGN KEY (tenant_id, organization_id, policy_id, version)
        REFERENCES workforce_recovery_limit_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_recovery_usage_windows (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    content_hash CHAR(64) NOT NULL,
    window_started_at TIMESTAMPTZ NOT NULL,
    window_ends_at TIMESTAMPTZ NOT NULL,
    used_units BIGINT NOT NULL DEFAULT 0 CHECK (used_units >= 0),
    reserved_units BIGINT NOT NULL DEFAULT 0 CHECK (reserved_units >= 0),
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (window_ends_at > window_started_at),
    PRIMARY KEY (tenant_id, organization_id, policy_id, policy_version, window_started_at),
    FOREIGN KEY (tenant_id, organization_id, policy_id, policy_version)
        REFERENCES workforce_recovery_limit_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_recovery_reservations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    resource TEXT NOT NULL,
    units BIGINT NOT NULL CHECK (units > 0),
    actual_units BIGINT NOT NULL DEFAULT 0 CHECK (actual_units >= 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    irreversible BOOLEAN NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reserved','committed','released','expired','denied')),
    reason_code TEXT NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    reserved_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    finalized_at TIMESTAMPTZ,
    CHECK (expires_at > requested_at),
    CHECK ((state = 'reserved') = (finalized_at IS NULL)),
    CHECK (actual_units <= units),
    PRIMARY KEY (tenant_id, organization_id, reservation_id),
    UNIQUE (tenant_id, organization_id, resource, idempotency_key)
);

CREATE TABLE workforce_recovery_reservation_bindings (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    content_hash CHAR(64) NOT NULL,
    window_started_at TIMESTAMPTZ NOT NULL,
    units BIGINT NOT NULL CHECK (units > 0),
    PRIMARY KEY (tenant_id, organization_id, reservation_id, policy_id, policy_version),
    FOREIGN KEY (tenant_id, organization_id, reservation_id)
        REFERENCES workforce_recovery_reservations (tenant_id, organization_id, reservation_id),
    FOREIGN KEY (tenant_id, organization_id, policy_id, policy_version, window_started_at)
        REFERENCES workforce_recovery_usage_windows (
            tenant_id, organization_id, policy_id, policy_version, window_started_at
        )
);

CREATE INDEX workforce_recovery_reservations_active_idx
    ON workforce_recovery_reservations (tenant_id, organization_id, resource, state, expires_at);

CREATE TABLE workforce_recovery_metrics (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    metric_kind TEXT NOT NULL,
    resource TEXT NOT NULL,
    value BIGINT NOT NULL,
    unit TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_metric BYTEA NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, metric_id)
);

CREATE INDEX workforce_recovery_metrics_time_idx
    ON workforce_recovery_metrics (tenant_id, organization_id, metric_kind, observed_at);

CREATE TABLE workforce_recovery_traces (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    trace_id TEXT NOT NULL,
    parent_id TEXT,
    operation TEXT NOT NULL,
    resource_kind TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('started','succeeded','failed','cancelled','ambiguous')),
    safe_code TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_trace BYTEA NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    CHECK ((status = 'started') = (finished_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, trace_id)
);

CREATE TABLE workforce_recovery_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    incident_kind TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    resource TEXT NOT NULL,
    safe_code TEXT NOT NULL,
    record_kind TEXT NOT NULL,
    record_id TEXT NOT NULL,
    observed_value BIGINT NOT NULL CHECK (observed_value >= 0),
    limit_value BIGINT NOT NULL CHECK (limit_value >= 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_incident BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, incident_id)
);

CREATE INDEX workforce_recovery_incidents_time_idx
    ON workforce_recovery_incidents (tenant_id, organization_id, incident_kind, created_at);

CREATE TABLE workforce_recovery_circuits (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    resource TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','closed')),
    reason_code TEXT NOT NULL,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    opened_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK ((state = 'open') = (closed_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, scope_kind, scope_id, resource)
);

CREATE TABLE workforce_recovery_policies (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    backup_interval_micros BIGINT NOT NULL CHECK (backup_interval_micros > 0),
    rpo_micros BIGINT NOT NULL CHECK (rpo_micros > 0),
    rto_micros BIGINT NOT NULL CHECK (rto_micros > 0),
    pitr_required BOOLEAN NOT NULL,
    max_archive_bytes BIGINT NOT NULL CHECK (max_archive_bytes > 0),
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_policy BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_recovery_retention_rules (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    data_class TEXT NOT NULL CHECK (data_class IN (
        'authority','ledger','graph','mail','receipt','project_brain','company_state',
        'customer','financial','browsing','model_evidence','business_outcome','backup'
    )),
    retention_micros BIGINT NOT NULL CHECK (retention_micros >= 0),
    action TEXT NOT NULL CHECK (action IN ('keep','delete','cryptographic_erase')),
    PRIMARY KEY (tenant_id, organization_id, policy_id, policy_version, data_class),
    FOREIGN KEY (tenant_id, organization_id, policy_id, policy_version)
        REFERENCES workforce_recovery_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_recovery_policy_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    content_hash CHAR(64) NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id),
    FOREIGN KEY (tenant_id, organization_id, policy_id, version)
        REFERENCES workforce_recovery_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_recovery_backups (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    backup_id TEXT NOT NULL,
    scope_kind TEXT NOT NULL CHECK (scope_kind IN ('organization','initiative','customer','project')),
    scope_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('completed','deleted')),
    authorization_hash CHAR(64) NOT NULL,
    manifest_hash CHAR(64) NOT NULL,
    archive_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_authorization BYTEA NOT NULL,
    sealed_manifest BYTEA NOT NULL,
    encrypted_archive BYTEA,
    sealed_archive_key BYTEA,
    key_erased BOOLEAN NOT NULL DEFAULT FALSE,
    wal_lsn TEXT NOT NULL,
    tx_snapshot TEXT NOT NULL,
    pitr_point TEXT,
    snapshot_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    rpo_status TEXT NOT NULL CHECK (rpo_status IN ('baseline','met','breached')),
    erased_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (completed_at >= snapshot_at),
    CHECK ((NOT key_erased) = (sealed_archive_key IS NOT NULL)),
    CHECK ((state = 'deleted') = (encrypted_archive IS NULL)),
    CHECK ((key_erased) = (erased_at IS NOT NULL)),
    PRIMARY KEY (tenant_id, organization_id, backup_id)
);

CREATE INDEX workforce_recovery_backups_time_idx
    ON workforce_recovery_backups (tenant_id, organization_id, completed_at DESC);

CREATE TABLE workforce_recovery_restores (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    restore_id TEXT NOT NULL,
    backup_id TEXT NOT NULL,
    archive_hash CHAR(64) NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('clean','point_in_time')),
    target_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reconciliation_required','ready','failed')),
    authorization_hash CHAR(64) NOT NULL,
    receipt_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_authorization BYTEA NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    restored_tables INTEGER NOT NULL CHECK (restored_tables > 0),
    restored_rows BIGINT NOT NULL CHECK (restored_rows >= 0),
    cancelled_runtime_leases BIGINT NOT NULL CHECK (cancelled_runtime_leases >= 0),
    invalidated_authority_leases BIGINT NOT NULL CHECK (invalidated_authority_leases >= 0),
    coalesced_wakes BIGINT NOT NULL CHECK (coalesced_wakes >= 0),
    quarantined_effects BIGINT NOT NULL CHECK (quarantined_effects >= 0),
    quarantined_external_state BIGINT NOT NULL CHECK (quarantined_external_state >= 0),
    rto_micros BIGINT NOT NULL CHECK (rto_micros > 0),
    rto_status TEXT NOT NULL CHECK (rto_status IN ('met','breached')),
    reconciliation_evidence_hash CHAR(64),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    reconciled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (completed_at >= started_at),
    CHECK ((state = 'ready') = (reconciled_at IS NOT NULL)),
    PRIMARY KEY (tenant_id, organization_id, restore_id)
);

CREATE TABLE workforce_recovery_restore_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    restore_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reconciliation_required','ready','failed')),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id),
    FOREIGN KEY (tenant_id, organization_id, restore_id)
        REFERENCES workforce_recovery_restores (tenant_id, organization_id, restore_id)
);

CREATE TABLE workforce_recovery_qualifications (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    qualification_id TEXT NOT NULL,
    recovery_policy_id TEXT NOT NULL,
    recovery_policy_version BIGINT NOT NULL CHECK (recovery_policy_version > 0),
    recovery_policy_hash CHAR(64) NOT NULL,
    backup_id TEXT NOT NULL,
    backup_manifest_hash CHAR(64) NOT NULL,
    archive_hash CHAR(64) NOT NULL,
    restore_id TEXT NOT NULL,
    restore_receipt_hash CHAR(64) NOT NULL,
    offline_batch_id TEXT NOT NULL,
    offline_receipt_hash CHAR(64) NOT NULL,
    restored_tables INTEGER NOT NULL CHECK (restored_tables > 0),
    restored_rows BIGINT NOT NULL CHECK (restored_rows >= 0),
    cancelled_runtime_leases BIGINT NOT NULL CHECK (cancelled_runtime_leases >= 0),
    invalidated_authority_leases BIGINT NOT NULL CHECK (invalidated_authority_leases >= 0),
    coalesced_wakes BIGINT NOT NULL CHECK (coalesced_wakes >= 0),
    quarantined_effects BIGINT NOT NULL CHECK (quarantined_effects >= 0),
    quarantined_external_state BIGINT NOT NULL CHECK (quarantined_external_state >= 0),
    offline_result_count INTEGER NOT NULL CHECK (offline_result_count > 0),
    offline_reconciliation_count INTEGER NOT NULL CHECK (offline_reconciliation_count = 0),
    rpo_micros BIGINT NOT NULL CHECK (rpo_micros > 0),
    rpo_status TEXT NOT NULL CHECK (rpo_status = 'met'),
    rto_micros BIGINT NOT NULL CHECK (rto_micros > 0),
    rto_status TEXT NOT NULL CHECK (rto_status = 'met'),
    canonical_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_qualification BYTEA NOT NULL,
    qualified_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > qualified_at),
    PRIMARY KEY (tenant_id, organization_id, qualification_id)
);

CREATE TABLE workforce_recovery_qualification_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    qualification_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    recovery_policy_id TEXT NOT NULL,
    recovery_policy_version BIGINT NOT NULL CHECK (recovery_policy_version > 0),
    recovery_policy_hash CHAR(64) NOT NULL,
    qualified_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id),
    FOREIGN KEY (tenant_id, organization_id, qualification_id)
        REFERENCES workforce_recovery_qualifications (
            tenant_id, organization_id, qualification_id
        )
);

CREATE TABLE workforce_recovery_erasures (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    erasure_id TEXT NOT NULL,
    target_kind TEXT NOT NULL CHECK (target_kind IN ('backup','scope')),
    target_id TEXT NOT NULL,
    data_class TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('delete','cryptographic_erase')),
    directive_hash CHAR(64) NOT NULL,
    receipt_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_directive BYTEA NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    destroyed_keys INTEGER NOT NULL CHECK (destroyed_keys >= 0),
    deleted_objects BIGINT NOT NULL CHECK (deleted_objects >= 0),
    executed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, erasure_id)
);

CREATE TABLE workforce_recovery_retention_executions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    backup_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('delete','cryptographic_erase')),
    receipt_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    destroyed_keys INTEGER NOT NULL CHECK (destroyed_keys >= 0),
    deleted_objects BIGINT NOT NULL CHECK (deleted_objects >= 0),
    executed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, execution_id)
);

CREATE TABLE workforce_recovery_machine_keys (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    machine_key_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    machine_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    public_key_hash CHAR(64) NOT NULL,
    content_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    founder_key_id TEXT NOT NULL,
    sealed_registration BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, machine_key_id, version),
    UNIQUE (tenant_id, organization_id, machine_id, version)
);

CREATE TABLE workforce_recovery_machine_key_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    machine_key_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    key_id TEXT NOT NULL,
    content_hash CHAR(64) NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, machine_id),
    FOREIGN KEY (tenant_id, organization_id, machine_key_id, version)
        REFERENCES workforce_recovery_machine_keys (
            tenant_id, organization_id, machine_key_id, version
        )
);

CREATE TABLE workforce_recovery_offline_batches (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    base_backup_id TEXT NOT NULL,
    base_archive_hash CHAR(64) NOT NULL,
    batch_hash CHAR(64) NOT NULL,
    receipt_hash CHAR(64) NOT NULL,
    machine_key_id TEXT NOT NULL,
    runtime_key_id TEXT NOT NULL,
    sealed_batch BYTEA NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    contiguous BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    reconciled_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, batch_id),
    UNIQUE (tenant_id, organization_id, machine_id, sequence)
);

CREATE TABLE workforce_recovery_offline_machine_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    last_sequence BIGINT NOT NULL CHECK (last_sequence > 0),
    last_batch_id TEXT NOT NULL,
    last_batch_hash CHAR(64) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, machine_id)
);

CREATE TABLE workforce_recovery_offline_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    record_kind TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    class TEXT NOT NULL CHECK (class IN ('evidence','observation','receipt','checkpoint')),
    content_hash CHAR(64) NOT NULL,
    payload BYTEA NOT NULL,
    disposition TEXT NOT NULL CHECK (disposition IN (
        'duplicate','accepted','stale','conflict','reconciliation_required'
    )),
    observed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, machine_id, record_kind, record_id, version, content_hash)
);

CREATE TABLE workforce_recovery_offline_record_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    record_kind TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    content_hash CHAR(64) NOT NULL,
    batch_id TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, machine_id, record_kind, record_id)
);

CREATE TABLE workforce_recovery_offline_reconciliation (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    reconciliation_id TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    record_kind TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    offline_hash CHAR(64) NOT NULL,
    current_hash CHAR(64),
    state TEXT NOT NULL CHECK (state IN ('open','resolved','rejected')),
    resolution_id TEXT,
    evidence_hash CHAR(64),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((state = 'open') = (resolved_at IS NULL)),
    CHECK ((state = 'open') = (resolution_id IS NULL)),
    CHECK ((state = 'open') = (evidence_hash IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, reconciliation_id)
);

CREATE TABLE workforce_recovery_offline_resolutions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    resolution_id TEXT NOT NULL,
    reconciliation_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    machine_id TEXT NOT NULL,
    record_kind TEXT NOT NULL,
    record_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    offline_hash CHAR(64) NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('accept_as_evidence','reject','supersede')),
    evidence_hash CHAR(64) NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_resolution BYTEA NOT NULL,
    resolved_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, resolution_id),
    UNIQUE (tenant_id, organization_id, reconciliation_id)
);

CREATE TABLE workforce_recovery_shutdowns (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    shutdown_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('draining','stopped')),
    reason_code TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    key_id TEXT NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    released_reservations BIGINT NOT NULL DEFAULT 0 CHECK (released_reservations >= 0),
    cancelled_leases BIGINT NOT NULL DEFAULT 0 CHECK (cancelled_leases >= 0),
    coalesced_wakes BIGINT NOT NULL DEFAULT 0 CHECK (coalesced_wakes >= 0),
    quarantined_effects BIGINT NOT NULL DEFAULT 0 CHECK (quarantined_effects >= 0),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK ((state = 'draining') = (completed_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, shutdown_id)
);

CREATE TABLE workforce_recovery_shutdown_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    shutdown_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('draining','stopped')),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id),
    FOREIGN KEY (tenant_id, organization_id, shutdown_id)
        REFERENCES workforce_recovery_shutdowns (tenant_id, organization_id, shutdown_id)
);

CREATE TRIGGER workforce_recovery_limit_policies_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_limit_policies
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_reservation_bindings_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_reservation_bindings
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_metrics_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_metrics
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_traces_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_traces
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_incidents_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_incidents
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_policies_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_policies
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_retention_rules_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_retention_rules
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_erasures_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_erasures
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_retention_executions_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_retention_executions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_offline_batches_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_offline_batches
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_offline_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_offline_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_machine_keys_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_machine_keys
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_qualifications_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_qualifications
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_recovery_offline_resolutions_immutable
    BEFORE UPDATE OR DELETE ON workforce_recovery_offline_resolutions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE OR REPLACE FUNCTION workforce_guard_recovery_backup_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invalid Workforce recovery backup mutation'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.tenant_id IS DISTINCT FROM NEW.tenant_id
       OR OLD.organization_id IS DISTINCT FROM NEW.organization_id
       OR OLD.backup_id IS DISTINCT FROM NEW.backup_id
       OR OLD.scope_kind IS DISTINCT FROM NEW.scope_kind
       OR OLD.scope_id IS DISTINCT FROM NEW.scope_id
       OR OLD.authorization_hash IS DISTINCT FROM NEW.authorization_hash
       OR OLD.manifest_hash IS DISTINCT FROM NEW.manifest_hash
       OR OLD.archive_hash IS DISTINCT FROM NEW.archive_hash
       OR OLD.key_id IS DISTINCT FROM NEW.key_id
       OR OLD.sealed_authorization IS DISTINCT FROM NEW.sealed_authorization
       OR OLD.sealed_manifest IS DISTINCT FROM NEW.sealed_manifest
       OR OLD.wal_lsn IS DISTINCT FROM NEW.wal_lsn
       OR OLD.tx_snapshot IS DISTINCT FROM NEW.tx_snapshot
       OR OLD.pitr_point IS DISTINCT FROM NEW.pitr_point
       OR OLD.snapshot_at IS DISTINCT FROM NEW.snapshot_at
       OR OLD.completed_at IS DISTINCT FROM NEW.completed_at
       OR OLD.rpo_status IS DISTINCT FROM NEW.rpo_status
       OR OLD.created_at IS DISTINCT FROM NEW.created_at
       OR OLD.state <> 'completed'
       OR OLD.key_erased
       OR NOT NEW.key_erased
       OR NEW.sealed_archive_key IS NOT NULL
       OR NEW.erased_at IS NULL
       OR NOT (
           (NEW.state = 'completed' AND NEW.encrypted_archive IS NOT DISTINCT FROM OLD.encrypted_archive)
           OR (NEW.state = 'deleted' AND NEW.encrypted_archive IS NULL)
       )
    THEN
        RAISE EXCEPTION 'invalid Workforce recovery backup mutation'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_recovery_backups_guard
    BEFORE UPDATE OR DELETE ON workforce_recovery_backups
    FOR EACH ROW EXECUTE FUNCTION workforce_guard_recovery_backup_mutation();

CREATE OR REPLACE FUNCTION workforce_guard_recovery_restore_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invalid Workforce recovery restore mutation'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.tenant_id IS DISTINCT FROM NEW.tenant_id
       OR OLD.organization_id IS DISTINCT FROM NEW.organization_id
       OR OLD.restore_id IS DISTINCT FROM NEW.restore_id
       OR OLD.backup_id IS DISTINCT FROM NEW.backup_id
       OR OLD.archive_hash IS DISTINCT FROM NEW.archive_hash
       OR OLD.mode IS DISTINCT FROM NEW.mode
       OR OLD.target_at IS DISTINCT FROM NEW.target_at
       OR OLD.authorization_hash IS DISTINCT FROM NEW.authorization_hash
       OR OLD.key_id IS DISTINCT FROM NEW.key_id
       OR OLD.sealed_authorization IS DISTINCT FROM NEW.sealed_authorization
       OR OLD.restored_tables IS DISTINCT FROM NEW.restored_tables
       OR OLD.restored_rows IS DISTINCT FROM NEW.restored_rows
       OR OLD.cancelled_runtime_leases IS DISTINCT FROM NEW.cancelled_runtime_leases
       OR OLD.invalidated_authority_leases IS DISTINCT FROM NEW.invalidated_authority_leases
       OR OLD.coalesced_wakes IS DISTINCT FROM NEW.coalesced_wakes
       OR OLD.quarantined_effects IS DISTINCT FROM NEW.quarantined_effects
       OR OLD.quarantined_external_state IS DISTINCT FROM NEW.quarantined_external_state
       OR OLD.rto_micros IS DISTINCT FROM NEW.rto_micros
       OR OLD.rto_status IS DISTINCT FROM NEW.rto_status
       OR OLD.started_at IS DISTINCT FROM NEW.started_at
       OR OLD.completed_at IS DISTINCT FROM NEW.completed_at
       OR OLD.created_at IS DISTINCT FROM NEW.created_at
       OR OLD.state <> 'reconciliation_required'
       OR NEW.state <> 'ready'
       OR NEW.reconciliation_evidence_hash IS NULL
       OR NEW.reconciled_at IS NULL
    THEN
        RAISE EXCEPTION 'invalid Workforce recovery restore mutation'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_recovery_restores_guard
    BEFORE UPDATE OR DELETE ON workforce_recovery_restores
    FOR EACH ROW EXECUTE FUNCTION workforce_guard_recovery_restore_mutation();

CREATE OR REPLACE FUNCTION workforce_guard_recovery_offline_reconciliation_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invalid Workforce offline reconciliation mutation'
            USING ERRCODE = '55000';
    END IF;
    IF OLD.tenant_id IS DISTINCT FROM NEW.tenant_id
       OR OLD.organization_id IS DISTINCT FROM NEW.organization_id
       OR OLD.reconciliation_id IS DISTINCT FROM NEW.reconciliation_id
       OR OLD.machine_id IS DISTINCT FROM NEW.machine_id
       OR OLD.batch_id IS DISTINCT FROM NEW.batch_id
       OR OLD.record_kind IS DISTINCT FROM NEW.record_kind
       OR OLD.record_id IS DISTINCT FROM NEW.record_id
       OR OLD.version IS DISTINCT FROM NEW.version
       OR OLD.offline_hash IS DISTINCT FROM NEW.offline_hash
       OR OLD.current_hash IS DISTINCT FROM NEW.current_hash
       OR OLD.created_at IS DISTINCT FROM NEW.created_at
       OR OLD.state <> 'open'
       OR NEW.state NOT IN ('resolved','rejected')
       OR NEW.resolution_id IS NULL
       OR NEW.evidence_hash IS NULL
       OR NEW.resolved_at IS NULL
    THEN
        RAISE EXCEPTION 'invalid Workforce offline reconciliation mutation'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_recovery_offline_reconciliation_guard
    BEFORE UPDATE OR DELETE ON workforce_recovery_offline_reconciliation
    FOR EACH ROW EXECUTE FUNCTION workforce_guard_recovery_offline_reconciliation_mutation();
