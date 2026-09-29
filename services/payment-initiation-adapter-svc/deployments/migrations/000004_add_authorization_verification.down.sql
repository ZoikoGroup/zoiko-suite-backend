CREATE OR REPLACE FUNCTION reject_attempt_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_initiation_attempts rows are never deleted';
    END IF;

    IF OLD.status IN ('SUBMITTED', 'REJECTED_BEFORE_SUBMISSION', 'CANCELLED', 'QUARANTINED') THEN
        RAISE EXCEPTION 'attempt % is in terminal status % and cannot be modified', OLD.attempt_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
        OR NEW.authorization_fingerprint IS DISTINCT FROM OLD.authorization_fingerprint
        OR NEW.payer_account_ref IS DISTINCT FROM OLD.payer_account_ref
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.execution_date IS DISTINCT FROM OLD.execution_date
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'attempt % is prepared; its authorized fields can never change', OLD.attempt_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE payment_initiation_attempts
    DROP COLUMN IF EXISTS authorization_id,
    DROP COLUMN IF EXISTS authorization_source;
