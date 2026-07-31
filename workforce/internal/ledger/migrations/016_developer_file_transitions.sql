CREATE TABLE workforce_developer_file_events (
    event_id BIGINT NOT NULL,
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    file_path TEXT NOT NULL,
    before_hash CHAR(64) NOT NULL,
    after_hash CHAR(64) NOT NULL,
    PRIMARY KEY (event_id, file_path),
    FOREIGN KEY (event_id)
        REFERENCES workforce_developer_scope_events (event_id),
    FOREIGN KEY (tenant_id, organization_id, lease_id)
        REFERENCES workforce_developer_change_scopes (
            tenant_id, organization_id, lease_id
        ),
    CHECK (before_hash <> after_hash)
);

CREATE INDEX workforce_developer_file_events_latest_idx
    ON workforce_developer_file_events (
        tenant_id, organization_id, lease_id, file_path, event_id DESC
    );

CREATE TRIGGER workforce_developer_file_events_no_update
BEFORE UPDATE OR DELETE ON workforce_developer_file_events
FOR EACH ROW EXECUTE FUNCTION workforce_developer_scope_immutable();
