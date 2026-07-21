-- 000002_add_idempotency.up.sql
--
-- correlation_id column + partial unique index so a retried
-- RaiseException call resolves to the ORIGINAL exception instead of
-- creating a duplicate exception (and duplicate blocker-flag event).
ALTER TABLE payroll_exceptions ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_exceptions_tenant_correlation
    ON payroll_exceptions (tenant_id, correlation_id)
    WHERE correlation_id != '';
