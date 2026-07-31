CREATE TABLE workforce_project_brain_records (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    author_seat_id TEXT NOT NULL,
    verifier_seat_id TEXT NOT NULL,
    source_root CHAR(64) NOT NULL,
    graph_generation BIGINT NOT NULL CHECK (graph_generation > 0),
    fresh BOOLEAN NOT NULL,
    supersedes TEXT,
    corrects TEXT,
    replacement_parent TEXT GENERATED ALWAYS AS (COALESCE(supersedes, corrects)) STORED,
    canonical_hash CHAR(64) NOT NULL,
    sealed_record BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, project_id, workspace_id, record_id
    ),
    CHECK (supersedes IS NULL OR corrects IS NULL)
);

CREATE INDEX workforce_project_brain_scope_idx
    ON workforce_project_brain_records (
        tenant_id, organization_id, project_id, workspace_id, verified_at, record_id
    );

CREATE UNIQUE INDEX workforce_project_brain_single_successor_idx
    ON workforce_project_brain_records (
        tenant_id, organization_id, project_id, workspace_id, replacement_parent
    )
    WHERE replacement_parent IS NOT NULL;

CREATE TABLE workforce_project_brain_access_denials (
    event_id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    grant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    operation TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    denied_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX workforce_project_brain_denials_scope_idx
    ON workforce_project_brain_access_denials (
        tenant_id, organization_id, project_id, workspace_id, denied_at
    );

CREATE FUNCTION workforce_project_brain_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workforce project brain records are immutable';
END;
$$;

CREATE TRIGGER workforce_project_brain_no_update
BEFORE UPDATE OR DELETE ON workforce_project_brain_records
FOR EACH ROW EXECUTE FUNCTION workforce_project_brain_immutable();
