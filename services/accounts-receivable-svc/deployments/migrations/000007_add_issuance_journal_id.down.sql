-- Rollback: remove the issuance_journal_id column and its index.

BEGIN;

DROP INDEX IF EXISTS idx_customer_invoices_issuance_journal;
ALTER TABLE customer_invoices
    DROP COLUMN IF EXISTS issuance_journal_id;

COMMIT;