ALTER TABLE workforce_compiled_plans
    ADD COLUMN effect_proposal_hash CHAR(64);

ALTER TABLE workforce_effect_operations
    ADD COLUMN compiled_proposal_hash CHAR(64),
    ADD COLUMN approval_id TEXT,
    ADD COLUMN approval_cost_microunits BIGINT NOT NULL DEFAULT 0
        CHECK (approval_cost_microunits >= 0);

ALTER TABLE workforce_effect_operations
    ADD CONSTRAINT workforce_effect_irreversible_approval_check CHECK (
        (irreversible AND approval_id IS NOT NULL AND approval_cost_microunits > 0)
        OR
        (NOT irreversible AND approval_id IS NULL AND approval_cost_microunits = 0)
    ) NOT VALID;
