CREATE TABLE workforce_organization_departments (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    department_kind TEXT NOT NULL,
    enabled BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, department_id)
);

CREATE TABLE workforce_organization_seats (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    seat_role TEXT NOT NULL CHECK (seat_role IN ('lead','executor','auditor')),
    mandate_id TEXT NOT NULL,
    mandate_version BIGINT NOT NULL CHECK (mandate_version > 0),
    active BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, seat_id),
    FOREIGN KEY (tenant_id, organization_id, department_id)
        REFERENCES workforce_organization_departments (
            tenant_id, organization_id, department_id
        )
);

CREATE INDEX workforce_organization_seats_department_idx
    ON workforce_organization_seats (
        tenant_id, organization_id, department_id, seat_role
    );
