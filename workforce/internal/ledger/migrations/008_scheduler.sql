CREATE TABLE workforce_scheduled_wakes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    schedule_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    mandate_id TEXT NOT NULL,
    mandate_version BIGINT NOT NULL CHECK (mandate_version > 0),
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN (
        'once','recurring','event','deadline','approval','dependency',
        'correction','retry','force'
    )),
    reason TEXT NOT NULL,
    graph_scope TEXT NOT NULL,
    model_provider TEXT NOT NULL,
    model_id TEXT NOT NULL,
    mgs_reference TEXT NOT NULL,
    mgs_digest CHAR(64) NOT NULL,
    budget_tasks INTEGER NOT NULL CHECK (budget_tasks > 0),
    budget_spend_microunits BIGINT NOT NULL CHECK (budget_spend_microunits >= 0),
    idempotency_key TEXT NOT NULL,
    coalesce_key TEXT NOT NULL,
    envelope_hash CHAR(64) NOT NULL,
    sealed_envelope BYTEA NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'queued','dispatched','completed','failed','coalesced'
    )),
    scheduled_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    actual_spend_microunits BIGINT CHECK (actual_spend_microunits >= 0),
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, wake_id),
    UNIQUE (tenant_id, organization_id, idempotency_key)
);

CREATE INDEX workforce_scheduled_wakes_due_idx
    ON workforce_scheduled_wakes (
        tenant_id, organization_id, state, scheduled_at, wake_id
    );

CREATE INDEX workforce_scheduled_wakes_seat_state_idx
    ON workforce_scheduled_wakes (
        tenant_id, organization_id, seat_id, state
    );

CREATE INDEX workforce_scheduled_wakes_coalesce_idx
    ON workforce_scheduled_wakes (
        tenant_id, organization_id, seat_id, coalesce_key, state
    );

CREATE TABLE workforce_wake_events (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_kind TEXT NOT NULL CHECK (event_kind IN (
        'queued','coalesced','dispatched','completed','failed','deferred_quiet_hours',
        'deferred_concurrency','deferred_task_ceiling','deferred_spend_ceiling'
    )),
    detail TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, event_id),
    FOREIGN KEY (tenant_id, organization_id, wake_id)
        REFERENCES workforce_scheduled_wakes (
            tenant_id, organization_id, wake_id
        )
);

CREATE TRIGGER workforce_wake_events_immutable
    BEFORE UPDATE OR DELETE ON workforce_wake_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
