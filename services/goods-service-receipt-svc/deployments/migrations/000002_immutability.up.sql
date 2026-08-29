-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here.

-- goods_service_receipts: DELETE is never allowed, at any status — this is
-- the direct enforcement of negative-path scenario #3 ("confirmed receipt
-- deleted to fix mismatch" must be blocked; corrections are ReverseReceipt,
-- never delete). UPDATE is allowed freely while DRAFT/PENDING_CONFIRMATION,
-- narrows to only {status, reversed_amount, confirmed_by_principal_id,
-- confirmed_at, updated_at} once CONFIRMED/PARTIALLY_REVERSED, and is
-- blocked entirely once REJECTED/FULLY_REVERSED (terminal).
CREATE OR REPLACE FUNCTION reject_receipt_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'goods_service_receipts rows are never deleted; use ReverseReceipt to correct a confirmed receipt';
    END IF;

    IF OLD.status IN ('REJECTED', 'FULLY_REVERSED') THEN
        RAISE EXCEPTION 'receipt % is in terminal status % and cannot be modified', OLD.receipt_id, OLD.status;
    END IF;

    IF OLD.status IN ('CONFIRMED', 'PARTIALLY_REVERSED') THEN
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
            OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
            OR NEW.purchase_order_id IS DISTINCT FROM OLD.purchase_order_id
            OR NEW.receipt_type IS DISTINCT FROM OLD.receipt_type
            OR NEW.quantity IS DISTINCT FROM OLD.quantity
            OR NEW.unit_of_measure IS DISTINCT FROM OLD.unit_of_measure
            OR NEW.amount IS DISTINCT FROM OLD.amount
            OR NEW.currency_code IS DISTINCT FROM OLD.currency_code
            OR NEW.receipt_date IS DISTINCT FROM OLD.receipt_date
            OR NEW.location IS DISTINCT FROM OLD.location
            OR NEW.inspection_result IS DISTINCT FROM OLD.inspection_result
            OR NEW.requires_independent_acceptance IS DISTINCT FROM OLD.requires_independent_acceptance
            OR NEW.tolerance_exception_ref IS DISTINCT FROM OLD.tolerance_exception_ref
            OR NEW.rejection_reason IS DISTINCT FROM OLD.rejection_reason
            OR NEW.receiver_principal_id IS DISTINCT FROM OLD.receiver_principal_id
            OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
            OR NEW.created_at IS DISTINCT FROM OLD.created_at
        THEN
            RAISE EXCEPTION 'receipt % is confirmed; only status, reversed_amount and confirmation fields may still change', OLD.receipt_id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_receipt_mutation
    BEFORE UPDATE OR DELETE ON goods_service_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_receipt_mutation();

-- Generic append-only guard for the three pure evidence tables.
CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_receipt_evidence_mutation
    BEFORE UPDATE OR DELETE ON receipt_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER trg_reject_receipt_reversals_mutation
    BEFORE UPDATE OR DELETE ON receipt_reversals
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER trg_reject_receipt_accounting_events_mutation
    BEFORE UPDATE OR DELETE ON receipt_accounting_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
