DROP INDEX IF EXISTS idx_contracts_tenant_correlation;
ALTER TABLE employment_contracts DROP COLUMN IF EXISTS correlation_id;
