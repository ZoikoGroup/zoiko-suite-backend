-- Reverts migration 000006. Down only for completeness: rolling this back
-- drops the lines a receivable is already accounted against, so a live
-- deployment has no business running it.

DROP POLICY IF EXISTS tenant_isolation_policy ON customer_invoice_lines;
ALTER TABLE customer_invoice_lines DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS customer_invoice_lines;

DROP INDEX IF EXISTS idx_customer_invoices_supply_date;

ALTER TABLE customer_invoices
    DROP CONSTRAINT IF EXISTS customer_invoices_net_amount_non_negative,
    DROP CONSTRAINT IF EXISTS customer_invoices_tax_amount_non_negative;

ALTER TABLE customer_invoices
    DROP COLUMN IF EXISTS invoice_date,
    DROP COLUMN IF EXISTS supply_date,
    DROP COLUMN IF EXISTS net_amount,
    DROP COLUMN IF EXISTS tax_amount,
    DROP COLUMN IF EXISTS invoice_document_id,
    DROP COLUMN IF EXISTS sales_order_id,
    DROP COLUMN IF EXISTS customer_billing_ref,
    DROP COLUMN IF EXISTS payment_date,
    DROP COLUMN IF EXISTS payment_reference;