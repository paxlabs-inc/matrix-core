CREATE TABLE workforce_fence_counters (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    last_fence BIGINT NOT NULL CHECK (last_fence > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, scope_kind, scope_id),
    CHECK (scope_kind IN ('organization', 'seat', 'node'))
);

CREATE TABLE workforce_runtime_leases (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    mandate_id TEXT NOT NULL,
    mandate_version BIGINT NOT NULL CHECK (mandate_version > 0),
    policy_binding_hash CHAR(64) NOT NULL,
    fence BIGINT NOT NULL CHECK (fence > 0),
    state TEXT NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    renewed_at TIMESTAMPTZ,
    cancellation_reason TEXT,
    PRIMARY KEY (tenant_id, organization_id, lease_id),
    CHECK (state IN ('active', 'expired', 'cancelled', 'lost')),
    CHECK (expires_at > issued_at),
    CHECK ((state = 'cancelled') = (cancellation_reason IS NOT NULL))
);

CREATE UNIQUE INDEX workforce_runtime_leases_active_seat_idx
    ON workforce_runtime_leases (tenant_id, organization_id, seat_id)
    WHERE state = 'active';

CREATE UNIQUE INDEX workforce_runtime_leases_active_node_idx
    ON workforce_runtime_leases (tenant_id, organization_id, node_id)
    WHERE state = 'active';

CREATE TABLE workforce_runtime_lease_policies (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    policy_hash CHAR(64) NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, lease_id, policy_id),
    FOREIGN KEY (tenant_id, organization_id, lease_id)
        REFERENCES workforce_runtime_leases (tenant_id, organization_id, lease_id)
);

CREATE TABLE workforce_runtime_lease_incidents (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, incident_id),
    CHECK (kind IN ('stale_fence', 'expired', 'cancelled', 'policy_mismatch', 'uncertain'))
);
