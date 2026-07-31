CREATE TABLE workforce_owner_commands (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    command_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_kind TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    expected_version BIGINT NOT NULL CHECK (expected_version >= 0),
    resulting_version BIGINT NOT NULL CHECK (resulting_version > 0),
    change_hash CHAR(64) NOT NULL,
    change JSONB NOT NULL,
    key_id TEXT NOT NULL,
    signature TEXT NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, command_id),
    UNIQUE (
        tenant_id, organization_id, resource_kind, resource_id, resulting_version
    )
);

CREATE TABLE workforce_control_versions (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    resource_kind TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    command_id TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, resource_kind, resource_id),
    FOREIGN KEY (tenant_id, organization_id, command_id)
        REFERENCES workforce_owner_commands (
            tenant_id, organization_id, command_id
        )
);

CREATE TABLE workforce_lifecycle_events (
    cursor BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    resource_kind TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    resource_version BIGINT NOT NULL CHECK (resource_version >= 0),
    verified_completion BOOLEAN NOT NULL DEFAULT FALSE,
    receipt_id TEXT,
    fields JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, organization_id, event_id),
    CHECK (
        verified_completion = FALSE
        OR (receipt_id IS NOT NULL AND length(receipt_id) > 0)
    )
);

CREATE INDEX workforce_lifecycle_events_replay_idx
    ON workforce_lifecycle_events (
        tenant_id, organization_id, cursor
    );

CREATE TRIGGER workforce_owner_commands_immutable
    BEFORE UPDATE OR DELETE ON workforce_owner_commands
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_lifecycle_events_immutable
    BEFORE UPDATE OR DELETE ON workforce_lifecycle_events
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
