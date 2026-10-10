-- AR-05/ACC-14: Store the GL journal ID created when an invoice is issued.
-- This provides a direct audit link from the receivable to the posted accounting event.
-- The journal is posted via general-ledger-svc's /v1/postings/events with the invoice ID
-- as source_event_id (idempotent). This column records the returned journal_id.

BEGIN;

ALTER TABLE customer_invoices
    ADD COLUMN IF NOT EXISTS issuance_journal_id TEXT;

-- Index for reverse lookup: find the invoice for a given journal.
CREATE INDEX IF NOT EXISTS idx_customer_invoices_issuance_journal
    ON customer_invoices (issuance_journal_id)
    WHERE issuance_journal_id IS NOT NULL;

COMMIT;