CREATE TABLE workforce_work_nodes (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    node_kind TEXT NOT NULL,
    owner_seat_id TEXT,
    owner_department_id TEXT,
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    base_priority INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    deadline TIMESTAMPTZ,
    contested BOOLEAN NOT NULL DEFAULT FALSE,
    cancellation_reason TEXT,
    terminal_record_id TEXT,
    version BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, organization_id, node_id),
    CHECK (node_kind IN (
        'goal', 'intent', 'delegation', 'handoff', 'artifact',
        'approval', 'terminal_outcome'
    )),
    CHECK (state IN (
        'pending', 'eligible', 'leased', 'waiting', 'completed',
        'cancelled', 'failed', 'contested'
    )),
    CHECK (base_priority BETWEEN -1000 AND 1000),
    CHECK ((state = 'cancelled') = (cancellation_reason IS NOT NULL))
);

CREATE INDEX workforce_work_nodes_ready_idx
    ON workforce_work_nodes (
        tenant_id, organization_id, state, contested, deadline, created_at, node_id
    );

CREATE TABLE workforce_work_edges (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    prerequisite_node_id TEXT NOT NULL,
    dependent_node_id TEXT NOT NULL,
    edge_kind TEXT NOT NULL,
    required_response_schema TEXT,
    expires_at TIMESTAMPTZ,
    timeout_action TEXT,
    sla_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, prerequisite_node_id,
        dependent_node_id, edge_kind
    ),
    FOREIGN KEY (tenant_id, organization_id, prerequisite_node_id)
        REFERENCES workforce_work_nodes (tenant_id, organization_id, node_id)
        ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, organization_id, dependent_node_id)
        REFERENCES workforce_work_nodes (tenant_id, organization_id, node_id)
        ON DELETE RESTRICT,
    CHECK (prerequisite_node_id <> dependent_node_id),
    CHECK (edge_kind IN (
        'dependency', 'delegation', 'handoff', 'artifact',
        'approval', 'correction'
    )),
    CHECK (timeout_action IS NULL OR timeout_action IN (
        'escalate', 'cancel', 'return_to_sender', 'safe_default'
    )),
    CHECK (
        edge_kind <> 'delegation' OR (
            required_response_schema IS NOT NULL
            AND expires_at IS NOT NULL
            AND timeout_action IS NOT NULL
            AND sla_at IS NOT NULL
        )
    )
);

CREATE INDEX workforce_work_edges_dependent_idx
    ON workforce_work_edges (
        tenant_id, organization_id, dependent_node_id, prerequisite_node_id
    );

CREATE TABLE workforce_work_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    node_ids TEXT[] NOT NULL,
    explanation TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, organization_id, incident_id),
    CHECK (kind IN ('deadlock', 'orphan', 'sla_breach', 'delegation_expired')),
    CHECK (cardinality(node_ids) > 0)
);

CREATE UNIQUE INDEX workforce_work_incident_open_idx
    ON workforce_work_incidents (
        tenant_id, organization_id, kind, node_ids
    )
    WHERE resolved_at IS NULL;
