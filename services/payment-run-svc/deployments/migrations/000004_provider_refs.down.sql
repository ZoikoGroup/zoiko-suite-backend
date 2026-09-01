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
        RAISE EXCEPTION 'run instruction % may only have status/consumed_at change, nothing else', OLD.instruction_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE run_instructions
    DROP COLUMN provider_attempt_id,
    DROP COLUMN bnk07_payment_id;
