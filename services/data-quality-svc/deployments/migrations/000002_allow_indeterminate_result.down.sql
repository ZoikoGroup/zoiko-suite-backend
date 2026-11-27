-- Reverting is only possible while no Indeterminate result exists: the
-- original CHECK admits 'Pass' and 'Fail' only, and dq_results is append-only,
-- so those rows cannot be rewritten. Postgres will refuse the ADD CONSTRAINT
-- below if any are present, which is the intended outcome.
DROP INDEX IF EXISTS idx_dq_results_indeterminate;
ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_indeterminate_has_no_counts;
ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_status_check;
ALTER TABLE dq_results
    ADD CONSTRAINT dq_results_status_check CHECK (status IN ('Pass', 'Fail'));
