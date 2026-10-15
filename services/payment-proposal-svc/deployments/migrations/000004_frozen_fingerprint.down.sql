CREATE OR REPLACE FUNCTION reject_proposal_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_proposals rows are never deleted';
    END IF;

    IF OLD.status IN ('AUTHORIZED', 'REJECTED', 'CANCELLED') THEN
        RAISE EXCEPTION 'proposal % is in terminal status % and cannot be modified', OLD.proposal_id, OLD.status;
    END IF;

    IF OLD.status = 'FROZEN' THEN
        IF NEW.status NOT IN ('FROZEN', 'CANCELLED', 'AUTHORIZED', 'REJECTED') THEN
            RAISE EXCEPTION 'proposal % is frozen and cannot move to status %', OLD.proposal_id, NEW.status;
        END IF;
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
            OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
            OR NEW.paying_bank_account_ref IS DISTINCT FROM OLD.paying_bank_account_ref
            OR NEW.currency IS DISTINCT FROM OLD.currency
            OR NEW.payment_date IS DISTINCT FROM OLD.payment_date
            OR NEW.payment_method IS DISTINCT FROM OLD.payment_method
            OR NEW.gross_amount IS DISTINCT FROM OLD.gross_amount
            OR NEW.withholding_amount IS DISTINCT FROM OLD.withholding_amount
            OR NEW.net_amount IS DISTINCT FROM OLD.net_amount
            OR NEW.frozen_by_principal_id IS DISTINCT FROM OLD.frozen_by_principal_id
            OR NEW.frozen_at IS DISTINCT FROM OLD.frozen_at
            OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
            OR NEW.created_at IS DISTINCT FROM OLD.created_at
        THEN
            RAISE EXCEPTION 'proposal % is frozen; only status/cancel_reason may still change', OLD.proposal_id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE payment_proposals DROP COLUMN IF EXISTS frozen_fingerprint;
