DROP INDEX IF EXISTS idx_payroll_runs_tenant_correlation;
ALTER TABLE payroll_runs DROP COLUMN IF EXISTS correlation_id;
