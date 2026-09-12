-- AR-05 Customer Invoice required business/source inputs
-- (ZS-ARCH-SVC-001 v2.0 §9.F).
--
-- The doc requires for the customer side what it requires for the supplier
-- side: "Customer; invoice number/date; supply date; due date; PO/sales order
-- refs; currency; lines; tax; invoice document." Customer, invoice number,
-- due date and currency were already here. This migration adds the rest, of
-- which the largest is lines: the service previously held a single flat
-- `amount` and no idea what it was for.
--
-- WHY LINES MATTER HERE SPECIFICALLY
--
-- Without lines there is nothing to match a sales order against, nothing to
-- determine tax on, and nothing to map to accounts -- AR-06 Cash Application,
-- AR-04 Aging and IMP-01 all consume line detail. A flat total also cannot
-- express the ordinary case of one invoice carrying two tax treatments, which
-- is why tax is per line, exactly as accounts-payable-svc does it.
--
-- This migration also adds two more things the supplier side proved necessary:
--
--  1. invoice_document_id -- the AR-05 "invoice document", which the SEND
--     transition requires (the evidence gate), yet a draft issue may precede.
--  2. payment_date / payment_reference -- the cash-application payload AR-08
--     asks for: when payment was received and the customer's own reference for
--     it. Both optional; the lifecycle stamp (payment_received_*) is mandatory
--     and unchanged.

BEGIN;

ALTER TABLE customer_invoices
    -- The date on our own document, distinct from created_at (when we raised
    -- it) and from due_date (when it must be paid). All three legitimately
    -- differ and each answers a different question.
    ADD COLUMN IF NOT EXISTS invoice_date DATE,

    -- The tax point: when the supply took place. Drives which tax period and
    -- which rule version apply, and is routinely in a different month from the
    -- invoice date on a supply invoiced in arrears.
    ADD COLUMN IF NOT EXISTS supply_date DATE,

    -- Money, split. `amount` keeps its existing meaning -- the gross total
    -- receivable -- so nothing that already reads it changes. net + tax = gross
    -- is enforced by the service, which is the AR equivalent of a balance check.
    ADD COLUMN IF NOT EXISTS net_amount NUMERIC(18,2),
    ADD COLUMN IF NOT EXISTS tax_amount NUMERIC(18,2),

    -- The invoice document itself (document-vault-svc reference). Required to
    -- leave ISSUED, not to enter it: an invoice keyed ahead of its scan is an
    -- ordinary working state, while one SENT without the customer's document
    -- behind it is the audit gap -- exactly the INV-10 posture the supplier
    -- side adopted.
    ADD COLUMN IF NOT EXISTS invoice_document_id TEXT,

    -- Sales order reference. Carried unvalidated: no sales-order/order
    -- management service exists in this platform, so nothing can confirm one
    -- did take place. Recorded for AR-06 and for the customer's own audit.
    ADD COLUMN IF NOT EXISTS sales_order_id TEXT,
    ADD COLUMN IF NOT EXISTS customer_billing_ref TEXT,

    -- AR-08 cash application. When payment was received and under what
    -- customer-side reference. Optional: a payment recorded with no reference
    -- is still a payment, and the lifecycle stamp is what an audit reaches for
    -- first. These add the remedy only the customer can supply.
    ADD COLUMN IF NOT EXISTS payment_date DATE,
    ADD COLUMN IF NOT EXISTS payment_reference TEXT;

-- Backfill so the NOT NULLs below can be applied to rows that predate this.
-- Pre-contract invoices are identifiable by tax_amount = 0 AND no lines, the
-- same marker accounts-payable-svc uses.
UPDATE customer_invoices
   SET invoice_date = COALESCE(invoice_date, created_at::DATE),
       supply_date  = COALESCE(supply_date, created_at::DATE),
       net_amount   = COALESCE(net_amount, amount),
       tax_amount   = COALESCE(tax_amount, 0);

ALTER TABLE customer_invoices
    ALTER COLUMN invoice_date SET NOT NULL,
    ALTER COLUMN supply_date  SET NOT NULL,
    ALTER COLUMN net_amount   SET NOT NULL,
    ALTER COLUMN tax_amount   SET NOT NULL;

-- The invariants that are cheap to state here and would otherwise live only in
-- Go. NOT VALID deliberately, so a backlog row (if any) cannot refuse the whole
-- migration; ALTER TABLE ... VALIDATE CONSTRAINT proves the backlog clean.
ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_net_amount_non_negative
    CHECK (net_amount >= 0) NOT VALID;

ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_tax_amount_non_negative
    CHECK (tax_amount >= 0) NOT VALID;

-- AR-05 "lines" and "tax".
--
-- Append-only in practice: a customer invoice is a document issued, not a draft
-- this service authors, so a correction is a credit note or a re-keyed invoice
-- rather than an edit. No ON DELETE CASCADE -- nothing deletes an invoice.
CREATE TABLE IF NOT EXISTS customer_invoice_lines (
    invoice_line_id UUID PRIMARY KEY,
    invoice_id      UUID NOT NULL REFERENCES customer_invoices(invoice_id),
    tenant_id       UUID NOT NULL,
    line_number     INTEGER NOT NULL,

    description  TEXT NOT NULL,
    quantity     NUMERIC(18,4) NOT NULL DEFAULT 1,
    unit_price   NUMERIC(18,4) NOT NULL DEFAULT 0,
    net_amount   NUMERIC(18,2) NOT NULL,

    -- Tax per line, because one invoice routinely carries two treatments -- a
    -- standard-rated item and a zero-rated one on the same document.
    tax_code   VARCHAR(64),
    tax_amount NUMERIC(18,2) NOT NULL DEFAULT 0,

    -- Which sales order line this invoice line answers, for AR-06 when it
    -- exists. Unvalidated: no sales-order service exposes line detail.
    sales_order_line_ref TEXT,

    -- Free-form, same posture as general-ledger-svc's journal line dimensions:
    -- REF-08 Financial Dimension Registry does not exist.
    dimensions JSONB,

    UNIQUE (invoice_id, line_number)
);

CREATE INDEX IF NOT EXISTS idx_customer_invoice_lines_invoice
    ON customer_invoice_lines (invoice_id);

ALTER TABLE customer_invoice_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_invoice_lines FORCE ROW LEVEL SECURITY;

-- Mirrors migration 000005 on customer_invoices exactly.
--
-- NULLIF(..., '') because a custom GUC does not return to "unset" once touched:
-- after any transaction on a pooled connection has run set_config, the parameter
-- persists as '' for the rest of the session, and ''::UUID raises rather than
-- matching nothing. NULLIF collapses unset and empty to the same NULL.
DROP POLICY IF EXISTS tenant_isolation_policy ON customer_invoice_lines;
CREATE POLICY tenant_isolation_policy ON customer_invoice_lines
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE INDEX IF NOT EXISTS idx_customer_invoices_supply_date
    ON customer_invoices (tenant_id, supply_date);

COMMIT;