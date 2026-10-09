-- Rollback: remove the approval_journal_id column and its index.

BEGIN;

DROP INDEX IF EXISTS idx_vendor_invoices_approval_journal;
ALTER TABLE vendor_invoices
    DROP COLUMN IF EXISTS approval_journal_id;

COMMIT;