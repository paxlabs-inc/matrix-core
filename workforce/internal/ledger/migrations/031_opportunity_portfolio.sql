CREATE TABLE workforce_opportunities (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    opportunity_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    canonical_identity TEXT NOT NULL,
    source_kind TEXT NOT NULL CHECK (
        source_kind IN ('founder','research','customer','market','product','sales','financial','learning')
    ),
    author_seat_id TEXT NOT NULL,
    submitted_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > submitted_at),
    PRIMARY KEY (tenant_id, organization_id, opportunity_id, version),
    UNIQUE (tenant_id, organization_id, idempotency_key)
);

CREATE TABLE workforce_opportunity_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    canonical_identity TEXT NOT NULL,
    opportunity_id TEXT NOT NULL,
    current_version BIGINT NOT NULL CHECK (current_version > 0),
    canonical_hash CHAR(64) NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('submitted','validating','funded','deferred','paused','closed')),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, canonical_identity),
    UNIQUE (tenant_id, organization_id, opportunity_id),
    FOREIGN KEY (tenant_id, organization_id, opportunity_id, current_version)
        REFERENCES workforce_opportunities (tenant_id, organization_id, opportunity_id, version)
);

CREATE TABLE workforce_portfolio_decisions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    decision_id TEXT NOT NULL,
    opportunity_id TEXT NOT NULL,
    procedure_id TEXT NOT NULL,
    procedure_version BIGINT NOT NULL CHECK (procedure_version > 0),
    decision TEXT NOT NULL CHECK (
        decision IN (
            'go','no_go','validate','defer','reject','prioritize','pause','resume',
            'reallocate','scale','pivot','maintain','terminate','escalate'
        )
    ),
    score_bps INTEGER NOT NULL CHECK (score_bps BETWEEN 0 AND 10000),
    capital_impact_microunits BIGINT NOT NULL CHECK (capital_impact_microunits >= 0),
    risk_impact_microunits BIGINT NOT NULL CHECK (risk_impact_microunits >= 0),
    next_review_at TIMESTAMPTZ NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, decision_id),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, opportunity_id)
        REFERENCES workforce_opportunity_heads (tenant_id, organization_id, opportunity_id)
);

CREATE TABLE workforce_portfolio_allocations (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    opportunity_id TEXT NOT NULL,
    decision_id TEXT NOT NULL,
    capital_microunits BIGINT NOT NULL CHECK (capital_microunits > 0),
    risk_microunits BIGINT NOT NULL CHECK (risk_microunits >= 0),
    state TEXT NOT NULL CHECK (state IN ('active','paused','closed')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id),
    UNIQUE (tenant_id, organization_id, decision_id),
    FOREIGN KEY (tenant_id, organization_id, decision_id)
        REFERENCES workforce_portfolio_decisions (tenant_id, organization_id, decision_id)
);

CREATE TABLE workforce_company_cadence_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    cadence_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    kind TEXT NOT NULL CHECK (
        kind IN (
            'opportunity_discovery','portfolio_review','capital_review','product_review',
            'commercial_review','operational_review','strategic_learning'
        )
    ),
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, cadence_id, version)
);

CREATE TABLE workforce_company_cadences (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    cadence_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    kind TEXT NOT NULL CHECK (
        kind IN (
            'opportunity_discovery','portfolio_review','capital_review','product_review',
            'commercial_review','operational_review','strategic_learning'
        )
    ),
    interval_seconds BIGINT NOT NULL CHECK (interval_seconds BETWEEN 300 AND 31536000),
    next_due_at TIMESTAMPTZ NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    state TEXT NOT NULL CHECK (state IN ('active','paused','revoked')),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, cadence_id),
    FOREIGN KEY (tenant_id, organization_id, cadence_id, version)
        REFERENCES workforce_company_cadence_records (tenant_id, organization_id, cadence_id, version)
);

CREATE TABLE workforce_portfolio_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (
        kind IN ('starvation','circular_evidence','no_evidence','initiative_limit','resource_capture','capital_limit','risk_limit')
    ),
    opportunity_id TEXT,
    detail TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open','resolved','escalated')),
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, incident_id)
);

CREATE INDEX workforce_opportunities_expiry_idx
    ON workforce_opportunities (tenant_id, organization_id, expires_at);
CREATE INDEX workforce_portfolio_decisions_review_idx
    ON workforce_portfolio_decisions (tenant_id, organization_id, next_review_at);
CREATE INDEX workforce_company_cadences_due_idx
    ON workforce_company_cadences (tenant_id, organization_id, next_due_at)
    WHERE state = 'active';

CREATE TRIGGER workforce_opportunities_immutable
    BEFORE UPDATE OR DELETE ON workforce_opportunities
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_portfolio_decisions_immutable
    BEFORE UPDATE OR DELETE ON workforce_portfolio_decisions
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
CREATE TRIGGER workforce_company_cadence_records_immutable
    BEFORE UPDATE OR DELETE ON workforce_company_cadence_records
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
