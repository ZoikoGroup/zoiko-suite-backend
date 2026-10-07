DROP INDEX IF EXISTS idx_reconciliation_certificates_run;
DROP INDEX IF EXISTS idx_reconciliation_certificates_legacy;
CREATE UNIQUE INDEX idx_reconciliation_certificates_statement
    ON reconciliation_certificates (tenant_id, bank_account_id, statement_date);
ALTER TABLE reconciliation_certificates
    DROP COLUMN IF EXISTS run_id,
    DROP COLUMN IF EXISTS population_id,
    DROP COLUMN IF EXISTS population_version;
DROP TABLE IF EXISTS reconciliation_population_lines;
DROP TABLE IF EXISTS reconciliation_populations;
DROP TABLE IF EXISTS reconciliation_policies;
DROP TABLE IF EXISTS reconciliation_runs;
