-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE INSERT/UPDATE/DELETE triggers raising exceptions actually
-- enforce immutability here.

-- payment_proposals: DELETE is never allowed. AUTHORIZED/REJECTED/CANCELLED
-- are fully terminal. FROZEN allows exactly one further change — the
-- transition to CANCELLED (this service's only reachable post-freeze
-- command; AUTHORIZED/REJECTED belong to AP-10, which doesn't exist yet) —
-- and even then only status/cancel_reason/updated_at may move. This is the
-- literal enforcement of negative-path scenario #3: nothing about a frozen
-- proposal's amounts or composition can silently change.
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

CREATE TRIGGER trg_reject_proposal_mutation
    BEFORE UPDATE OR DELETE ON payment_proposals
    FOR EACH ROW EXECUTE FUNCTION reject_proposal_mutation();

-- proposal_items: never deleted. New items may only be inserted while the
-- parent proposal is still composable (DRAFT/CALCULATED/REVIEW) — the
-- custom SQLSTATE ZK001 lets the store distinguish this from a generic
-- failure. The ONLY update ever permitted on an existing item, at any
-- proposal status, is is_active flipping from TRUE to FALSE — both
-- RemovePayable (pre-freeze) and CancelPaymentProposal's cascade
-- (any status) use this same mechanism, which is also what frees a payable
-- for reselection under the partial unique index.
CREATE OR REPLACE FUNCTION reject_proposal_item_mutation() RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'proposal_items rows are never deleted';
    END IF;

    IF TG_OP = 'INSERT' THEN
        SELECT status INTO parent_status FROM payment_proposals WHERE proposal_id = NEW.proposal_id;
        IF parent_status NOT IN ('DRAFT', 'CALCULATED', 'REVIEW') THEN
            RAISE EXCEPTION 'proposal % is in status % and can no longer accept new items', NEW.proposal_id, parent_status
                USING ERRCODE = 'ZK001';
        END IF;
        RETURN NEW;
    END IF;

    -- TG_OP = 'UPDATE'
    IF NOT (OLD.is_active AND NOT NEW.is_active) THEN
        RAISE EXCEPTION 'proposal item % is immutable except is_active TRUE -> FALSE', OLD.item_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.proposal_id IS DISTINCT FROM OLD.proposal_id
        OR NEW.payable_source IS DISTINCT FROM OLD.payable_source
        OR NEW.payable_id IS DISTINCT FROM OLD.payable_id
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.gross_amount IS DISTINCT FROM OLD.gross_amount
        OR NEW.withholding_amount IS DISTINCT FROM OLD.withholding_amount
        OR NEW.net_amount IS DISTINCT FROM OLD.net_amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.due_date IS DISTINCT FROM OLD.due_date
        OR NEW.payee_snapshot_at IS DISTINCT FROM OLD.payee_snapshot_at
        OR NEW.tax_determination_id IS DISTINCT FROM OLD.tax_determination_id
        OR NEW.exception_ref IS DISTINCT FROM OLD.exception_ref
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'proposal item % may only have is_active flipped, nothing else', OLD.item_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_proposal_item_mutation
    BEFORE INSERT OR UPDATE OR DELETE ON proposal_items
    FOR EACH ROW EXECUTE FUNCTION reject_proposal_item_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_proposal_events_mutation
    BEFORE UPDATE OR DELETE ON proposal_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
