CREATE TABLE workforce_developer_change_scopes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    task_node_id TEXT NOT NULL,
    source_root CHAR(64) NOT NULL,
    graph_generation BIGINT NOT NULL CHECK (graph_generation > 0),
    fresh BOOLEAN NOT NULL,
    coordination_plan_id TEXT,
    scope_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, lease_id),
    FOREIGN KEY (tenant_id, organization_id, lease_id)
        REFERENCES workforce_runtime_leases (tenant_id, organization_id, lease_id),
    FOREIGN KEY (tenant_id, organization_id, task_node_id)
        REFERENCES workforce_work_nodes (tenant_id, organization_id, node_id)
);

CREATE INDEX workforce_developer_scope_project_idx
    ON workforce_developer_change_scopes (
        tenant_id, organization_id, project_id, workspace_id, created_at
    );

CREATE TABLE workforce_developer_change_claims (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    claim_kind TEXT NOT NULL,
    claim_hash CHAR(64) NOT NULL,
    exclusive BOOLEAN NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, lease_id, claim_kind, claim_hash
    ),
    FOREIGN KEY (tenant_id, organization_id, lease_id)
        REFERENCES workforce_developer_change_scopes (
            tenant_id, organization_id, lease_id
        ),
    CHECK (claim_kind IN (
        'project', 'workspace', 'task', 'file', 'symbol',
        'blast_radius', 'affected_test'
    ))
);

CREATE INDEX workforce_developer_claim_conflict_idx
    ON workforce_developer_change_claims (
        tenant_id, organization_id, claim_kind, claim_hash, exclusive, lease_id
    );
