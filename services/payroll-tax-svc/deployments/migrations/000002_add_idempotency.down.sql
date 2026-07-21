DROP INDEX IF EXISTS idx_tax_calcs_tenant_correlation;
ALTER TABLE tax_calculation_records DROP COLUMN IF EXISTS correlation_id;
