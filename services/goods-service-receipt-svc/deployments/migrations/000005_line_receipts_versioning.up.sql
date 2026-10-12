-- Migration: 000005_line_receipts_versioning.up.sql
--
--   * version                 - optimistic-concurrency counter, incremented by every
--                               UPDATE issued by the store (expected_version).
--   * po_line_id / po_revision - line-level receipts against an AP-03 PO line; NULL
--                               for legacy header-only receipts.
--   * reversed_quantity       - cumulative quantity reversed (receipt + each reversal).
--
-- The immutability trigger is replaced so a confirmed receipt may still move
-- status/reversal totals and bump version/updated_at, but nothing else, and so a
-- line link can never be re-pointed after confirmation. DELETE stays blocked
-- unconditionally (negative path #3).

ALTER TABLE goods_service_receipts
    ADD COLUMN version           INT NOT NULL DEFAULT 1,
    ADD COLUMN po_line_id        UUID NULL,
    ADD COLUMN po_revision       INT NULL,
    ADD COLUMN reversed_quantity NUMERIC(18, 4) NOT NULL DEFAULT 0;

CREATE INDEX idx_gsr_po_line ON goods_service_receipts (tenant_id, purchase_order_id, po_line_id)
    WHERE po_line_id IS NOT NULL;

ALTER TABLE receipt_reversals
    ADD COLUMN reversed_quantity NUMERIC(18, 4) NOT NULL DEFAULT 0;

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
            OR NEW.po_line_id IS DISTINCT FROM OLD.po_line_id
            OR NEW.po_revision IS DISTINCT FROM OLD.po_revision
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
            OR NEW.confirmed_by_principal_id IS DISTINCT FROM OLD.confirmed_by_principal_id
            OR NEW.confirmed_at IS DISTINCT FROM OLD.confirmed_at
            OR NEW.created_at IS DISTINCT FROM OLD.created_at
        THEN
            RAISE EXCEPTION 'receipt % is confirmed; only status, reversal totals, version and updated_at may still change', OLD.receipt_id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
