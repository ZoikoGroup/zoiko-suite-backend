-- (renumbered 000007 -> 000011 when merging main into yash: yash already has 000007..000010.)
-- AP-05/ACC-14: Store the GL journal ID created when an invoice is approved.
-- This provides a direct audit link from the payable to the posted accounting event.
-- The journal is posted via general-ledger-svc's /v1/postings/events with the invoice ID
-- as source_event_id (idempotent). This column records the returned journal_id.

BEGIN;

ALTER TABLE vendor_invoices
    ADD COLUMN IF NOT EXISTS approval_journal_id TEXT;

-- Index for reverse lookup: find the invoice for a given journal.
CREATE INDEX IF NOT EXISTS idx_vendor_invoices_approval_journal
    ON vendor_invoices (approval_journal_id)
    WHERE approval_journal_id IS NOT NULL;

COMMIT;