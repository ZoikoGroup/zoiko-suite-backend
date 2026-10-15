-- Restores the 000002 trigger body (see 000002_immutability.up.sql).
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

ALTER TABLE receipt_reversals DROP COLUMN IF EXISTS reversed_quantity;
DROP INDEX IF EXISTS idx_gsr_po_line;
ALTER TABLE goods_service_receipts
    DROP COLUMN IF EXISTS reversed_quantity,
    DROP COLUMN IF EXISTS po_revision,
    DROP COLUMN IF EXISTS po_line_id,
    DROP COLUMN IF EXISTS version;
