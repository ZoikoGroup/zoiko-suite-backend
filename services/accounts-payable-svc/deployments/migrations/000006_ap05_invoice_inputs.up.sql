-- AP-05 Supplier Invoice required business/source inputs
-- (ZS-ARCH-SVC-001 v2.0 §9.F).
--
-- The doc requires: "Supplier; invoice number/date; supply date; due date;
-- PO/receipt refs; currency; lines; tax; invoice document." Supplier, invoice
-- number, due date and currency were already here. This migration adds the
-- rest, of which the largest is lines: the service previously held a single
-- flat `amount` and no idea what it was for.
--
-- WHY LINES MATTER HERE SPECIFICALLY
--
-- Without lines there is nothing to match a purchase order against, nothing to
-- determine tax on, and nothing to map to accounts — AP-06 Invoice Matching,
-- TAX-03 and ACC-02 all consume line detail. A flat total also cannot express
-- the ordinary case of one invoice carrying two tax treatments.

BEGIN;

ALTER TABLE vendor_invoices
    -- The date on the supplier's document, distinct from created_at (when we
    -- received it) and from due_date (when it must be paid). All three
    -- legitimately differ and each answers a different question.
    ADD COLUMN IF NOT EXISTS invoice_date DATE,

    -- The tax point: when the supply took place. Drives which tax period and
    -- which rule version apply, and is routinely in a different month from the
    -- invoice date on a supply invoiced in arrears.
    ADD COLUMN IF NOT EXISTS supply_date DATE,

    -- Money, split. `amount` keeps its existing meaning — the gross total
    -- payable — so nothing that already reads it changes. net + tax = gross is
    -- enforced by the service, which is the AP equivalent of a balance check.
    ADD COLUMN IF NOT EXISTS net_amount NUMERIC(18,2),
    ADD COLUMN IF NOT EXISTS tax_amount NUMERIC(18,2),

    -- PO / receipt references.
    --
    -- purchase_order_id is validated against purchase-order-svc when supplied:
    -- it must exist, belong to the same legal entity, and not be closed.
    -- goods_receipt_ref is carried unvalidated — AP-04 Goods/Service Receipt
    -- does not exist, so nothing can confirm a receipt happened.
    ADD COLUMN IF NOT EXISTS purchase_order_id TEXT,
    ADD COLUMN IF NOT EXISTS goods_receipt_ref TEXT,

    -- Recorded from the purchase order at intake so that a later disagreement
    -- between the PO's supplier and the invoice's is visible in the data. NOT
    -- used to refuse the invoice: purchase-order-svc keys its supplier as
    -- vendor_profile_id and this service as vendor_id, and nothing establishes
    -- that the two are the same identifier space. Refusing on an unproven
    -- equivalence would reject correct invoices; recording it makes the
    -- mismatch auditable and lets AP-06 decide when it exists.
    ADD COLUMN IF NOT EXISTS po_vendor_profile_id TEXT,

    -- The invoice document itself. Required to leave RECEIVED, not to enter it:
    -- §7 makes Draft an editable working state and INV-10 requires evidence
    -- before COMPLETION, so an invoice keyed ahead of its scan is legitimate
    -- while an invoice VALIDATED without one is the audit gap.
    ADD COLUMN IF NOT EXISTS invoice_document_id TEXT;

-- Backfill so the NOT NULLs below can be applied to rows that predate this.
-- Pre-contract invoices are identifiable by tax_amount = 0 AND no lines.
UPDATE vendor_invoices
   SET invoice_date = COALESCE(invoice_date, created_at::DATE),
       supply_date  = COALESCE(supply_date, created_at::DATE),
       net_amount   = COALESCE(net_amount, amount),
       tax_amount   = COALESCE(tax_amount, 0);

ALTER TABLE vendor_invoices
    ALTER COLUMN invoice_date SET NOT NULL,
    ALTER COLUMN supply_date  SET NOT NULL,
    ALTER COLUMN net_amount   SET NOT NULL,
    ALTER COLUMN tax_amount   SET NOT NULL;

-- AP-05 "lines" and "tax".
--
-- Append-only in practice: a supplier invoice is a document received, not a
-- draft this service authors, so a correction is a credit note or a re-keyed
-- invoice rather than an edit. No ON DELETE CASCADE — nothing deletes an
-- invoice (no soft-delete doctrine), so a cascade could only ever fire by
-- accident.
CREATE TABLE IF NOT EXISTS vendor_invoice_lines (
    invoice_line_id UUID PRIMARY KEY,
    invoice_id      UUID NOT NULL REFERENCES vendor_invoices(invoice_id),
    tenant_id       UUID NOT NULL,
    line_number     INTEGER NOT NULL,

    description  TEXT NOT NULL,
    quantity     NUMERIC(18,4) NOT NULL DEFAULT 1,
    unit_price   NUMERIC(18,4) NOT NULL DEFAULT 0,
    net_amount   NUMERIC(18,2) NOT NULL,

    -- Tax per line, because one invoice routinely carries two treatments — a
    -- standard-rated item and a zero-rated one on the same document.
    tax_code   VARCHAR(64),
    tax_amount NUMERIC(18,2) NOT NULL DEFAULT 0,

    -- The tax-determination-svc determination this line's tax came from, when
    -- one was made. Null for a line whose tax was keyed from the supplier's
    -- document without a determination being run — which is the common case
    -- today and is why this is a link, not a requirement.
    tax_determination_id TEXT,

    -- Which PO line this invoice line answers, for AP-06 when it exists.
    -- Unvalidated: purchase-order-svc exposes no line detail.
    po_line_reference TEXT,

    -- Free-form, same posture as general-ledger-svc's journal line dimensions:
    -- REF-08 Financial Dimension Registry does not exist.
    dimensions JSONB,

    UNIQUE (invoice_id, line_number)
);

CREATE INDEX IF NOT EXISTS idx_vendor_invoice_lines_invoice
    ON vendor_invoice_lines (invoice_id);

ALTER TABLE vendor_invoice_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE vendor_invoice_lines FORCE ROW LEVEL SECURITY;

-- Mirrors migrations 000004 and 000005 on vendor_invoices exactly.
--
-- NULLIF(..., '') because a custom GUC does not return to "unset" once touched:
-- after any transaction on a pooled connection has run set_config, the parameter
-- persists as '' for the rest of the session, and ''::UUID raises rather than
-- matching nothing. NULLIF collapses unset and empty to the same NULL.
--
-- WITH CHECK written out rather than left to default from USING. It is what
-- refuses an INSERT carrying another tenant's id, and relying on the implicit
-- form is how a write path comes to be overlooked.
DROP POLICY IF EXISTS tenant_isolation_policy ON vendor_invoice_lines;
CREATE POLICY tenant_isolation_policy ON vendor_invoice_lines
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE INDEX IF NOT EXISTS idx_vendor_invoices_supply_date
    ON vendor_invoices (tenant_id, supply_date);

CREATE INDEX IF NOT EXISTS idx_vendor_invoices_po
    ON vendor_invoices (tenant_id, purchase_order_id);

COMMIT;
