-- Reverses 000006_ap05_invoice_inputs.up.sql.
--
-- Dropping vendor_invoice_lines discards the line detail, per-line tax and PO
-- linkage of every invoice recorded while it existed. `amount` survives, so the
-- total payable is preserved and what is lost is the account of what it was
-- for. Safe only where the payables ledger is disposable.

BEGIN;

DROP INDEX IF EXISTS idx_vendor_invoices_po;
DROP INDEX IF EXISTS idx_vendor_invoices_supply_date;

DROP TABLE IF EXISTS vendor_invoice_lines;

ALTER TABLE vendor_invoices
    DROP COLUMN IF EXISTS invoice_document_id,
    DROP COLUMN IF EXISTS po_vendor_profile_id,
    DROP COLUMN IF EXISTS goods_receipt_ref,
    DROP COLUMN IF EXISTS purchase_order_id,
    DROP COLUMN IF EXISTS tax_amount,
    DROP COLUMN IF EXISTS net_amount,
    DROP COLUMN IF EXISTS supply_date,
    DROP COLUMN IF EXISTS invoice_date;

COMMIT;
