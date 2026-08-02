CREATE TABLE workforce_lifecycle_initiatives (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    opportunity_id TEXT NOT NULL,
    portfolio_id TEXT NOT NULL,
    initiative_hash CHAR(64) NOT NULL,
    sealed_initiative BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id),
    CHECK (octet_length(sealed_initiative) > 0)
);

CREATE TABLE workforce_lifecycle_decision_receipts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL,
    transition_id TEXT NOT NULL,
    receipt_hash CHAR(64) NOT NULL,
    sealed_receipt BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, receipt_id),
    UNIQUE (tenant_id, organization_id, initiative_id, transition_id),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    CHECK (octet_length(sealed_receipt) > 0)
);

CREATE TABLE workforce_lifecycle_transition_journal (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    transition_id TEXT NOT NULL,
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    decision TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash CHAR(64) NOT NULL,
    sealed_request BYTEA NOT NULL,
    checkpoint_hash CHAR(64) NOT NULL,
    sealed_checkpoint BYTEA NOT NULL,
    receipt_id TEXT NOT NULL,
    receipt_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, sequence),
    UNIQUE (tenant_id, organization_id, initiative_id, transition_id),
    UNIQUE (tenant_id, organization_id, initiative_id, idempotency_key),
    UNIQUE (tenant_id, organization_id, initiative_id, receipt_id),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, receipt_id)
        REFERENCES workforce_lifecycle_decision_receipts (
            tenant_id, organization_id, initiative_id, receipt_id
        ),
    CHECK (from_state IN (
        '', 'DISCOVER', 'SCREEN', 'VALIDATE', 'DECIDE', 'FUND', 'DESIGN',
        'BUILD', 'VERIFY', 'LAUNCH', 'ACQUIRE', 'MONETIZE', 'OPERATE',
        'MEASURE', 'SCALE', 'PIVOT', 'MAINTAIN', 'PAUSED'
    )),
    CHECK (to_state IN (
        'DISCOVER', 'SCREEN', 'VALIDATE', 'DECIDE', 'FUND', 'DESIGN',
        'BUILD', 'VERIFY', 'LAUNCH', 'ACQUIRE', 'MONETIZE', 'OPERATE',
        'MEASURE', 'SCALE', 'PIVOT', 'MAINTAIN', 'TERMINATE', 'PAUSED'
    )),
    CHECK (decision IN (
        'INITIALIZE', 'ADVANCE', 'GO', 'NO_GO', 'SCALE', 'PIVOT',
        'MAINTAIN', 'TERMINATE', 'PAUSE', 'RESUME'
    )),
    CHECK (octet_length(sealed_request) > 0),
    CHECK (octet_length(sealed_checkpoint) > 0)
);

CREATE TABLE workforce_lifecycle_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    state TEXT NOT NULL,
    resume_state TEXT,
    version BIGINT NOT NULL CHECK (version > 0),
    checkpoint_hash CHAR(64) NOT NULL,
    sealed_checkpoint BYTEA NOT NULL,
    last_transition_id TEXT NOT NULL,
    last_receipt_id TEXT NOT NULL,
    last_receipt_hash CHAR(64) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    terminated_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, initiative_id),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, version)
        REFERENCES workforce_lifecycle_transition_journal (
            tenant_id, organization_id, initiative_id, sequence
        ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, last_receipt_id)
        REFERENCES workforce_lifecycle_decision_receipts (
            tenant_id, organization_id, initiative_id, receipt_id
        ),
    CHECK (state IN (
        'DISCOVER', 'SCREEN', 'VALIDATE', 'DECIDE', 'FUND', 'DESIGN',
        'BUILD', 'VERIFY', 'LAUNCH', 'ACQUIRE', 'MONETIZE', 'OPERATE',
        'MEASURE', 'SCALE', 'PIVOT', 'MAINTAIN', 'TERMINATE', 'PAUSED'
    )),
    CHECK (resume_state IS NULL OR resume_state IN (
        'DISCOVER', 'SCREEN', 'VALIDATE', 'DECIDE', 'FUND', 'DESIGN',
        'BUILD', 'VERIFY', 'LAUNCH', 'ACQUIRE', 'MONETIZE', 'OPERATE',
        'MEASURE', 'SCALE', 'PIVOT', 'MAINTAIN'
    )),
    CHECK ((state = 'PAUSED') = (resume_state IS NOT NULL)),
    CHECK ((state = 'TERMINATE') = (terminated_at IS NOT NULL)),
    CHECK (octet_length(sealed_checkpoint) > 0)
);

