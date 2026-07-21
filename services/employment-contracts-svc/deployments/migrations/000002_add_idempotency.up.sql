-- 000002_add_idempotency.up.sql
--
-- correlation_id column + partial unique index so a retried
-- IssueContract call resolves to the ORIGINAL contract instead of
-- creating a duplicate contract — contract_number alone was never unique.
ALTER TABLE employment_contracts ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_contracts_tenant_correlation
    ON employment_contracts (tenant_id, correlation_id)
    WHERE correlation_id != '';
