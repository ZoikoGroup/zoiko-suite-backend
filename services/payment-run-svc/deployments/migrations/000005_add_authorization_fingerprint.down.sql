CREATE OR REPLACE FUNCTION reject_run_instruction_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run_instructions rows are never deleted';
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.authorization_id IS DISTINCT FROM OLD.authorization_id
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

ALTER TABLE run_instructions
    DROP COLUMN IF EXISTS authorization_fingerprint;
