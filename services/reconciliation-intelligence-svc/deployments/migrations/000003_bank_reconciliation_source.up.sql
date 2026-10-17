-- Suggestions are now produced from bank-reconciliation-svc's own UNMATCHED
-- statement lines, read by run id, instead of transactions a caller posted.
-- Every suggestion records the authoritative record it is about.
--
-- The new columns are NULL on jobs analyzed before this migration: those
-- were built from caller-supplied transactions and have no source record to
-- point at. Nothing is backfilled, because there is nothing true to backfill.

ALTER TABLE reconciliation_jobs
    ADD COLUMN IF NOT EXISTS bank_reconciliation_run_id UUID;

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN IF NOT EXISTS bank_reconciliation_run_id UUID,
    ADD COLUMN IF NOT EXISTS statement_line_id UUID;

-- An unmatched line has no second side, and match state belongs to
-- bank-reconciliation-svc: a run-sourced job stores NULL rather than a
-- constant 0 matched / 0% rate that would read as a measurement.
ALTER TABLE reconciliation_jobs
    ALTER COLUMN source_system_b DROP NOT NULL,
    ALTER COLUMN matched_count DROP NOT NULL,
    ALTER COLUMN matched_count DROP DEFAULT,
    ALTER COLUMN reconciliation_rate DROP NOT NULL,
    ALTER COLUMN reconciliation_rate DROP DEFAULT;

-- bank-reconciliation-svc's bank_reference is VARCHAR(255).
ALTER TABLE reconciliation_unmatched_items
    ALTER COLUMN transaction_ref_a TYPE VARCHAR(255);

-- "Which suggestions exist about this statement line?"
CREATE INDEX IF NOT EXISTS idx_reconciliation_unmatched_statement_line
    ON reconciliation_unmatched_items (tenant_id, statement_line_id)
    WHERE statement_line_id IS NOT NULL;
