ALTER TABLE workforce_external_connections
    DROP CONSTRAINT IF EXISTS workforce_external_connections_family_check;

ALTER TABLE workforce_external_connections
    ADD CONSTRAINT workforce_external_connections_family_check CHECK (
        family IN (
            'browser_research','browser_authenticated','website','publication',
            'product_analytics','deployment','infrastructure',
            'authoritative_observation','financial_transport'
        )
    );

CREATE TABLE workforce_financial_connections (
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
            'paxeer','layerx','billing','invoicing','payment','collection',
            'treasury','trading','settlement','transfer','reconciliation'
        )
    ),
    account_id TEXT NOT NULL,
    identity_id TEXT NOT NULL,
    base_currency TEXT NOT NULL,
    provider_contract_kind TEXT NOT NULL CHECK (
        provider_contract_kind IN (
            'paxeer_evm_json_rpc','layerx_account_v1',
            'billing_ledger_v1','treasury_ledger_v1'
        )
    ),
    network_id TEXT NOT NULL,
    chain_id BIGINT NOT NULL CHECK (chain_id >= 0),
    settlement_contract TEXT NOT NULL,
    contract_version TEXT NOT NULL,
    required_confirmations INTEGER NOT NULL CHECK (required_confirmations >= 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, connection_id, version),
    UNIQUE (tenant_id, organization_id, adapter_name, version),
    FOREIGN KEY (
        tenant_id, organization_id, external_connection_id,
        external_connection_version
    ) REFERENCES workforce_external_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_financial_connection_heads (
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
        REFERENCES workforce_financial_connections (
            tenant_id, organization_id, connection_id, version
        )
);

CREATE TABLE workforce_financial_connection_revocations (
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
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_financial_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_financial_valuations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    valuation_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    base_currency TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > observed_at),
    PRIMARY KEY (
        tenant_id, organization_id, connection_id,
        connection_version, valuation_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_financial_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_financial_valuation_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    valuation_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_hash CHAR(64) NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > observed_at),
    PRIMARY KEY (
        tenant_id, organization_id, connection_id, connection_version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id,
        connection_version, valuation_id, version
    ) REFERENCES workforce_financial_valuations (
        tenant_id, organization_id, connection_id,
        connection_version, valuation_id, version
    )
);

CREATE TABLE workforce_financial_risk_snapshots (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    snapshot_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    source_kind TEXT NOT NULL CHECK (source_kind IN ('signed_snapshot','provider_observation')),
    source_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    resource_version TEXT NOT NULL,
    total_capital_microunits BIGINT NOT NULL CHECK (total_capital_microunits >= 0),
    available_liquidity_microunits BIGINT NOT NULL CHECK (available_liquidity_microunits >= 0),
    gross_exposure_microunits BIGINT NOT NULL CHECK (gross_exposure_microunits >= 0),
    drawdown_microunits BIGINT NOT NULL CHECK (drawdown_microunits >= 0),
    runway_microunits BIGINT NOT NULL CHECK (runway_microunits >= 0),
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > observed_at),
    PRIMARY KEY (
        tenant_id, organization_id, connection_id,
        connection_version, snapshot_id, version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_financial_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_financial_risk_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    snapshot_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_hash CHAR(64) NOT NULL,
    resource_version TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > observed_at),
    PRIMARY KEY (
        tenant_id, organization_id, connection_id, connection_version
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id,
        connection_version, snapshot_id, version
    ) REFERENCES workforce_financial_risk_snapshots (
        tenant_id, organization_id, connection_id,
        connection_version, snapshot_id, version
    )
);

CREATE TABLE workforce_financial_effect_identities (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    proposal_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, connection_id,
        connection_version, operation, idempotency_key
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_financial_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_financial_reservations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    proposal_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    asset TEXT NOT NULL,
    venue TEXT NOT NULL,
    rail TEXT NOT NULL,
    counterparty TEXT NOT NULL,
    destination_hash CHAR(64) NOT NULL,
    notional_microunits BIGINT NOT NULL CHECK (notional_microunits >= 0),
    exposure_increase_microunits BIGINT NOT NULL CHECK (exposure_increase_microunits >= 0),
    maximum_loss_microunits BIGINT NOT NULL CHECK (maximum_loss_microunits >= 0),
    fee_ceiling_microunits BIGINT NOT NULL CHECK (fee_ceiling_microunits >= 0),
    state TEXT NOT NULL CHECK (
        state IN ('reserved','ambiguous','settled','failed','rejected','reversed')
    ),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    reconciled_at TIMESTAMPTZ,
    CHECK ((state IN ('reserved','ambiguous')) = (reconciled_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, reservation_id),
    UNIQUE (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key
    ) REFERENCES workforce_financial_effect_identities (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key
    )
);

CREATE TABLE workforce_financial_attempts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    attempt_kind TEXT NOT NULL CHECK (attempt_kind IN ('dispatch','probe')),
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
    FOREIGN KEY (tenant_id, organization_id, reservation_id)
        REFERENCES workforce_financial_reservations (
            tenant_id, organization_id, reservation_id
        )
);

