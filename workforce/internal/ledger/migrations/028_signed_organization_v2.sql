ALTER TABLE workforce_company_authority_records
    DROP CONSTRAINT IF EXISTS workforce_company_authority_records_authority_kind_check;

ALTER TABLE workforce_company_authority_records
    ADD CONSTRAINT workforce_company_authority_records_authority_kind_check
    CHECK (
        authority_kind IN (
            'founder_mission',
            'company_constitution',
            'capital_envelope',
            'company_issuer_policy',
            'organization_v2'
        )
    );

ALTER TABLE workforce_organization_v2_projection
    ADD COLUMN organization_v2_version BIGINT NOT NULL DEFAULT 1
    CHECK (organization_v2_version > 0);

ALTER TABLE workforce_company_authority_change_receipts
    ADD COLUMN affected_authority_lease_count BIGINT NOT NULL DEFAULT 0
        CHECK (affected_authority_lease_count >= 0),
    ADD COLUMN affected_runtime_lease_count BIGINT NOT NULL DEFAULT 0
        CHECK (affected_runtime_lease_count >= 0),
    ADD COLUMN affected_queued_wake_count BIGINT NOT NULL DEFAULT 0
        CHECK (affected_queued_wake_count >= 0),
    ADD COLUMN affected_dispatched_wake_count BIGINT NOT NULL DEFAULT 0
        CHECK (affected_dispatched_wake_count >= 0),
    ADD COLUMN affected_effect_count BIGINT NOT NULL DEFAULT 0
        CHECK (affected_effect_count >= 0);
