-- BNK-06 Wave 11a: durably records the real, AP-10-computed fingerprint
-- captured at CreateRun time, so a later resumed/retried submission to
-- Banking (submitInstructionToBanking) carries the SAME fingerprint it
-- was originally authorized against, not a value recomputed or
-- re-fetched at submission time (which could have moved on by then).
ALTER TABLE run_instructions
    ADD COLUMN authorization_fingerprint TEXT NOT NULL DEFAULT '';

-- Same "authorized field, never changes" set reject_run_instruction_mutation
-- already protects (000004_provider_refs.up.sql) — authorization_fingerprint
-- joins authorization_id/payee_ref/net_amount/currency as immutable for the
-- life of the row.
CREATE OR REPLACE FUNCTION reject_run_instruction_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run_instructions rows are never deleted';
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.authorization_id IS DISTINCT FROM OLD.authorization_id
        OR NEW.authorization_fingerprint IS DISTINCT FROM OLD.authorization_fingerprint
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.net_amount IS DISTINCT FROM OLD.net_amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'run instruction % may only have status/consumed_at/provider refs change, nothing else', OLD.instruction_id;
    END IF;

    IF OLD.provider_attempt_id <> '' AND NEW.provider_attempt_id IS DISTINCT FROM OLD.provider_attempt_id THEN
        RAISE EXCEPTION 'run instruction % provider_attempt_id is already set and cannot change', OLD.instruction_id;
    END IF;

    IF OLD.bnk07_payment_id <> '' AND NEW.bnk07_payment_id IS DISTINCT FROM OLD.bnk07_payment_id THEN
        RAISE EXCEPTION 'run instruction % bnk07_payment_id is already set and cannot change', OLD.instruction_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
