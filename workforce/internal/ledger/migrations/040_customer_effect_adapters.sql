CREATE TABLE workforce_customer_connections (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    adapter_name TEXT NOT NULL,
    external_adapter_name TEXT NOT NULL,
    external_connection_id TEXT NOT NULL,
    external_connection_version BIGINT NOT NULL CHECK (external_connection_version > 0),
    family TEXT NOT NULL CHECK (
        family IN (
            'email','consented_outbound','social_distribution','crm',
            'sales_pipeline','contract_transmission','customer_onboarding',
            'customer_support','customer_observation'
        )
    ),
    account_id TEXT NOT NULL,
    identity_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, connection_id, version),
    UNIQUE (tenant_id, organization_id, adapter_name, version)
);

CREATE TABLE workforce_customer_connection_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('active','scheduled','revoked','expired')),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
    PRIMARY KEY (tenant_id, organization_id, connection_id),
    FOREIGN KEY (tenant_id, organization_id, connection_id, version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_connection_revocations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    revocation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    reason_code TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, revocation_id),
    UNIQUE (tenant_id, organization_id, connection_id, connection_version),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_scopes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    recipient_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('active','blocked','deleted')),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, customer_id, version),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_scope_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_hash CHAR(64) NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    recipient_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('active','blocked','deleted')),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, customer_id),
    FOREIGN KEY (tenant_id, organization_id, customer_id, version)
        REFERENCES workforce_customer_scopes (
            tenant_id, organization_id, customer_id, version
        )
);

CREATE TABLE workforce_customer_consents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    consent_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    customer_id TEXT NOT NULL,
    customer_version BIGINT NOT NULL CHECK (customer_version > 0),
    recipient_hash CHAR(64) NOT NULL,
    channel TEXT NOT NULL,
    purpose TEXT NOT NULL,
    jurisdiction TEXT NOT NULL,
    basis TEXT NOT NULL CHECK (
        basis IN (
            'explicit_opt_in','contractual','service_communication',
            'documented_legitimate_interest'
        )
    ),
    state TEXT NOT NULL CHECK (state IN ('granted','withdrawn')),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, consent_id, version),
    FOREIGN KEY (tenant_id, organization_id, customer_id, customer_version)
        REFERENCES workforce_customer_scopes (
            tenant_id, organization_id, customer_id, version
        ),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_consent_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    consent_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_hash CHAR(64) NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    customer_id TEXT NOT NULL,
    customer_version BIGINT NOT NULL CHECK (customer_version > 0),
    recipient_hash CHAR(64) NOT NULL,
    channel TEXT NOT NULL,
    purpose TEXT NOT NULL,
    jurisdiction TEXT NOT NULL,
    basis TEXT NOT NULL CHECK (
        basis IN (
            'explicit_opt_in','contractual','service_communication',
            'documented_legitimate_interest'
        )
    ),
    state TEXT NOT NULL CHECK (state IN ('granted','withdrawn')),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, consent_id),
    FOREIGN KEY (tenant_id, organization_id, consent_id, version)
        REFERENCES workforce_customer_consents (
            tenant_id, organization_id, consent_id, version
        )
);

CREATE TABLE workforce_customer_effect_identities (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key
    ),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_effect_attempts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    attempt_kind TEXT NOT NULL CHECK (attempt_kind IN ('dispatch','probe','compensate')),
    request_hash CHAR(64) NOT NULL,
    recipient_hash CHAR(64) NOT NULL,
    counts_frequency BOOLEAN NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('in_flight','completed','failed','ambiguous')),
    safe_code TEXT,
    external_id TEXT,
    observation_hash CHAR(64),
    started_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    CHECK (expires_at > started_at),
    CHECK ((state = 'in_flight') = (finished_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, attempt_id),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_effect_inflight (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at),
    PRIMARY KEY (tenant_id, organization_id, attempt_id),
    FOREIGN KEY (tenant_id, organization_id, attempt_id)
        REFERENCES workforce_customer_effect_attempts (
            tenant_id, organization_id, attempt_id
        )
);