CREATE TABLE workforce_financial_scope_freezes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    freeze_id TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    scope_kind TEXT NOT NULL CHECK (
        scope_kind IN ('organization','asset','venue','counterparty','destination')
    ),
    scope_key TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','reconciled','escalated')),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((state = 'open') = (resolved_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, freeze_id),
    UNIQUE (
        tenant_id, organization_id, reservation_id, scope_kind, scope_key
    ),
    FOREIGN KEY (tenant_id, organization_id, reservation_id)
        REFERENCES workforce_financial_reservations (
            tenant_id, organization_id, reservation_id
        )
);

CREATE TABLE workforce_financial_observations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    external_id TEXT NOT NULL,
    financial_state TEXT NOT NULL CHECK (
        financial_state IN (
            'accepted','pending','submitted','authorized','posted','settled',
            'collected','reconciled','rejected','reversed','failed','unknown'
        )
    ),
    authority TEXT NOT NULL CHECK (
        authority IN (
            'untrusted_external_data','provider_authoritative',
            'control_plane_authoritative'
        )
    ),
    reconciled BOOLEAN NOT NULL,
    economic_truth BOOLEAN NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    provider_observed_at TIMESTAMPTZ NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, observation_id),
    UNIQUE (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key, canonical_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, reservation_id)
        REFERENCES workforce_financial_reservations (
            tenant_id, organization_id, reservation_id
        )
);

CREATE TABLE workforce_financial_accounting_entries (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    entry_id TEXT NOT NULL,
    observation_id TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    account_id TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('debit','credit')),
    currency TEXT NOT NULL,
    microunits BIGINT NOT NULL CHECK (microunits > 0),
    valuation_time TIMESTAMPTZ NOT NULL,
    methodology_id TEXT NOT NULL,
    evidence_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, entry_id),
    FOREIGN KEY (tenant_id, organization_id, observation_id)
        REFERENCES workforce_financial_observations (
            tenant_id, organization_id, observation_id
        ),
    FOREIGN KEY (tenant_id, organization_id, reservation_id)
        REFERENCES workforce_financial_reservations (
            tenant_id, organization_id, reservation_id
        )
);

CREATE TABLE workforce_financial_founder_reservation_uses (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    founder_reservation_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    request_hash CHAR(64) NOT NULL,
    proposal_id TEXT NOT NULL,
    approval_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    consumed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, founder_reservation_id
    ),
    UNIQUE (
        tenant_id, organization_id, connection_id, connection_version,
        request_hash
    ),
    FOREIGN KEY (
        tenant_id, organization_id, connection_id, connection_version
    ) REFERENCES workforce_financial_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE TABLE workforce_financial_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    reservation_id TEXT,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (
        kind IN (
            'financial_ambiguity','provider_outage','limit_denial',
            'stale_valuation','stale_risk','account_confusion',
            'counterparty_substitution','destination_substitution',
            'pending_as_settled','out_of_band_change','credential_revoked',
            'reserved_action_denied','circuit_open','reconciliation_exhausted',
            'accounting_conflict'
        )
    ),
    safe_code TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','resolved','escalated')),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CHECK ((state = 'open') = (resolved_at IS NULL)),
    PRIMARY KEY (tenant_id, organization_id, incident_id)
);

CREATE TABLE workforce_financial_operation_circuits (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    connection_version BIGINT NOT NULL CHECK (connection_version > 0),
    operation TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('closed','open','half_open')),
    failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
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
    ) REFERENCES workforce_financial_connections (
        tenant_id, organization_id, connection_id, version
    )
);

CREATE INDEX workforce_financial_connection_expiry_idx
    ON workforce_financial_connection_heads (
        tenant_id, organization_id, expires_at
    ) WHERE state IN ('active','scheduled');
CREATE INDEX workforce_financial_reservation_daily_idx
    ON workforce_financial_reservations (
        tenant_id, organization_id, created_at, state
    );
CREATE INDEX workforce_financial_reservation_scope_idx
    ON workforce_financial_reservations (
        tenant_id, organization_id, asset, venue, rail,
        counterparty, initiative_id, state, created_at
    );
CREATE INDEX workforce_financial_ambiguous_idx
    ON workforce_financial_reservations (
        tenant_id, organization_id, connection_id,
        connection_version, operation, updated_at
    ) WHERE state = 'ambiguous';
CREATE INDEX workforce_financial_freeze_open_idx
    ON workforce_financial_scope_freezes (
        tenant_id, organization_id, scope_kind, scope_key, created_at
    ) WHERE state = 'open';
CREATE INDEX workforce_financial_observation_lookup_idx
    ON workforce_financial_observations (
        tenant_id, organization_id, connection_id, connection_version,
        operation, idempotency_key, provider_observed_at
    );
CREATE INDEX workforce_financial_incident_open_idx
    ON workforce_financial_incidents (
        tenant_id, organization_id, connection_id, created_at
    ) WHERE state = 'open';
CREATE INDEX workforce_financial_circuit_retry_idx
    ON workforce_financial_operation_circuits (
        tenant_id, organization_id, state, retry_at
    ) WHERE state IN ('open','half_open');

CREATE TRIGGER workforce_financial_connections_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_connections
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_financial_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_connection_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_financial_valuations_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_valuations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_financial_risk_snapshots_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_risk_snapshots
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_financial_identities_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_effect_identities
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_financial_observations_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_observations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_financial_accounting_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_accounting_entries
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_financial_founder_uses_immutable
    BEFORE UPDATE OR DELETE ON workforce_financial_founder_reservation_uses
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
