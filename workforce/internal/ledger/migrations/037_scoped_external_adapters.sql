CREATE TABLE workforce_external_connections (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    adapter_name TEXT NOT NULL,
    family TEXT NOT NULL CHECK (
        family IN (
            'browser_research','browser_authenticated','website','publication',
            'product_analytics','deployment','infrastructure',
            'authoritative_observation'
        )
    ),
    protocol TEXT NOT NULL CHECK (
        protocol IN ('workforce_json_v1','matrix_mcp_2024_11_05')
    ),
    provider TEXT NOT NULL,
    account_id TEXT NOT NULL,
    identity_id TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, connection_id, version),
    UNIQUE (tenant_id, organization_id, adapter_name, version)
);

CREATE TABLE workforce_external_credentials (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    credential_id TEXT NOT NULL,
    credential_kind TEXT NOT NULL CHECK (
        credential_kind IN ('none','bearer','api_key','basic')
    ),
    sealed_credential BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, connection_id,
        connection_version, credential_id
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_external_connection_heads (
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
        REFERENCES workforce_external_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_external_connection_revocations (
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
    UNIQUE (
        tenant_id, organization_id, connection_id, connection_version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_external_identities (
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
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_external_operation_circuits (
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
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_external_operation_attempts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    attempt_kind TEXT NOT NULL CHECK (
        attempt_kind IN ('dispatch','probe','compensate')
    ),
    request_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('in_flight','completed','failed','ambiguous')
    ),
    safe_code TEXT,
    external_id TEXT,
    observation_hash CHAR(64),
    started_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    CHECK (expires_at > started_at),
    CHECK ((state = 'in_flight') = (finished_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, attempt_id),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_external_inflight (
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
        REFERENCES workforce_external_operation_attempts (
            tenant_id, organization_id, attempt_id
        )
);

CREATE TABLE workforce_external_drift_exposures (
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
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_external_observations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
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
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_external_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (
        kind IN (
            'external_ambiguity','provider_outage','redirect_denied',
            'account_confusion','drift_ceiling','capacity_exhausted',
            'credential_revoked','observation_conflict','circuit_open'
        )
    ),
    safe_code TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','resolved','escalated')),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((state = 'open') = (resolved_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, incident_id),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE INDEX workforce_external_head_expiry_idx
    ON workforce_external_connection_heads (
        tenant_id, organization_id, expires_at
    ) WHERE state IN ('active','scheduled');
CREATE INDEX workforce_external_attempt_retry_idx
    ON workforce_external_operation_attempts (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key, started_at
    );
CREATE INDEX workforce_external_circuit_retry_idx
    ON workforce_external_operation_circuits (
        tenant_id, organization_id, state, retry_at
    ) WHERE state IN ('open','half_open');
CREATE INDEX workforce_external_inflight_expiry_idx
    ON workforce_external_inflight (
        tenant_id, organization_id, connection_id, connection_version, expires_at
    );
CREATE INDEX workforce_external_drift_open_idx
    ON workforce_external_drift_exposures (
        tenant_id, organization_id, connection_id, connection_version,
        operation, created_at
    ) WHERE state = 'open';
CREATE INDEX workforce_external_observation_lookup_idx
    ON workforce_external_observations (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key, provider_observed_at
    );
CREATE INDEX workforce_external_incident_open_idx
    ON workforce_external_incidents (
        tenant_id, organization_id, connection_id, created_at
    ) WHERE state = 'open';

CREATE TRIGGER workforce_external_connections_immutable
    BEFORE UPDATE OR DELETE ON workforce_external_connections
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_external_credentials_immutable
    BEFORE UPDATE OR DELETE ON workforce_external_credentials
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_external_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_external_connection_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_external_identities_immutable
    BEFORE UPDATE OR DELETE ON workforce_external_identities
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_external_observations_immutable
    BEFORE UPDATE OR DELETE ON workforce_external_observations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
