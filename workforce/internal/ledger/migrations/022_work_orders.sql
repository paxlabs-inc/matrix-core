CREATE TABLE workforce_work_orders (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    work_order_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    idempotency_key TEXT NOT NULL,
    canonical_hash CHAR(64) NOT NULL,
    sealed_order BYTEA NOT NULL,
    goal_id TEXT NOT NULL,
    wake_id TEXT NOT NULL,
    event_cursor BIGINT NOT NULL CHECK (event_cursor > 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, work_order_id),
    UNIQUE (tenant_id, organization_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, goal_id)
        REFERENCES workforce_work_nodes (
            tenant_id, organization_id, node_id
        ),
    FOREIGN KEY (tenant_id, organization_id, wake_id)
        REFERENCES workforce_scheduled_wakes (
            tenant_id, organization_id, wake_id
        )
);

CREATE TRIGGER workforce_work_orders_immutable
    BEFORE UPDATE OR DELETE ON workforce_work_orders
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