CREATE INDEX workforce_lifecycle_heads_state_idx
    ON workforce_lifecycle_heads (
        tenant_id, organization_id, state, updated_at, initiative_id
    );

CREATE TABLE workforce_lifecycle_effect_heads (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    effect_id TEXT NOT NULL,
    status TEXT NOT NULL,
    expected_lifecycle_version BIGINT NOT NULL CHECK (expected_lifecycle_version > 0),
    lease_id TEXT NOT NULL,
    fence BIGINT NOT NULL CHECK (fence > 0),
    lease_expires_at TIMESTAMPTZ NOT NULL,
    external_idempotency_key TEXT NOT NULL,
    external_request_hash CHAR(64) NOT NULL,
    prepare_hash CHAR(64) NOT NULL,
    sealed_prepare BYTEA NOT NULL,
    commit_hash CHAR(64),
    sealed_commit BYTEA,
    external_receipt_id TEXT,
    external_receipt_hash CHAR(64),
    consumed_by_transition_id TEXT,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, initiative_id, effect_id),
    UNIQUE (
        tenant_id, organization_id, initiative_id, external_idempotency_key
    ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id)
        REFERENCES workforce_lifecycle_initiatives (
            tenant_id, organization_id, initiative_id
        ),
    FOREIGN KEY (
        tenant_id, organization_id, initiative_id, consumed_by_transition_id
    ) REFERENCES workforce_lifecycle_decision_receipts (
        tenant_id, organization_id, initiative_id, transition_id
    ),
    CHECK (status IN ('PREPARED', 'COMMITTED', 'CONSUMED')),
    CHECK (octet_length(sealed_prepare) > 0),
    CHECK (
        (status = 'PREPARED' AND commit_hash IS NULL AND sealed_commit IS NULL
            AND external_receipt_id IS NULL AND external_receipt_hash IS NULL
            AND consumed_by_transition_id IS NULL)
        OR
        (status = 'COMMITTED' AND commit_hash IS NOT NULL
            AND sealed_commit IS NOT NULL AND external_receipt_id IS NOT NULL
            AND external_receipt_hash IS NOT NULL
            AND consumed_by_transition_id IS NULL)
        OR
        (status = 'CONSUMED' AND commit_hash IS NOT NULL
            AND sealed_commit IS NOT NULL AND external_receipt_id IS NOT NULL
            AND external_receipt_hash IS NOT NULL
            AND consumed_by_transition_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX workforce_lifecycle_effect_external_receipt_idx
    ON workforce_lifecycle_effect_heads (
        tenant_id, organization_id, external_receipt_id
    ) WHERE external_receipt_id IS NOT NULL;

CREATE INDEX workforce_lifecycle_effect_recovery_idx
    ON workforce_lifecycle_effect_heads (
        tenant_id, organization_id, initiative_id, status, effect_id
    ) WHERE status IN ('PREPARED', 'COMMITTED');

CREATE TABLE workforce_lifecycle_effect_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    initiative_id TEXT NOT NULL,
    effect_id TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence BETWEEN 1 AND 3),
    event_kind TEXT NOT NULL,
    event_hash CHAR(64) NOT NULL,
    sealed_event BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, initiative_id, effect_id, sequence
    ),
    FOREIGN KEY (tenant_id, organization_id, initiative_id, effect_id)
        REFERENCES workforce_lifecycle_effect_heads (
            tenant_id, organization_id, initiative_id, effect_id
        ),
    CHECK (event_kind IN ('PREPARED', 'COMMITTED', 'CONSUMED')),
    CHECK (
        (sequence = 1 AND event_kind = 'PREPARED') OR
        (sequence = 2 AND event_kind = 'COMMITTED') OR
        (sequence = 3 AND event_kind = 'CONSUMED')
    ),
    CHECK (octet_length(sealed_event) > 0)
);

CREATE TRIGGER workforce_lifecycle_initiatives_immutable
    BEFORE UPDATE OR DELETE ON workforce_lifecycle_initiatives
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_lifecycle_decision_receipts_immutable
    BEFORE UPDATE OR DELETE ON workforce_lifecycle_decision_receipts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_lifecycle_transition_journal_immutable
    BEFORE UPDATE OR DELETE ON workforce_lifecycle_transition_journal
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_lifecycle_effect_events_immutable
    BEFORE UPDATE OR DELETE ON workforce_lifecycle_effect_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
