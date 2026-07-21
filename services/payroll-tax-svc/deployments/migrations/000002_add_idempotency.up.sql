-- 000002_add_idempotency.up.sql
--
-- correlation_id column + partial unique index so a retried
-- CalculateTax call resolves to the ORIGINAL calculation record instead of
-- creating a duplicate calculation (and duplicate audit entry).
ALTER TABLE tax_calculation_records ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_tax_calcs_tenant_correlation
    ON tax_calculation_records (tenant_id, correlation_id)
    WHERE correlation_id != '';
