DROP INDEX IF EXISTS idx_exceptions_tenant_correlation;
ALTER TABLE payroll_exceptions DROP COLUMN IF EXISTS correlation_id;
