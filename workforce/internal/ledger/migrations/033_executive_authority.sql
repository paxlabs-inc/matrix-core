CREATE TABLE workforce_executive_policies (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_executive_compiled_authorities (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    compiled_authority_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    compiled_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    PRIMARY KEY (tenant_id, organization_id, compiled_authority_id),
    UNIQUE (tenant_id, organization_id, policy_id, policy_version),
    FOREIGN KEY (tenant_id, organization_id, policy_id, policy_version)
        REFERENCES workforce_executive_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_executive_policy_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    compiled_authority_id TEXT NOT NULL,
    compiled_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('active','revoked','expired')),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > effective_at),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
    PRIMARY KEY (tenant_id, organization_id, policy_id),
    FOREIGN KEY (tenant_id, organization_id, policy_id, version)
        REFERENCES workforce_executive_policies (tenant_id, organization_id, policy_id, version),
    FOREIGN KEY (tenant_id, organization_id, compiled_authority_id)
        REFERENCES workforce_executive_compiled_authorities (tenant_id, organization_id, compiled_authority_id)
);

CREATE TABLE workforce_executive_policy_revocations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    revocation_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    revoked_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, revocation_id),
    UNIQUE (tenant_id, organization_id, policy_id, policy_version),
    FOREIGN KEY (tenant_id, organization_id, policy_id, policy_version)
        REFERENCES workforce_executive_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_executive_decision_requests (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    clause_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK (
        action IN (
            'reject_opportunity','authorize_experiment','prioritize_initiative',
            'allocate_delegated_capital','select_product','authorize_pricing_test',
            'sequence_launch','reallocate_resources','scale','pivot','maintain',
            'pause','terminate','emergency_pause'
        )
    ),
    initiative_id TEXT NOT NULL,
    aggregation_key CHAR(64) NOT NULL,
    action_family TEXT NOT NULL CHECK (
        action_family IN (
            'capital_and_exposure','portfolio_and_product',
            'safety_and_terminal','opportunity_disposition'
        )
    ),
    capital_microunits BIGINT NOT NULL CHECK (capital_microunits >= 0),
    exposure_microunits BIGINT NOT NULL CHECK (exposure_microunits >= 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at),
    PRIMARY KEY (tenant_id, organization_id, request_id),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, policy_id, policy_version)
        REFERENCES workforce_executive_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_executive_reviews (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    review_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('economic','evidence','financial','legal','security')),
    outcome TEXT NOT NULL CHECK (outcome IN ('approve','reject','requires_human')),
    reviewer_seat_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    reviewed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > reviewed_at),
    PRIMARY KEY (tenant_id, organization_id, review_id),
    UNIQUE (tenant_id, organization_id, request_id, kind),
    UNIQUE (tenant_id, organization_id, request_id, reviewer_seat_id),
    FOREIGN KEY (tenant_id, organization_id, request_id)
        REFERENCES workforce_executive_decision_requests (tenant_id, organization_id, request_id)
);

CREATE TABLE workforce_founder_decision_requests (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    founder_request_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    reserved_kind TEXT NOT NULL CHECK (
        reserved_kind IN (
            'mission_change','constitution_change','aggregate_capital_increase',
            'debt_or_leverage','material_transfer','restricted_jurisdiction',
            'custody_or_withdrawal','irreversible_corporate_action','control_relaxation'
        )
    ),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending','approved','denied','expired','cancelled')),
    CHECK (expires_at > created_at),
    PRIMARY KEY (tenant_id, organization_id, founder_request_id),
    UNIQUE (tenant_id, organization_id, request_id),
    FOREIGN KEY (tenant_id, organization_id, request_id)
        REFERENCES workforce_executive_decision_requests (tenant_id, organization_id, request_id)
);

CREATE TABLE workforce_executive_decision_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (
        kind IN ('material_disagreement','self_approval','authority_evasion','stale_evidence')
    ),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','resolved','escalated')),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, incident_id),
    UNIQUE (tenant_id, organization_id, request_id),
    FOREIGN KEY (tenant_id, organization_id, request_id)
        REFERENCES workforce_executive_decision_requests (tenant_id, organization_id, request_id)
);

CREATE TABLE workforce_executive_decisions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    decision_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (
        outcome IN ('authorized','denied','founder_required','emergency_paused')
    ),
    action TEXT NOT NULL CHECK (
        action IN (
            'reject_opportunity','authorize_experiment','prioritize_initiative',
            'allocate_delegated_capital','select_product','authorize_pricing_test',
            'sequence_launch','reallocate_resources','scale','pivot','maintain',
            'pause','terminate','emergency_pause'
        )
    ),
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    clause_id TEXT NOT NULL,
    capital_microunits BIGINT NOT NULL CHECK (capital_microunits >= 0),
    exposure_microunits BIGINT NOT NULL CHECK (exposure_microunits >= 0),
    rolling_capital_microunits BIGINT NOT NULL CHECK (rolling_capital_microunits >= 0),
    rolling_exposure_microunits BIGINT NOT NULL CHECK (rolling_exposure_microunits >= 0),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    authorized_until TIMESTAMPTZ NOT NULL,
    next_review_at TIMESTAMPTZ NOT NULL,
    CHECK (authorized_until > created_at),
    CHECK (next_review_at > created_at),
    PRIMARY KEY (tenant_id, organization_id, decision_id),
    UNIQUE (tenant_id, organization_id, request_id),
    FOREIGN KEY (tenant_id, organization_id, request_id)
        REFERENCES workforce_executive_decision_requests (tenant_id, organization_id, request_id),
    FOREIGN KEY (tenant_id, organization_id, policy_id, policy_version)
        REFERENCES workforce_executive_policies (tenant_id, organization_id, policy_id, version)
);

CREATE TABLE workforce_executive_decision_consumptions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    consumption_id TEXT NOT NULL,
    decision_id TEXT NOT NULL,
    operation_hash CHAR(64) NOT NULL,
    effect_id TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    consumed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, consumption_id),
    UNIQUE (tenant_id, organization_id, decision_id),
    UNIQUE (tenant_id, organization_id, effect_id),
    FOREIGN KEY (tenant_id, organization_id, decision_id)
        REFERENCES workforce_executive_decisions (tenant_id, organization_id, decision_id)
);

CREATE INDEX workforce_executive_policy_expiry_idx
    ON workforce_executive_policy_heads (tenant_id, organization_id, expires_at)
    WHERE state = 'active';
CREATE INDEX workforce_executive_decision_scope_idx
    ON workforce_executive_decision_requests (
        tenant_id, organization_id, aggregation_key, created_at
    );
CREATE INDEX workforce_executive_decision_global_idx
    ON workforce_executive_decisions (tenant_id, organization_id, created_at, outcome);
CREATE INDEX workforce_founder_decision_pending_idx
    ON workforce_founder_decision_requests (tenant_id, organization_id, created_at)
    WHERE state = 'pending';
CREATE INDEX workforce_executive_incident_open_idx
    ON workforce_executive_decision_incidents (tenant_id, organization_id, created_at)
    WHERE state = 'open';

CREATE TRIGGER workforce_executive_policies_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_policies
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_executive_compiled_authorities_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_compiled_authorities
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_executive_policy_revocations_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_policy_revocations
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_executive_decision_requests_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_decision_requests
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_executive_reviews_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_reviews
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_founder_decision_requests_immutable
    BEFORE UPDATE OR DELETE ON workforce_founder_decision_requests
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_executive_decision_incidents_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_decision_incidents
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_executive_decisions_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_decisions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_executive_decision_consumptions_immutable
    BEFORE UPDATE OR DELETE ON workforce_executive_decision_consumptions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
