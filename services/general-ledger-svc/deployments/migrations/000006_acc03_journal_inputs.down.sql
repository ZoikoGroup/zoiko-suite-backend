-- Reverses 000006_acc03_journal_inputs.up.sql.
--
-- Dropping these columns discards the journal type, dates, currency and
-- dimensions of every posting written while they existed. That data cannot be
-- reconstructed from what remains, so this migration is safe to run only on an
-- environment whose ledger is disposable.

DROP INDEX IF EXISTS idx_journal_headers_type;
DROP INDEX IF EXISTS idx_journal_headers_posting_date;

ALTER TABLE journal_lines
    DROP COLUMN IF EXISTS dimensions;

ALTER TABLE journal_headers
    DROP COLUMN IF EXISTS evidence_refs,
    DROP COLUMN IF EXISTS reporting_basis,
    DROP COLUMN IF EXISTS book_id,
    DROP COLUMN IF EXISTS currency_code,
    DROP COLUMN IF EXISTS posting_date,
    DROP COLUMN IF EXISTS transaction_date,
    DROP COLUMN IF EXISTS journal_type;
