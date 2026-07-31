CREATE TABLE workforce_wake_runtime_artifacts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    artifact_kind TEXT NOT NULL,
    content_hash CHAR(64) NOT NULL,
    sealed_content BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, wake_id, artifact_kind)
);

CREATE TRIGGER workforce_wake_runtime_artifacts_immutable
    BEFORE UPDATE OR DELETE ON workforce_wake_runtime_artifacts
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_wake_runtime_artifacts_no_truncate
    BEFORE TRUNCATE ON workforce_wake_runtime_artifacts
    FOR EACH STATEMENT EXECUTE FUNCTION workforce_reject_immutable_mutation();