CREATE TABLE workforce_customer_frequency_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    customer_id TEXT NOT NULL,
    recipient_hash CHAR(64) NOT NULL,
    channel TEXT NOT NULL,
    purpose TEXT NOT NULL,
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, event_id),
    UNIQUE (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key
    ),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_drift_exposures (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','reconciled','compensated')),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((state = 'open') = (resolved_at IS NULL)),
    PRIMARY KEY (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key
    ),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_operation_circuits (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('closed','open','half_open')),
    failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    success_count INTEGER NOT NULL DEFAULT 0 CHECK (success_count >= 0),
    window_started_at TIMESTAMPTZ NOT NULL,
    retry_at TIMESTAMPTZ,
    last_safe_code TEXT,
    updated_at TIMESTAMPTZ NOT NULL,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK ((state = 'open') = (retry_at IS NOT NULL)),
    PRIMARY KEY (
        tenant_id, organization_id, connection_id,
        connection_version, operation
    ),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_observations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    recipient_hash CHAR(64) NOT NULL,
    external_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    external_state TEXT NOT NULL CHECK (
        external_state IN (
            'completed','pending','rejected','reversed',
            'drifted','conflicted','unknown'
        )
    ),
    authority TEXT NOT NULL CHECK (
        authority IN (
            'untrusted_external_data','provider_authoritative',
            'control_plane_authoritative'
        )
    ),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    provider_observed_at TIMESTAMPTZ NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, observation_id),
    UNIQUE (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key, external_state, canonical_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_customer_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (
        kind IN (
            'prompt_injection','recipient_substitution','account_confusion',
            'duplicate_communication','consent_withdrawn','unsubscribed',
            'frequency_exhausted','privacy_scope_denied','provider_outage',
            'customer_effect_ambiguity','observation_conflict',
            'capacity_exhausted','circuit_open','compensation_failed'
        )
    ),
    safe_code TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','resolved','escalated')),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((state = 'open') = (resolved_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, incident_id),
    FOREIGN KEY (tenant_id, organization_id, connection_id, connection_version)
        REFERENCES workforce_customer_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE INDEX workforce_customer_connection_expiry_idx
    ON workforce_customer_connection_heads (
        tenant_id, organization_id, expires_at
    ) WHERE state IN ('active','scheduled');
CREATE INDEX workforce_customer_scope_expiry_idx
    ON workforce_customer_scope_heads (
        tenant_id, organization_id, connection_id, state, expires_at
    );
CREATE INDEX workforce_customer_consent_lookup_idx
    ON workforce_customer_consent_heads (
        tenant_id, organization_id, customer_id, recipient_hash,
        channel, purpose, state, expires_at
    );
CREATE INDEX workforce_customer_frequency_window_idx
    ON workforce_customer_frequency_events (
        tenant_id, organization_id, connection_id, connection_version,
        recipient_hash, channel, purpose, occurred_at
    );
CREATE INDEX workforce_customer_inflight_expiry_idx
    ON workforce_customer_effect_inflight (
        tenant_id, organization_id, connection_id,
        connection_version, operation, expires_at
    );
CREATE INDEX workforce_customer_drift_open_idx
    ON workforce_customer_drift_exposures (
        tenant_id, organization_id, connection_id,
        connection_version, operation, created_at
    ) WHERE state = 'open';
CREATE INDEX workforce_customer_circuit_retry_idx
    ON workforce_customer_operation_circuits (
        tenant_id, organization_id, state, retry_at
    ) WHERE state IN ('open','half_open');
CREATE INDEX workforce_customer_observation_lookup_idx
    ON workforce_customer_observations (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key, provider_observed_at
    );
CREATE INDEX workforce_customer_incident_open_idx
    ON workforce_customer_incidents (
        tenant_id, organization_id, connection_id, created_at
    ) WHERE state = 'open';

CREATE TRIGGER workforce_customer_connections_immutable
    BEFORE UPDATE OR DELETE ON workforce_customer_connections
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_customer_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_customer_connection_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_customer_scopes_immutable
    BEFORE UPDATE OR DELETE ON workforce_customer_scopes
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_customer_consents_immutable
    BEFORE UPDATE OR DELETE ON workforce_customer_consents
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_customer_identities_immutable
    BEFORE UPDATE OR DELETE ON workforce_customer_effect_identities
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_customer_frequency_immutable
    BEFORE UPDATE OR DELETE ON workforce_customer_frequency_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_customer_observations_immutable
    BEFORE UPDATE OR DELETE ON workforce_customer_observations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
