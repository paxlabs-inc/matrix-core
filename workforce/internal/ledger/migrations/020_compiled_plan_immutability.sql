-- A compiled plan is the sole durable authorization for one external effect.
-- Dispatch authority must therefore be append-only: a mutable projection could
-- be rewritten to authorize a proposal the compiler never approved.
DELETE FROM workforce_compiled_plans WHERE effect_proposal_hash IS NULL;

ALTER TABLE workforce_compiled_plans
    ALTER COLUMN effect_proposal_hash SET NOT NULL;

CREATE TRIGGER workforce_compiled_plans_immutable
    BEFORE UPDATE OR DELETE ON workforce_compiled_plans
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

CREATE TRIGGER workforce_approval_batch_intents_immutable
    BEFORE UPDATE OR DELETE ON workforce_approval_batch_intents
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();

-- An approval batch legitimately mutates only its consumption counter and its
-- revocation stamp. Every column carrying owner-signed authority is frozen, so
-- a ceiling, expiry, or intent set cannot be widened after the owner signed it.
-- The two mutable columns move in one direction only: spend never decreases and
-- a revocation is never cleared or restamped, so neither can be rolled back to
-- buy further authority.
CREATE OR REPLACE FUNCTION workforce_reject_approval_authority_mutation()
RETURNS trigger AS $$
BEGIN
    IF NEW.consumed_microunits < OLD.consumed_microunits THEN
        RAISE EXCEPTION 'Workforce approval consumption cannot decrease'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.revoked_at IS NOT NULL
        AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at
    THEN
        RAISE EXCEPTION 'Workforce approval revocation cannot be reversed'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.batch_id IS DISTINCT FROM OLD.batch_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.organization_id IS DISTINCT FROM OLD.organization_id
        OR NEW.intent_set_hash IS DISTINCT FROM OLD.intent_set_hash
        OR NEW.aggregate_ceiling_microunits
            IS DISTINCT FROM OLD.aggregate_ceiling_microunits
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
        OR NEW.owner_id IS DISTINCT FROM OLD.owner_id
        OR NEW.key_id IS DISTINCT FROM OLD.key_id
        OR NEW.signature IS DISTINCT FROM OLD.signature
        OR NEW.canonical_hash IS DISTINCT FROM OLD.canonical_hash
        OR NEW.sealed_batch IS DISTINCT FROM OLD.sealed_batch
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'signed Workforce approval authority cannot be mutated'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_approval_batches_authority_immutable
    BEFORE UPDATE ON workforce_approval_batches
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_approval_authority_mutation();

-- Row triggers never see TRUNCATE, so append-only authority would otherwise be
-- erasable wholesale. This does not replace withholding DDL rights from the
-- runtime role; it removes the cheapest way to void the record.
CREATE OR REPLACE FUNCTION workforce_reject_authority_truncate()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'durable Workforce authority relation % cannot be truncated',
        TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_compiled_plans_no_truncate
    BEFORE TRUNCATE ON workforce_compiled_plans
    FOR EACH STATEMENT EXECUTE FUNCTION workforce_reject_authority_truncate();

CREATE TRIGGER workforce_approval_batches_no_truncate
    BEFORE TRUNCATE ON workforce_approval_batches
    FOR EACH STATEMENT EXECUTE FUNCTION workforce_reject_authority_truncate();

CREATE TRIGGER workforce_approval_batch_intents_no_truncate
    BEFORE TRUNCATE ON workforce_approval_batch_intents
    FOR EACH STATEMENT EXECUTE FUNCTION workforce_reject_authority_truncate();

CREATE TRIGGER workforce_approval_consumptions_no_truncate
    BEFORE TRUNCATE ON workforce_approval_batch_consumptions
    FOR EACH STATEMENT EXECUTE FUNCTION workforce_reject_authority_truncate();
