-- Migration: 000003_bank_reconciliation_source.down.sql
--
-- Manual rollback only. Restoring NOT NULL fails while any run-sourced job
-- exists (its matched_count / reconciliation_rate / source_system_b are
-- NULL). That is deliberate: the alternative is writing 0 / 0% into those
-- jobs, a measurement nobody made. Archive or remove them first.

DROP INDEX IF EXISTS idx_reconciliation_unmatched_statement_line;

ALTER TABLE reconciliation_jobs
    ALTER COLUMN source_system_b SET NOT NULL,
    ALTER COLUMN matched_count SET DEFAULT 0,
    ALTER COLUMN matched_count SET NOT NULL,
    ALTER COLUMN reconciliation_rate SET DEFAULT 0.00,
    ALTER COLUMN reconciliation_rate SET NOT NULL;

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS statement_line_id,
    DROP COLUMN IF EXISTS bank_reconciliation_run_id;

ALTER TABLE reconciliation_jobs
    DROP COLUMN IF EXISTS bank_reconciliation_run_id;
